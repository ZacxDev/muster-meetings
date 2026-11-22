// The call, for the owner's page and the guest's: LiveKit's browser client
// (vendored, loaded from this origin) driving the tiles, the layout and the
// controls, drawn with muster's UI kit (static/ui.js).
//
// 🔴 Every name is rendered with textContent (or an attribute value): a
// guest's name is text a stranger typed, and is never interpreted.
//
// MeetingsCall.prejoin() is the device check before a call: the camera
// preview, the microphone's level, the device pickers and the two toggles.
// MeetingsCall.join(opts) connects and returns {leave, room}. opts:
//   url, token, ice   from the plugin's token answer ("relay": media only
//                     through TURN, the deployment's one media path)
//   media             false, or {mic, cam, micId, camId} from the pre-join
//   onStatus(text)    a line for the status area ('' clears it)
//   onGone(reason)    the call ended from LiveKit's side
//
// The tiles: one per participant's camera and one per shared screen, each a
// kit mu-tile with the name (text), and in its status row a muted
// microphone, a camera that is off, and the connection's quality (LiveKit's
// ConnectionQuality as a signal-strength icon), each an icon with an
// aria-label. A tile with no video shows the person's initials. The active
// speaker's tile carries the kit's speaking ring.
//
// The layout: a grid, or the presentation view (one tile on the stage, the
// rest in a filmstrip). It switches to presentation by itself when anyone
// starts sharing a screen, with that screen on the stage, and back to the
// grid when the last share stops; the layout button overrides it until the
// set of shared screens changes again.
(function () {
  'use strict';
  var LK = window.LivekitClient;
  var UI = window.MeetingsUI;
  var $ = function (id) { return document.getElementById(id); };

  function tileKey(identity, source) { return identity + '|' + source; }

  // QUALITY is LiveKit's ConnectionQuality → the kit's signal icon and its
  // words. "unknown" (no measurement yet) draws nothing.
  var QUALITY = {
    excellent: { icon: 'signal-4', label: 'Connection excellent' },
    good: { icon: 'signal-3', label: 'Connection good' },
    poor: { icon: 'signal-1', label: 'Connection poor' },
    lost: { icon: 'signal-0', label: 'Connection lost' }
  };

  // ---- pre-join ----

  // PENDING_MS is how long a page waits on the browser's camera and
  // microphone prompt before it lets the person join anyway (without them):
  // a prompt nobody answers must not hold Join forever.
  var PENDING_MS = 5000;

  // prejoin wires the pre-join section's preview, meter, pickers and
  // toggles. onState(state) is told "checking", "ready" (devices on),
  // "blocked" (refused or missing: the call goes on without them),
  // "pending" (the browser's prompt still unanswered after PENDING_MS: the
  // call may go on without them) or "remembered"; a page's Join waits only
  // while "checking".
  //
  // opts.remember (the owner's page only): a check finished before
  // (MeetingsPrefs) is not run again: start() goes straight to "remembered"
  // and choice() is what was kept. A guest's page never remembers.
  function prejoin(opts) {
    opts = opts || {};
    var root = opts.root, video = $('preview'), tile = $('pre-tile'), avatar = $('pre-avatar'), checking = $('pre-checking');
    var level = $('mic-level'), fill = level.querySelector('.meter__fill'), micBtn = $('pre-mic'), camBtn = $('pre-cam');
    var pickCam = $('pick-cam'), pickMic = $('pick-mic'), status = $('pre-status');
    var stream = null, micOn = true, camOn = true, ac = null, raf = 0, state = 'idle', started = false, gen = 0;
    var saved = null, pending = 0;

    function say(t) { status.textContent = t || ''; }
    function setState(s) {
      state = s;
      root.setAttribute('data-check', s);
      checking.hidden = s !== 'checking';
      micBtn.disabled = camBtn.disabled = s !== 'ready';
      if (opts.onState) opts.onState(s);
    }
    function has(kind) { return !!stream && stream.getTracks().some(function (t) { return t.kind === kind && t.readyState === 'live'; }); }
    function sync() {
      var m = micOn && has('audio'), c = camOn && has('video');
      if (stream) stream.getTracks().forEach(function (t) { t.enabled = t.kind === 'audio' ? micOn : camOn; });
      tile.setAttribute('data-video', c ? 'on' : 'off');
      micBtn.setAttribute('aria-pressed', String(!m));
      UI.setIcon(micBtn, m ? 'mic' : 'mic-off');
      camBtn.setAttribute('aria-pressed', String(!c));
      UI.setIcon(camBtn, c ? 'camera' : 'camera-off');
      var who = opts.name ? opts.name() : '';
      avatar.textContent = '';
      if (who) avatar.textContent = UI.initials(who); else avatar.appendChild(UI.icon('people'));
      if (!m) { fill.style.setProperty('--level', '0'); level.setAttribute('aria-valuenow', '0'); }
    }

    // The meter: the microphone's loudness (RMS of the waveform), drawn as
    // a bar. Its AudioContext may start suspended (no gesture yet); the
    // first click or key on the page resumes it.
    function stopMeter() {
      if (raf) cancelAnimationFrame(raf);
      raf = 0;
      if (ac) { ac.close().catch(function () {}); ac = null; }
    }
    function startMeter() {
      stopMeter();
      if (!has('audio') || !(window.AudioContext || window.webkitAudioContext)) return;
      ac = new (window.AudioContext || window.webkitAudioContext)();
      var an = ac.createAnalyser();
      an.fftSize = 512;
      ac.createMediaStreamSource(stream).connect(an);
      var buf = new Uint8Array(an.fftSize);
      (function loop() {
        an.getByteTimeDomainData(buf);
        var sum = 0;
        for (var i = 0; i < buf.length; i++) { var x = (buf[i] - 128) / 128; sum += x * x; }
        var v = micOn ? Math.min(1, Math.sqrt(sum / buf.length) * 4) : 0;
        fill.style.setProperty('--level', v.toFixed(3));
        level.setAttribute('aria-valuenow', String(Math.round(v * 100)));
        raf = requestAnimationFrame(loop);
      })();
    }
    function resume() { if (ac && ac.state === 'suspended') ac.resume().catch(function () {}); }
    document.addEventListener('pointerdown', resume, true);
    document.addEventListener('keydown', resume, true);

    function fillPicker(sel, list, current, noun) {
      sel.textContent = '';
      list.forEach(function (d, i) {
        var o = document.createElement('option');
        o.value = d.deviceId;
        o.textContent = d.label || noun + ' ' + (i + 1);
        if (d.deviceId === current) o.selected = true;
        sel.appendChild(o);
      });
      sel.disabled = list.length < 2;
    }
    function currentId(kind) {
      var t = stream && stream.getTracks().filter(function (x) { return x.kind === kind; })[0];
      return t && t.getSettings ? t.getSettings().deviceId || '' : '';
    }
    function listDevices() {
      if (!navigator.mediaDevices || !navigator.mediaDevices.enumerateDevices) return Promise.resolve();
      return navigator.mediaDevices.enumerateDevices().then(function (ds) {
        fillPicker(pickCam, ds.filter(function (d) { return d.kind === 'videoinput'; }), currentId('video'), 'Camera');
        fillPicker(pickMic, ds.filter(function (d) { return d.kind === 'audioinput'; }), currentId('audio'), 'Microphone');
      }).catch(function () {});
    }

    function release() {
      if (stream) stream.getTracks().forEach(function (t) { t.stop(); });
      stream = null;
      video.srcObject = null;
      stopMeter();
    }

    function get(c) { return navigator.mediaDevices.getUserMedia(c); }
    // open asks for both; when one kind is missing or broken (not refused),
    // it takes the other alone.
    function open(micId, camId) {
      var a = micId ? { deviceId: { exact: micId } } : true, v = camId ? { deviceId: { exact: camId } } : true;
      return get({ audio: a, video: v }).catch(function (e) {
        if (e && e.name === 'NotAllowedError') throw e;
        return get({ audio: a }).catch(function () { return get({ video: v }); });
      });
    }

    function check(micId, camId) {
      var my = ++gen;
      if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
        setState('blocked');
        say('This browser cannot use a camera here. You can still join.');
        sync();
        return Promise.resolve();
      }
      release();
      setState('checking');
      clearTimeout(pending);
      pending = setTimeout(function () {
        if (my !== gen || state !== 'checking') return;
        setState('pending');
        say('Still waiting for the browser to allow the camera and microphone. You can join without them.');
      }, PENDING_MS);
      return open(micId, camId).then(function (s) {
        if (my !== gen) { s.getTracks().forEach(function (t) { t.stop(); }); return; }
        stream = s;
        video.srcObject = s;
        say(has('video') && has('audio') ? '' : has('audio') ? 'No camera found.' : 'No microphone found.');
        setState('ready');
        sync();
        startMeter();
        return listDevices();
      }, function () {
        if (my !== gen) return;
        setState('blocked');
        say('Your camera and microphone are blocked. Allow them in the browser to be seen and heard, or join without them.');
        sync();
      });
    }

    micBtn.addEventListener('click', function () { micOn = !micOn; sync(); });
    camBtn.addEventListener('click', function () { camOn = !camOn; sync(); });
    pickCam.addEventListener('change', function () { check(pickMic.value, pickCam.value); });
    pickMic.addEventListener('change', function () { check(pickMic.value, pickCam.value); });
    if (navigator.mediaDevices && 'ondevicechange' in navigator.mediaDevices) {
      navigator.mediaDevices.addEventListener('devicechange', function () { if (started && stream) listDevices(); });
    }

    return {
      // start runs the device check (again), or, remembered, does not.
      start: function () {
        started = true;
        saved = opts.remember && window.MeetingsPrefs ? window.MeetingsPrefs.load() : null;
        if (saved) { setState('remembered'); return Promise.resolve(); }
        sync();
        return check(pickMic.value || '', pickCam.value || '');
      },
      state: function () { return state; },
      // choice is what the call should publish.
      choice: function () {
        if (state === 'remembered') return saved;
        return { mic: micOn && has('audio'), cam: camOn && has('video'), micId: currentId('audio'), camId: currentId('video') };
      },
      // keep remembers a FINISHED check (devices granted), for the next
      // time; a refused or unanswered one is asked again.
      keep: function () { if (opts.remember && state === 'ready' && window.MeetingsPrefs) window.MeetingsPrefs.save(this.choice()); },
      // stop releases the camera and the microphone.
      stop: function () { started = false; gen++; clearTimeout(pending); release(); setState('idle'); },
      refresh: sync
    };
  }

  // ---- the side panels: the lobby (owner) and the transcript ----

  // Side keeps one panel open at a time in #side, each with its toggle in
  // the control bar (aria-pressed) and a close button.
  function Side() {
    var side = $('side');
    var panels = {}; // name -> {panel, button}
    var open = '';
    var listeners = [];
    function show(name) {
      open = name;
      Object.keys(panels).forEach(function (k) {
        panels[k].panel.hidden = k !== name;
        panels[k].button.setAttribute('aria-pressed', String(k === name));
      });
      side.hidden = !name;
      listeners.forEach(function (fn) { fn(name); });
    }
    var api = {
      add: function (name, panel, button) {
        panels[name] = { panel: panel, button: button };
        button.addEventListener('click', function () { api.toggle(name, true); });
        Array.prototype.forEach.call(panel.querySelectorAll('.panel-close'), function (b) {
          b.addEventListener('click', function () { api.close(true); button.focus(); });
        });
      },
      open: function (name) { show(name); },
      toggle: function (name, byUser) { if (open === name) api.close(byUser); else show(name); },
      close: function (byUser) { var was = open; show(''); if (byUser) listeners.forEach(function (fn) { fn('', was); }); },
      isOpen: function (name) { return open === name; },
      on: function (fn) { listeners.push(fn); }
    };
    return api;
  }

  // ---- the consent badge ----

  // showBadge draws the notice in the call: the chip names what is on
  // ("Recording · Transcribing · AI"), its popover the notice's words. The
  // chip hides when nothing is on, except on the owner's page (opts.owner),
  // where it holds the two switches and reads "Not recorded".
  var consentWired = false;
  function showBadge(notice, opts) {
    var btn = $('consent-btn'), b = $('badge');
    if (!btn || !b) return;
    if (!consentWired) { consentWired = true; UI.popover(btn, $('consent-pop')); }
    var text = notice && notice.badge ? notice.badge : '';
    b.textContent = text;
    var off = $('badge-off');
    if (off) off.hidden = !!text;
    btn.hidden = !text && !(opts && opts.owner);
    btn.classList.toggle('mu-chip--error', !!text);
    var box = $('consent-text');
    if (box) {
      box.textContent = '';
      ((notice && notice.text) || []).forEach(function (t) {
        var p = document.createElement('p');
        p.textContent = t;
        box.appendChild(p);
      });
    }
  }

  // usable keeps a remembered choice's devices only where they still
  // exist (a camera unplugged since, or no permission to list them): the
  // call then takes the browser's default instead of failing on a stale id.
  function usable(media) {
    if (!media || !navigator.mediaDevices || !navigator.mediaDevices.enumerateDevices) return Promise.resolve(media);
    return navigator.mediaDevices.enumerateDevices().then(function (ds) {
      var ids = ds.map(function (d) { return d.deviceId; }).filter(Boolean);
      return { mic: media.mic, cam: media.cam,
        micId: ids.indexOf(media.micId) >= 0 ? media.micId : '', camId: ids.indexOf(media.camId) >= 0 ? media.camId : '' };
    }, function () { return { mic: media.mic, cam: media.cam, micId: '', camId: '' }; });
  }

  // ---- in-call settings: the camera and microphone pickers ----
  var settings = null; // {room, onDevice}
  var settingsWired = false;
  function fillSettings() {
    var cam = $('set-cam'), mic = $('set-mic');
    if (!cam || !navigator.mediaDevices || !navigator.mediaDevices.enumerateDevices) return;
    navigator.mediaDevices.enumerateDevices().then(function (ds) {
      [[cam, 'videoinput', 'Camera'], [mic, 'audioinput', 'Microphone']].forEach(function (x) {
        var sel = x[0], cur = settings && settings.room.getActiveDevice ? settings.room.getActiveDevice(x[1]) : '';
        sel.textContent = '';
        ds.filter(function (d) { return d.kind === x[1]; }).forEach(function (d, i) {
          var o = document.createElement('option');
          o.value = d.deviceId;
          o.textContent = d.label || x[2] + ' ' + (i + 1);
          if (d.deviceId === cur) o.selected = true;
          sel.appendChild(o);
        });
        sel.disabled = sel.options.length < 2;
      });
    }).catch(function () {});
  }
  function wireSettings() {
    if (settingsWired || !$('btn-settings')) return;
    settingsWired = true;
    UI.popover($('btn-settings'), $('settings-pop'));
    $('btn-settings').addEventListener('click', fillSettings);
    [['set-cam', 'videoinput', 'camId'], ['set-mic', 'audioinput', 'micId']].forEach(function (x) {
      $(x[0]).addEventListener('change', function () {
        if (!settings) return;
        var id = this.value, cb = settings.onDevice;
        settings.room.switchActiveDevice(x[1], id).then(function () { if (cb) cb(x[2], id); },
          function () { UI.toast('That device could not be used.', { kind: 'warning' }); });
      });
    });
  }

  // ---- the call ----

  function join(opts) {
    var stage = $('stage'), strip = $('strip'), audio = $('audio'), view = $('call-view');
    var tiles = {}; // key -> tile
    var order = []; // keys in arrival order
    var speaking = {}; // identity -> true
    var featuredSpeaker = '';
    var manual = null, shareSet = '';
    var say = opts.onStatus || function () {};
    wireSettings();
    if (!LK) {
      say('The call could not load. Reload the page.');
      return Promise.reject(new Error('livekit-client is missing'));
    }
    var media = opts.media || null;
    var roomOpts = { adaptiveStream: true, dynacast: true };
    if (media && media.micId) roomOpts.audioCaptureDefaults = { deviceId: media.micId };
    if (media && media.camId) roomOpts.videoCaptureDefaults = { deviceId: media.camId };
    var room = new LK.Room(roomOpts);

    function lp() { return room.localParticipant; }
    function isLocal(p) { return !!p && p === lp(); }

    function ind(st, key) {
      var s = document.createElement('span');
      s.className = 'ind';
      s.setAttribute('role', 'img');
      s.setAttribute('data-ind', key);
      s.hidden = true;
      st.appendChild(s);
      return s;
    }
    function setInd(s, show, iconName, label) {
      s.hidden = !show;
      if (!show) return;
      if (s.getAttribute('data-icon') !== iconName) {
        s.textContent = '';
        s.appendChild(UI.icon(iconName));
        s.setAttribute('data-icon', iconName);
      }
      s.setAttribute('aria-label', label);
    }

    function nameOf(p) { return (p && (p.name || p.identity)) || ''; }

    function tile(p, source, local) {
      var k = tileKey(p.identity, source);
      var t = tiles[k];
      if (t) return t;
      var el = document.createElement('figure');
      el.className = 'mu-tile tile' + (local ? ' local' : '');
      el.setAttribute('data-identity', p.identity);
      el.setAttribute('data-source', source);
      el.setAttribute('data-video', 'off');
      var av = document.createElement('span');
      av.className = 'mu-avatar mu-avatar--lg';
      av.setAttribute('aria-hidden', 'true');
      var cap = document.createElement('figcaption');
      cap.className = 'mu-tile__label';
      var st = document.createElement('div');
      st.className = 'mu-tile__status';
      el.appendChild(av); el.appendChild(cap); el.appendChild(st);
      t = tiles[k] = { el: el, av: av, cap: cap, st: st, identity: p.identity, source: source, local: local, p: p, video: null };
      if (source === 'camera') {
        t.mic = ind(st, 'mic'); t.cam = ind(st, 'cam'); t.q = ind(st, 'quality');
      } else if (local) {
        // Your own shared screen is not drawn back to you (a mirror of a
        // mirror): the stage says you are presenting, with Stop.
        var pr = document.createElement('div');
        pr.className = 'presenting';
        pr.appendChild(UI.icon('screen-share'));
        var msg = document.createElement('p');
        msg.textContent = 'You are presenting';
        var stop = document.createElement('button');
        stop.type = 'button';
        stop.className = 'mu-btn mu-btn--secondary mu-btn--sm stop-share';
        stop.appendChild(UI.icon('screen-share-stop'));
        stop.appendChild(document.createTextNode('Stop presenting'));
        stop.addEventListener('click', function () { shareBtn.click(); });
        pr.appendChild(msg); pr.appendChild(stop);
        el.appendChild(pr);
      }
      order.push(k);
      draw(t);
      return t;
    }

    function dropTile(k) {
      var t = tiles[k];
      if (!t) return;
      delete tiles[k];
      order = order.filter(function (x) { return x !== k; });
      UI.remove(t.el);
    }

    // draw puts a tile's state on screen from its participant: the name,
    // the indicators, the video or the initials, the speaking ring.
    function draw(t) {
      var p = t.p, name = nameOf(p);
      t.cap.textContent = name + (t.source === 'screen_share' ? ' · screen' : '') + (t.local ? ' (you)' : '');
      t.av.textContent = UI.initials(name);
      if (t.source === 'camera') {
        var mic = !!p.isMicrophoneEnabled, cam = !!p.isCameraEnabled;
        var q = QUALITY[p.connectionQuality];
        t.el.setAttribute('data-mic', mic ? 'on' : 'off');
        t.el.setAttribute('data-cam', cam ? 'on' : 'off');
        t.el.setAttribute('data-quality', q ? p.connectionQuality : 'unknown');
        setInd(t.mic, !mic, 'mic-off', 'Microphone off');
        setInd(t.cam, !cam, 'camera-off', 'Camera off');
        setInd(t.q, !!q, q ? q.icon : '', q ? q.label : '');
        if (q) t.q.setAttribute('data-q', p.connectionQuality);
        t.st.hidden = t.mic.hidden && t.cam.hidden && t.q.hidden;
        t.el.setAttribute('data-video', cam && t.video ? 'on' : 'off');
        var sp = !!speaking[p.identity];
        t.el.classList.toggle('mu-tile--speaking', sp);
        t.el.setAttribute('data-speaking', String(sp));
      } else {
        t.st.hidden = true;
        t.el.setAttribute('data-video', t.video && !t.local ? 'on' : 'off');
      }
    }

    function drawParticipant(p) {
      var t = tiles[tileKey(p.identity, 'camera')];
      if (t) { t.p = p; draw(t); }
    }

    function setVideo(t, track) {
      if (t.video) { t.video.remove(); t.video = null; }
      if (track) {
        var v = track.attach();
        v.setAttribute('playsinline', '');
        v.muted = true;
        if (t.local && t.source === 'camera') v.className = 'mirror';
        t.el.insertBefore(v, t.el.firstChild);
        t.video = v;
      }
      draw(t);
    }

    // ---- the layout ----

    // place puts els in container, in that order, moving only a tile whose
    // place differs: re-inserting a tile restarts its entry animation (and
    // reloads nothing else), so a tile that stays put is left alone. A tile
    // leaving (its fade-out) keeps its place and is not counted; a tile that
    // belongs elsewhere is moved there by the other container's place.
    function place(container, els) {
      els.forEach(function (want, i) {
        var cur = Array.prototype.filter.call(container.children, function (c) { return !c.classList.contains('leaving'); });
        if (cur[i] !== want) container.insertBefore(want, cur[i] || null);
      });
    }
    var layoutBtn = $('btn-layout');
    function shares() { return order.filter(function (k) { return tiles[k].source === 'screen_share'; }); }
    function mode() {
      var s = shares().join(',');
      if (s !== shareSet) { shareSet = s; manual = null; }
      return manual || (s ? 'presentation' : 'grid');
    }
    function layout() {
      var m = mode();
      var keys = order.slice();
      var screens = shares();
      var featured = null;
      if (m === 'presentation') {
        featured = screens[0] ||
          (featuredSpeaker && tiles[tileKey(featuredSpeaker, 'camera')] ? tileKey(featuredSpeaker, 'camera') : null) ||
          keys.filter(function (k) { return !tiles[k].local; })[0] || keys[0] || null;
      }
      var els = keys.map(function (k) { return tiles[k].el; });
      UI.flip(els, function () {
        view.setAttribute('data-layout', m);
        var el = function (k) { return tiles[k].el; };
        if (m === 'grid') {
          // Screens first, then the people in the order they came.
          place(stage, screens.concat(keys.filter(function (k) { return screens.indexOf(k) < 0; })).map(el));
          place(strip, []);
        } else {
          place(stage, featured ? [el(featured)] : []);
          place(strip, keys.filter(function (k) { return k !== featured; }).map(el));
        }
        Array.prototype.forEach.call(stage.querySelectorAll('.tile'), function (el) { el.classList.toggle('featured', m === 'presentation'); });
      });
      strip.hidden = m !== 'presentation' || strip.querySelectorAll('.tile:not(.leaving)').length === 0;
      if (opts.onRoster) opts.onRoster(keys.filter(function (k) { return !tiles[k].local && tiles[k].source === 'camera'; }).length);
      layoutBtn.setAttribute('aria-pressed', String(m === 'presentation'));
      UI.setIcon(layoutBtn, m === 'presentation' ? 'fullscreen-exit' : 'fullscreen');
    }
    function onLayout() { manual = mode() === 'presentation' ? 'grid' : 'presentation'; layout(); }

    function addParticipant(p, local) {
      tile(p, 'camera', local);
      layout();
    }

    room.on(LK.RoomEvent.ParticipantConnected, function (p) { addParticipant(p, false); });
    room.on(LK.RoomEvent.ParticipantDisconnected, function (p) {
      Object.keys(tiles).forEach(function (k) { if (tiles[k].identity === p.identity) dropTile(k); });
      delete speaking[p.identity];
      if (featuredSpeaker === p.identity) featuredSpeaker = '';
      layout();
    });
    room.on(LK.RoomEvent.TrackSubscribed, function (track, pub, p) {
      if (track.kind === 'audio') { audio.appendChild(track.attach()); return; }
      setVideo(tile(p, track.source, false), track);
      drawParticipant(p);
      layout();
    });
    room.on(LK.RoomEvent.TrackUnsubscribed, function (track, pub, p) {
      track.detach().forEach(function (el) { el.remove(); });
      var k = tileKey(p.identity, track.source);
      if (track.source === 'screen_share') dropTile(k);
      else if (tiles[k]) setVideo(tiles[k], null);
      drawParticipant(p);
      layout();
    });
    // A remote mute, unmute, publish or unpublish: redraw that person's
    // tile from their participant (isMicrophoneEnabled, isCameraEnabled).
    [LK.RoomEvent.TrackMuted, LK.RoomEvent.TrackUnmuted, LK.RoomEvent.TrackPublished, LK.RoomEvent.TrackUnpublished].forEach(function (ev) {
      room.on(ev, function (pub, p) { if (p) drawParticipant(p); if (isLocal(p)) syncButtons(); });
    });
    room.on(LK.RoomEvent.ConnectionQualityChanged, function (q, p) { if (p) drawParticipant(p); });
    room.on(LK.RoomEvent.ParticipantNameChanged, function (n, p) { if (p) Object.keys(tiles).forEach(function (k) { if (tiles[k].identity === p.identity) draw(tiles[k]); }); });
    room.on(LK.RoomEvent.LocalTrackPublished, function (pub) {
      var track = pub.track;
      if (track && track.kind === 'video') setVideo(tile(lp(), track.source, true), track);
      syncButtons();
      layout();
    });
    room.on(LK.RoomEvent.LocalTrackUnpublished, function (pub) {
      // Also the browser's own "Stop sharing": LiveKit unpublishes the
      // screen when its track ends.
      if (pub.source === 'screen_share') dropTile(tileKey(lp().identity, 'screen_share'));
      else if (pub.source === 'camera' && tiles[tileKey(lp().identity, 'camera')]) setVideo(tiles[tileKey(lp().identity, 'camera')], null);
      syncButtons();
      layout();
    });
    room.on(LK.RoomEvent.ActiveSpeakersChanged, function (speakers) {
      speaking = {};
      (speakers || []).forEach(function (p) { speaking[p.identity] = true; });
      Object.keys(tiles).forEach(function (k) { if (tiles[k].source === 'camera') draw(tiles[k]); });
      var s = (speakers || []).filter(function (p) { return !isLocal(p); })[0];
      if (s && s.identity !== featuredSpeaker) {
        featuredSpeaker = s.identity;
        if (mode() === 'presentation' && !shares().length) layout();
      }
    });
    room.on(LK.RoomEvent.Reconnecting, function () { say('Reconnecting…'); });
    room.on(LK.RoomEvent.Reconnected, function () { say(''); });
    room.on(LK.RoomEvent.Disconnected, function (reason) {
      unbind();
      clear();
      if (opts.onGone) opts.onGone(reason);
    });

    function clear() {
      Object.keys(tiles).forEach(function (k) { tiles[k].el.remove(); });
      tiles = {};
      order = [];
      speaking = {};
      audio.textContent = '';
      var pres = $('presenting');
      if (pres) pres.hidden = true;
    }

    // ---- the controls ----
    var mic = $('btn-mic'), cam = $('btn-cam'), shareBtn = $('btn-share');
    function syncButtons() {
      var l = lp();
      var m = l.isMicrophoneEnabled, c = l.isCameraEnabled, s = l.isScreenShareEnabled;
      mic.setAttribute('aria-pressed', String(!m));
      UI.setIcon(mic, m ? 'mic' : 'mic-off');
      cam.setAttribute('aria-pressed', String(!c));
      UI.setIcon(cam, c ? 'camera' : 'camera-off');
      shareBtn.setAttribute('aria-pressed', String(!!s));
      UI.setIcon(shareBtn, s ? 'screen-share-stop' : 'screen-share');
      var pres = $('presenting');
      if (pres) pres.hidden = !s;
      drawParticipant(l);
    }
    var busy = false;
    function toggle(fn) {
      return function () {
        if (busy) return;
        busy = true;
        fn().then(function () { busy = false; syncButtons(); }, function (e) {
          busy = false;
          // Closing the browser's share picker is a choice, not an error.
          if (!(e && e.name === 'NotAllowedError')) say('That did not work: ' + (e && e.message ? e.message : 'refused'));
          syncButtons();
        });
      };
    }
    var onMic = toggle(function () { return lp().setMicrophoneEnabled(!lp().isMicrophoneEnabled); });
    var onCam = toggle(function () { return lp().setCameraEnabled(!lp().isCameraEnabled); });
    var onShare = toggle(function () { return lp().setScreenShareEnabled(!lp().isScreenShareEnabled, { audio: true }); });
    // No getDisplayMedia (a phone's browser): no Share button at all.
    shareBtn.hidden = !(navigator.mediaDevices && navigator.mediaDevices.getDisplayMedia);

    // The shortcuts: M, V and S (each control's tooltip names its key),
    // while the call is on screen, never while typing in a field or with a
    // modifier held.
    function onKey(e) {
      if (e.defaultPrevented || e.ctrlKey || e.metaKey || e.altKey || view.hidden) return;
      var t = e.target;
      if (t && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName))) return;
      var k = (e.key || '').toLowerCase();
      var b = k === 'm' ? mic : k === 'v' ? cam : k === 's' && !shareBtn.hidden ? shareBtn : null;
      if (!b) return;
      e.preventDefault();
      b.click();
    }

    mic.addEventListener('click', onMic);
    cam.addEventListener('click', onCam);
    shareBtn.addEventListener('click', onShare);
    layoutBtn.addEventListener('click', onLayout);
    document.addEventListener('keydown', onKey);
    var bound = true;
    function unbind() {
      if (!bound) return;
      bound = false;
      mic.removeEventListener('click', onMic);
      cam.removeEventListener('click', onCam);
      shareBtn.removeEventListener('click', onShare);
      layoutBtn.removeEventListener('click', onLayout);
      document.removeEventListener('keydown', onKey);
      settings = null;
    }

    function leave() {
      unbind();
      settings = null;
      opts.onGone = null;
      clear();
      return room.disconnect();
    }

    say('Connecting…');
    return room.connect(opts.url, opts.token, { rtcConfig: { iceTransportPolicy: opts.ice === 'all' ? 'all' : 'relay' } })
      .then(function () {
        say('');
        settings = { room: room, onDevice: opts.onDevice };
        addParticipant(lp(), true);
        room.remoteParticipants.forEach(function (p) {
          addParticipant(p, false);
          drawParticipant(p);
        });
        if (media && (media.mic || media.cam)) {
          return Promise.all([
            media.mic ? lp().setMicrophoneEnabled(true) : null,
            media.cam ? lp().setCameraEnabled(true) : null
          ]).catch(function () {
            // A remembered device gone or permission now refused: the call
            // goes on with what works, and says so.
            say('Your camera or microphone is off: allow them in the browser to be seen and heard.');
            UI.toast('Your camera or microphone could not start. You are in the call without it.', { kind: 'warning' });
          });
        }
      })
      .then(function () { syncButtons(); layout(); return { leave: leave, room: room }; }, function (e) {
        unbind();
        clear();
        say("Can't connect media. Check your connection and try again.");
        throw e;
      });
  }

  // readEvents reads a text/event-stream from a fetch Response, calling
  // onEvent(name, data) per event. It resolves when the stream ends.
  function readEvents(res, onEvent) {
    var reader = res.body.getReader();
    var dec = new TextDecoder();
    var buf = '';
    function pump() {
      return reader.read().then(function (r) {
        if (r.done) return;
        buf += dec.decode(r.value, { stream: true });
        var i;
        while ((i = buf.indexOf('\n\n')) >= 0) {
          var block = buf.slice(0, i);
          buf = buf.slice(i + 2);
          var name = 'message', data = '';
          block.split('\n').forEach(function (line) {
            if (line.indexOf('event: ') === 0) name = line.slice(7);
            else if (line.indexOf('data: ') === 0) data += line.slice(6);
          });
          if (data) {
            var v;
            try { v = JSON.parse(data); } catch (e) { continue; }
            onEvent(name, v);
          }
        }
        return pump();
      });
    }
    return pump();
  }

  // replacedElsewhere says a disconnect reason is LiveKit's "the same
  // identity joined again": the owner opened the meeting on another device,
  // and the newer join won.
  function replacedElsewhere(reason) {
    return !!LK && reason === LK.DisconnectReason.DUPLICATE_IDENTITY;
  }

  // clock is a meeting time in ms as m:ss (h:mm:ss past an hour).
  function clock(ms) {
    var t = Math.max(0, Math.floor(ms / 1000));
    var h = Math.floor(t / 3600), m = Math.floor(t / 60) % 60, s = t % 60;
    var ss = (s < 10 ? '0' : '') + s;
    return h > 0 ? h + ':' + (m < 10 ? '0' : '') + m + ':' + ss : m + ':' + ss;
  }

  // lineEl draws one segment: a line (when, who, what was said) or a gap
  // marker. 🔴 Every part is textContent: the words are what a participant
  // SAID (a guest is a stranger) and the name is what they typed. Nothing
  // here is ever parsed as HTML.
  function lineEl(g) {
    var li = document.createElement('li');
    li.setAttribute('data-id', String(g.id));
    var when = document.createElement('span');
    when.className = 'when';
    when.textContent = clock(g.start_ms);
    li.appendChild(when);
    if (g.kind === 'gap') {
      li.className = 'gap';
      var what = document.createElement('span');
      what.className = 'text';
      what.textContent = g.speaker + ': not transcribed' + (g.end_ms == null ? '…' : ' until ' + clock(g.end_ms));
      li.appendChild(what);
      return li;
    }
    li.className = 'line';
    var who = document.createElement('span');
    who.className = 'who';
    who.textContent = g.speaker;
    var text = document.createElement('span');
    text.className = 'text';
    text.textContent = g.text;
    li.appendChild(who); li.appendChild(text);
    return li;
  }

  // Transcript is the live transcript panel over a list: segment events add
  // or (a gap that closed) replace a line, partials (live captions) are the
  // lines in progress at the end, and the status says when the transcript is
  // paused. ui.reveal() opens the panel with its first line, unless the
  // person closed it; ui.unseen() marks new lines while it is closed.
  function Transcript(list, statusEl, ui) {
    ui = ui || {};
    var partials = [];
    function reveal() { if (ui.reveal) ui.reveal(); }
    function unseen() { if (ui.unseen) ui.unseen(); }
    function follow(fn) {
      var atEnd = list.scrollHeight - list.scrollTop - list.clientHeight < 40;
      fn();
      if (atEnd) list.scrollTop = list.scrollHeight;
    }
    return {
      segment: function (g) {
        follow(function () {
          var el = lineEl(g);
          var old = list.querySelector('li[data-id="' + String(Number(g.id)) + '"]');
          if (old) { list.replaceChild(el, old); return; }
          var first = partials.length ? partials[0] : null;
          list.insertBefore(el, first);
        });
        reveal();
        unseen();
      },
      // amend is a line already shown whose text changed (its closing mark
      // came with the speaker's next words): the text is replaced in place,
      // the element kept, so it neither moves nor plays its entry again. As
      // textContent, like every line. A line not shown yet changes nothing:
      // its segment event will carry the new text.
      amend: function (a) {
        var li = list.querySelector('li.line[data-id="' + String(Number(a.id)) + '"]');
        var text = li && li.querySelector('.text');
        if (text) text.textContent = a.text;
      },
      partials: function (ps) {
        follow(function () {
          // A caption that is still the same line keeps its element (no
          // re-entry animation while its words grow).
          var next = (ps || []).map(function (p, i) {
            var li = partials[i];
            if (!li || li.getAttribute('data-speaker') !== p.speaker) {
              li = lineEl({ id: 'p', kind: 'text', start_ms: p.start_ms, speaker: p.speaker, text: '' });
              li.className = 'line partial';
              li.removeAttribute('data-id');
              li.setAttribute('data-speaker', p.speaker);
            }
            li.querySelector('.when').textContent = clock(p.start_ms);
            li.querySelector('.text').textContent = p.text + '…';
            list.appendChild(li);
            return li;
          });
          partials.forEach(function (el) { if (next.indexOf(el) < 0) el.remove(); });
          partials = next;
        });
        if (partials.length) { reveal(); unseen(); }
      },
      status: function (s) {
        if (statusEl) { statusEl.textContent = s || ''; statusEl.hidden = !s; }
        if (s) reveal();
      },
      reset: function () {
        list.textContent = '';
        partials = [];
        if (statusEl) { statusEl.textContent = ''; statusEl.hidden = true; }
        if (ui.hide) ui.hide();
      }
    };
  }

  // transcriptPanel joins a Transcript to the side panel and its button:
  // the panel opens itself with the first line until the person closes it;
  // after that a dot on the button says there is something new.
  // busy() (the owner's page) says the people panel is in use (someone is
  // waiting): the transcript does not take its place then.
  function transcriptPanel(side, busy) {
    var btn = $('btn-transcript'), dot = $('transcript-new'), closed = false;
    side.add('transcript', $('panels'), btn);
    side.on(function (open, was) {
      if (open === 'transcript') dot.hidden = true;
      if (open === '' && was === 'transcript') closed = true;
    });
    return Transcript($('transcript'), $('transcript-status'), {
      reveal: function () { if (!closed && !side.isOpen('transcript') && !(side.isOpen('people') && busy && busy())) side.open('transcript'); },
      unseen: function () { if (!side.isOpen('transcript')) dot.hidden = false; },
      hide: function () { closed = false; dot.hidden = true; if (side.isOpen('transcript')) side.close(false); }
    });
  }

  UI.decorate(document);

  window.MeetingsCall = { prejoin: prejoin, join: join, usable: usable, PENDING_MS: PENDING_MS, readEvents: readEvents, showBadge: showBadge, replacedElsewhere: replacedElsewhere,
    Transcript: Transcript, transcriptPanel: transcriptPanel, Side: Side, lineEl: lineEl, clock: clock };
})();
