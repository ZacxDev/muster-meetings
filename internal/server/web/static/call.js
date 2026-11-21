// The call, for the owner's page and the guest's: LiveKit's browser client
// (vendored, loaded from this origin) driving the tiles and the controls.
// Every name is rendered with textContent: a guest's name is text a stranger
// typed, and is never interpreted.
//
// MeetingsCall.join(opts) connects and returns {leave, room}. opts:
//   url, token, ice   from the plugin's token answer ("relay": media only
//                     through TURN, the deployment's one media path)
//   media             true: publish the camera and the microphone
//   onStatus(text)    a line for the status area ('' clears it)
//   onGone(reason)    the call ended from LiveKit's side
(function () {
  'use strict';
  var LK = window.LivekitClient;
  var $ = function (id) { return document.getElementById(id); };

  function tileKey(identity, source) { return identity + '|' + source; }

  function join(opts) {
    var stage = $('stage'), strip = $('strip'), audio = $('audio');
    var tiles = {}; // key -> {el, identity, source, local}
    var speaker = '';
    var say = opts.onStatus || function () {};
    if (!LK) {
      say('The call could not load. Reload the page.');
      return Promise.reject(new Error('livekit-client is missing'));
    }
    var room = new LK.Room({ adaptiveStream: true, dynacast: true });

    function tile(identity, source, name, local) {
      var k = tileKey(identity, source);
      var t = tiles[k];
      if (!t) {
        var el = document.createElement('figure');
        el.className = 'tile' + (source === 'screen_share' ? ' screen' : '') + (local ? ' local' : '');
        el.setAttribute('data-identity', identity);
        el.setAttribute('data-source', source);
        var cap = document.createElement('figcaption');
        cap.textContent = (name || identity) + (source === 'screen_share' ? ' · screen' : '') + (local ? ' (you)' : '');
        el.appendChild(cap);
        t = tiles[k] = { el: el, identity: identity, source: source, local: local };
      }
      return t;
    }

    function dropTile(identity, source) {
      var k = tileKey(identity, source);
      var t = tiles[k];
      if (t) { t.el.remove(); delete tiles[k]; }
    }

    function setVideo(t, track) {
      var old = t.el.querySelector('video');
      if (old) old.remove();
      if (track) {
        var v = track.attach();
        v.setAttribute('playsinline', '');
        if (t.local && t.source === 'camera') v.className = 'mirror';
        t.el.insertBefore(v, t.el.firstChild);
      }
    }

    // layout puts one tile on the stage: a shared screen first, else the
    // active speaker (someone else before yourself); the rest in the strip.
    function layout() {
      var all = Object.keys(tiles).map(function (k) { return tiles[k]; });
      var pick = all.filter(function (t) { return t.source === 'screen_share'; })[0] ||
        all.filter(function (t) { return t.identity === speaker && !t.local; })[0] ||
        all.filter(function (t) { return !t.local; })[0] || all[0];
      all.forEach(function (t) {
        var want = t === pick ? stage : strip;
        if (t.el.parentNode !== want) want.appendChild(t.el);
      });
      strip.hidden = strip.children.length === 0;
    }

    function addParticipant(p, local) {
      tile(p.identity, 'camera', p.name, local);
      layout();
    }

    room.on(LK.RoomEvent.ParticipantConnected, function (p) { addParticipant(p, false); });
    room.on(LK.RoomEvent.ParticipantDisconnected, function (p) {
      Object.keys(tiles).forEach(function (k) { if (tiles[k].identity === p.identity) { tiles[k].el.remove(); delete tiles[k]; } });
      layout();
    });
    room.on(LK.RoomEvent.TrackSubscribed, function (track, pub, p) {
      if (track.kind === 'audio') { audio.appendChild(track.attach()); return; }
      setVideo(tile(p.identity, track.source, p.name, false), track);
      layout();
    });
    room.on(LK.RoomEvent.TrackUnsubscribed, function (track, pub, p) {
      track.detach().forEach(function (el) { el.remove(); });
      if (track.source === 'screen_share') dropTile(p.identity, 'screen_share');
      else if (tiles[tileKey(p.identity, track.source)]) setVideo(tiles[tileKey(p.identity, track.source)], null);
      layout();
    });
    room.on(LK.RoomEvent.LocalTrackPublished, function (pub) {
      var track = pub.track;
      if (!track || track.kind !== 'video') return;
      setVideo(tile(room.localParticipant.identity, track.source, room.localParticipant.name, true), track);
      layout();
    });
    room.on(LK.RoomEvent.LocalTrackUnpublished, function (pub) {
      if (pub.source === 'screen_share') dropTile(room.localParticipant.identity, 'screen_share');
      else if (pub.source === 'camera') setVideo(tile(room.localParticipant.identity, 'camera', room.localParticipant.name, true), null);
      syncButtons();
      layout();
    });
    room.on(LK.RoomEvent.ActiveSpeakersChanged, function (speakers) {
      var s = speakers.filter(function (p) { return p.identity !== room.localParticipant.identity; })[0];
      if (s && s.identity !== speaker) { speaker = s.identity; layout(); }
    });
    room.on(LK.RoomEvent.Reconnecting, function () { say('Reconnecting…'); });
    room.on(LK.RoomEvent.Reconnected, function () { say(''); });
    room.on(LK.RoomEvent.Disconnected, function (reason) {
      clear();
      if (opts.onGone) opts.onGone(reason);
    });

    function clear() {
      Object.keys(tiles).forEach(function (k) { tiles[k].el.remove(); });
      tiles = {};
      audio.textContent = '';
    }

    var mic = $('btn-mic'), cam = $('btn-cam'), share = $('btn-share');
    function syncButtons() {
      var lp = room.localParticipant;
      var m = lp.isMicrophoneEnabled, c = lp.isCameraEnabled, s = lp.isScreenShareEnabled;
      mic.textContent = m ? 'Mute' : 'Unmute'; mic.setAttribute('aria-pressed', String(m));
      cam.textContent = c ? 'Camera off' : 'Camera on'; cam.setAttribute('aria-pressed', String(c));
      share.textContent = s ? 'Stop sharing' : 'Share screen'; share.setAttribute('aria-pressed', String(s));
    }
    function toggle(fn) {
      return function () {
        fn().then(syncButtons, function (e) { say('That did not work: ' + (e && e.message ? e.message : 'refused')); syncButtons(); });
      };
    }
    var onMic = toggle(function () { return room.localParticipant.setMicrophoneEnabled(!room.localParticipant.isMicrophoneEnabled); });
    var onCam = toggle(function () { return room.localParticipant.setCameraEnabled(!room.localParticipant.isCameraEnabled); });
    var onShare = toggle(function () {
      return room.localParticipant.setScreenShareEnabled(!room.localParticipant.isScreenShareEnabled, { audio: true });
    });
    mic.addEventListener('click', onMic);
    cam.addEventListener('click', onCam);
    share.addEventListener('click', onShare);
    share.hidden = !(navigator.mediaDevices && navigator.mediaDevices.getDisplayMedia);

    function leave() {
      mic.removeEventListener('click', onMic);
      cam.removeEventListener('click', onCam);
      share.removeEventListener('click', onShare);
      opts.onGone = null;
      clear();
      return room.disconnect();
    }

    say('Connecting…');
    return room.connect(opts.url, opts.token, { rtcConfig: { iceTransportPolicy: opts.ice === 'all' ? 'all' : 'relay' } })
      .then(function () {
        say('');
        addParticipant(room.localParticipant, true);
        room.remoteParticipants.forEach(function (p) { addParticipant(p, false); });
        if (opts.media) {
          return room.localParticipant.enableCameraAndMicrophone().catch(function () {
            say('Your camera or microphone is off: allow them in the browser to be seen and heard.');
          });
        }
      })
      .then(function () { syncButtons(); return { leave: leave, room: room }; }, function (e) {
        clear();
        mic.removeEventListener('click', onMic);
        cam.removeEventListener('click', onCam);
        share.removeEventListener('click', onShare);
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

  // badgeText is the notice's badge, from the server's words.
  function showBadge(notice) {
    var b = $('badge');
    if (b) b.textContent = notice && notice.badge ? notice.badge : '';
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
  // or (a gap that closed) replace a line, partials are the lines in
  // progress at the end, and the status says when the transcript is paused.
  // The panel shows itself with its first line.
  function Transcript(list, statusEl, panel) {
    var partials = [];
    function reveal() { if (panel) panel.hidden = false; }
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
      },
      partials: function (ps) {
        follow(function () {
          partials.forEach(function (el) { el.remove(); });
          partials = (ps || []).map(function (p) {
            var li = lineEl({ id: 'p', kind: 'text', start_ms: p.start_ms, speaker: p.speaker, text: p.text + '…' });
            li.className = 'line partial';
            li.removeAttribute('data-id');
            list.appendChild(li);
            return li;
          });
        });
        if (partials.length) reveal();
      },
      status: function (s) {
        if (statusEl) statusEl.textContent = s || '';
        if (s) reveal();
      },
      reset: function () {
        list.textContent = '';
        partials = [];
        if (statusEl) statusEl.textContent = '';
        if (panel) panel.hidden = true;
      }
    };
  }

  window.MeetingsCall = { join: join, readEvents: readEvents, showBadge: showBadge, replacedElsewhere: replacedElsewhere,
    Transcript: Transcript, lineEl: lineEl, clock: clock };
})();
