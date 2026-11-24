// The owner app, inside muster's frame. The session token lives in this
// script's memory only: it is read from the page once, the attribute is
// removed, and it is never written to storage or a cookie. A reload finds no
// token and the server says "Open Meetings from muster again".
//
// The whole owner flow: New meeting, then share the link. The FIRST time,
// New meeting opens the pre-join (the camera check, and the link to copy
// or share) and Join; the check's result is remembered (static/prefs.js),
// and from then on New meeting goes straight into the call with the link on
// screen. Storage that is blocked just means the check again. Guests who knock are a
// count on the People button and a toast with Admit; one tap admits them.
// Every name a guest typed is rendered as text only (textContent, or an
// attribute value).
(function () {
  'use strict';
  var root = document.getElementById('app');
  if (!root) return;
  var token = root.getAttribute('data-session');
  var ref = root.getAttribute('data-ref') || '';
  root.removeAttribute('data-session');
  root.removeAttribute('data-expires');
  root.removeAttribute('data-ref');
  // Take muster's one-time code out of the address bar and the history.
  try { history.replaceState(null, '', location.pathname); } catch (e) { /* not fatal */ }

  var UI = window.MeetingsUI;
  var $ = function (id) { return document.getElementById(id); };
  var list = $('meetings'), empty = $('empty'), more = $('more'), status = $('status');
  var next = null;
  var call = null; // the open meeting: {id, url, abort, conn, ended, joined}

  function say(text) { status.textContent = text || ''; }
  function callSay(text) { $('call-status').textContent = text || ''; }
  function preSay(text) { $('pre-status').textContent = text || ''; }

  function ended() {
    token = null;
    if (call) closeCall();
    document.body.textContent = '';
    var m = document.createElement('main');
    m.className = 'page notice';
    var h = document.createElement('h1');
    h.textContent = 'Meetings';
    var p = document.createElement('p');
    p.textContent = 'Your session ended. Open Meetings from muster again.';
    m.appendChild(h); m.appendChild(p);
    document.body.appendChild(m);
  }

  function request(method, path, body, signal) {
    if (!token) return Promise.reject(new Error('no session'));
    var init = { method: method, headers: { 'Authorization': 'Bearer ' + token }, cache: 'no-store', credentials: 'omit' };
    if (signal) init.signal = signal;
    if (body !== undefined) {
      init.headers['Content-Type'] = 'application/json';
      init.body = JSON.stringify(body);
    }
    return fetch(path, init).then(function (res) {
      if (res.status === 401) { ended(); throw new Error('session ended'); }
      return res;
    });
  }

  function api(method, path, body) {
    return request(method, path, body).then(function (res) {
      return res.json().catch(function () { return {}; }).then(function (b) {
        if (!res.ok) { var e = new Error(b.error || ('the server answered ' + res.status)); e.status = res.status; throw e; }
        return b;
      });
    });
  }

  function when(iso) {
    var d = new Date(iso);
    return d.toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' });
  }

  function duration(m) {
    if (!m.started_at || !m.ended_at) return '';
    var s = Math.max(0, Math.round((new Date(m.ended_at) - new Date(m.started_at)) / 1000));
    var h = Math.floor(s / 3600), min = Math.round((s % 3600) / 60);
    return h > 0 ? h + ' h ' + min + ' min' : min + ' min';
  }

  // linkLive: the server still gives this meeting's link (open, under 24 h).
  function linkLive(m) { return m.state === 'open' && new Date(m.link_expires_at) > new Date(); }

  // copyText copies, or falls back to selecting the given field.
  function copyText(text, input) {
    var fallback = function () { if (input) input.select(); UI.toast('Press Ctrl+C (or ⌘C) to copy the link.'); };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      return navigator.clipboard.writeText(text).then(function () { UI.toast('Link copied.', { kind: 'success', timeout: 2500 }); }, fallback);
    }
    fallback();
    return Promise.resolve();
  }

  // linkOf asks the server for an open meeting's link again (it is derived
  // there, never kept in this page between visits).
  function linkOf(id) {
    return api('GET', '/api/meetings/' + encodeURIComponent(id)).then(function (b) {
      if (!b.link) throw new Error("this meeting's link has expired");
      return b.link;
    });
  }

  function row(m) {
    var li = document.createElement('li');
    li.className = 'meeting';
    li.id = 'meeting-' + m.id;
    if (m.id === ref) li.className += ' current';
    var t = document.createElement('span');
    t.className = 'title';
    t.textContent = m.title || when(m.created_at);
    var meta = document.createElement('span');
    meta.className = 'meta';
    var bits = [];
    if (m.title) bits.push(when(m.created_at));
    bits.push(m.state === 'open' ? 'Open' : 'Ended');
    var d = duration(m);
    if (d) bits.push(d);
    meta.textContent = bits.join(' · ');
    var text = document.createElement('div');
    text.className = 'text';
    text.appendChild(t); text.appendChild(meta);
    li.appendChild(text);
    var actions = document.createElement('div');
    actions.className = 'actions';
    var title = t.textContent;
    if (linkLive(m)) {
      actions.appendChild(UI.iconButton('phone', 'Open ' + title, 'open-row', function () {
        linkOf(m.id).then(function (url) { openMeeting(m.id, url); })
          .catch(function (e) { if (token) say('Could not open the meeting: ' + e.message); });
      }));
      actions.appendChild(UI.iconButton('link', 'Copy link to ' + title, 'copy-row', function () {
        say('');
        linkOf(m.id).then(function (url) { $('join-url').value = url; return copyText(url, null); })
          .catch(function (e) { if (token) say('Could not get the link: ' + e.message); });
      }));
    }
    actions.appendChild(UI.iconButton('transcript', 'Transcript of ' + title, 'transcript-row', function () { openArchive(m); }));
    li.appendChild(actions);
    return li;
  }

  // show puts one of the four views on screen.
  function show(view) {
    ['list-view', 'archive-view', 'prejoin', 'call-view'].forEach(function (id) { $(id).hidden = id !== view; });
  }

  // ---- the archive: one meeting's transcript, and its downloads ----

  var archived = null; // the meeting the archive view shows

  function archiveSay(text) { $('archive-status').textContent = text || ''; }

  function openArchive(m) {
    archived = m;
    show('archive-view');
    $('archive-lines').textContent = '';
    $('archive-empty').hidden = true;
    media.reset();
    var meta = [when(m.created_at), m.state === 'open' ? 'Open' : 'Ended'];
    var d = duration(m);
    if (d) meta.push(d);
    $('archive-title').textContent = m.title || 'Transcript';
    $('archive-meta').textContent = meta.join(' · ');
    archiveSay('Loading…');
    var id = encodeURIComponent(m.id);
    // The time-aligned bundle when the plugin records (each line with its
    // place in the recording); the transcript alone when it does not (the
    // bundle's route is then the 404 every unknown path is).
    api('GET', '/api/meetings/' + id + '/timeline').catch(function (e) {
      if (e.status !== 404) throw e;
      return api('GET', '/api/meetings/' + id + '/transcript');
    }).then(function (b) {
      if (archived !== m) return;
      archiveSay('');
      var l = $('archive-lines');
      b.segments.forEach(function (g) {
        var li = MeetingsCall.lineEl(g);
        if (g.recording) media.seekable(li, g.recording);
        l.appendChild(li);
      });
      $('archive-empty').hidden = b.segments.length > 0;
      if (b.parts) media.load(m);
    }).catch(function (e) { if (token && archived === m) archiveSay('Could not load the transcript: ' + e.message); });
    $('archive-back').focus();
  }

  function closeArchive() {
    archived = null;
    media.reset();
    show('list-view');
  }

  // ---- the recording in the archive ----
  //
  // The video plays a signed /media URL per part (a <video> cannot carry the
  // bearer; each URL lives 15 minutes, and a URL that has expired is asked
  // for again). A line with a place in the recording ({part, offset_ms},
  // computed by the server from the egress's own start) plays from there
  // when clicked: the visible proof that the two are aligned.
  var media = (function () {
    var video = $('archive-video'), box = $('archive-media');
    var urls = {}, part = 0, meetingID = null, pending = null, retried = false;
    function urlsFor(id) {
      return api('POST', '/api/meetings/' + encodeURIComponent(id) + '/recording-url').then(function (b) {
        urls = {};
        b.parts.forEach(function (p) { urls[p.part] = p.url; });
        return b.parts;
      });
    }
    function play(p, offsetMS) {
      var at = offsetMS / 1000;
      if (p !== part || !video.getAttribute('src')) {
        part = p;
        pending = at;
        video.src = urls[p];
        $('archive-part').hidden = Object.keys(urls).length < 2;
        $('archive-part').textContent = 'Part ' + p;
        return;
      }
      if (video.readyState >= 1) video.currentTime = at; else pending = at;
    }
    video.addEventListener('loadedmetadata', function () {
      if (pending != null) { video.currentTime = pending; pending = null; }
    });
    video.addEventListener('error', function () {
      // An expired URL: ask once for fresh ones, and carry on where it was.
      if (retried || !meetingID) return;
      retried = true;
      var at = video.currentTime || pending || 0;
      urlsFor(meetingID).then(function () { var p = part; part = 0; play(p, at * 1000); }).catch(function () {});
    });
    video.addEventListener('playing', function () { retried = false; });
    return {
      reset: function () {
        meetingID = null; urls = {}; part = 0; pending = null; retried = false;
        video.removeAttribute('src');
        try { video.load(); } catch (e) { /* not fatal */ }
        box.hidden = true;
        $('share-out').hidden = true;
        $('delete-confirm').hidden = true;
        $('archive-deleting').hidden = true;
      },
      load: function (m) {
        meetingID = m.id;
        urlsFor(m.id).then(function (parts) {
          if (archived !== m || !parts.length) return;
          box.hidden = false;
          play(parts[0].part, 0);
          return api('GET', '/api/meetings/' + encodeURIComponent(m.id) + '/recording').then(function (r) {
            if (archived !== m) return;
            $('unshare-rec').hidden = !(r.share && r.share.live);
          });
        }).then(function () {
          if (archived !== m || !box.hidden) return;
          // No part to play: one deleted, and still leaving the store?
          return api('GET', '/api/meetings/' + encodeURIComponent(m.id) + '/recording').then(function (r) {
            if (archived === m) $('archive-deleting').hidden = !r.deleting;
          });
        }).catch(function (e) { if (token && archived === m) archiveSay('Could not load the recording: ' + e.message); });
      },
      seekable: function (li, at) {
        li.classList.add('seekable');
        li.tabIndex = 0;
        li.setAttribute('data-part', String(at.part));
        li.setAttribute('data-offset-ms', String(at.offset_ms));
        li.title = 'Play the recording from here';
        var go = function () {
          if (!urls[at.part]) return;
          Array.prototype.forEach.call(document.querySelectorAll('#archive-lines li.playing'), function (x) { x.classList.remove('playing'); });
          li.classList.add('playing');
          play(at.part, at.offset_ms);
        };
        li.addEventListener('click', go);
        li.addEventListener('keydown', function (ev) { if (ev.key === 'Enter' || ev.key === ' ') { ev.preventDefault(); go(); } });
      }
    };
  })();

  // The share link: minted (or minted anew, which turns the old one off)
  // and shown once, since the server keeps only its hash.
  $('share-rec').addEventListener('click', function () {
    if (!archived) return;
    var m = archived;
    archiveSay('');
    api('POST', '/api/meetings/' + encodeURIComponent(m.id) + '/share').then(function (b) {
      if (archived !== m) return;
      $('share-url').value = b.url;
      // expires_at null: the recording is kept forever.
      $('share-until').textContent = b.expires_at ? 'until ' + new Date(b.expires_at).toLocaleDateString(undefined, { dateStyle: 'medium' })
        : 'until you turn the link off or delete the recording';
      $('share-out').hidden = false;
      $('unshare-rec').hidden = false;
      $('share-url').select();
    }).catch(function (e) { if (token) archiveSay('Could not share the recording: ' + e.message); });
  });
  $('share-copy').addEventListener('click', function () { copyText($('share-url').value, $('share-url')); });
  $('unshare-rec').addEventListener('click', function () {
    if (!archived) return;
    var m = archived;
    api('DELETE', '/api/meetings/' + encodeURIComponent(m.id) + '/share').then(function () {
      if (archived !== m) return;
      $('share-out').hidden = true;
      $('unshare-rec').hidden = true;
      UI.toast('The link no longer opens the recording.', { timeout: 3000 });
    }).catch(function (e) { if (token) archiveSay('Could not turn the link off: ' + e.message); });
  });

  // Delete now: the confirmation first; then every part and every share link
  // go at once. A store that is down finishes later: the archive says so.
  $('delete-rec').addEventListener('click', function () {
    $('delete-confirm').hidden = false;
    $('delete-no').focus();
  });
  $('delete-no').addEventListener('click', function () { $('delete-confirm').hidden = true; $('delete-rec').focus(); });
  $('delete-yes').addEventListener('click', function () {
    if (!archived) return;
    var m = archived, b = this;
    b.disabled = true;
    api('DELETE', '/api/meetings/' + encodeURIComponent(m.id) + '/recording').then(function (r) {
      if (archived !== m) return;
      media.reset();
      $('archive-deleting').hidden = !(r.pending > 0);
      UI.toast('The recording was deleted.', { kind: 'success', timeout: 3000 });
    }).catch(function (e) { if (token) archiveSay('Could not delete the recording: ' + e.message); })
      .then(function () { b.disabled = false; });
  });

  // The recording retention (only a plugin that records answers it). 0 is
  // forever.
  function retentionAfter(v) { $('retention-after').textContent = v === '0' ? 'until you delete them.' : 'after a meeting ends.'; }
  api('GET', '/api/settings').then(function (b) {
    var sel = $('retention-days');
    sel.value = String(b.settings.recording_retention_days);
    retentionAfter(sel.value);
    $('retention').hidden = false;
    sel.addEventListener('change', function () {
      retentionAfter(sel.value);
      api('PUT', '/api/settings', { recording_retention_days: parseInt(sel.value, 10) }).then(function () {
        UI.toast(sel.value === '0' ? 'Recordings of meetings that end from now on are kept until you delete them.'
          : 'Recordings of meetings that end from now on are kept ' + sel.value + ' days.', { kind: 'success', timeout: 3000 });
      }).catch(function (e) { if (token) say('Could not change how long recordings are kept: ' + e.message); });
    });
  }).catch(function () { /* no recording: the setting stays hidden */ });

  // download fetches the export with the bearer (a link cannot carry it) and
  // saves it from a blob: URL.
  function download(format) {
    if (!archived) return;
    var id = archived.id;
    archiveSay('');
    request('GET', '/api/meetings/' + encodeURIComponent(id) + '/transcript?format=' + format).then(function (res) {
      if (!res.ok) throw new Error('the server answered ' + res.status);
      return res.blob();
    }).then(function (blob) {
      var a = document.createElement('a');
      a.href = URL.createObjectURL(blob);
      a.download = 'meeting-' + id + '.' + format;
      document.body.appendChild(a);
      a.click();
      a.remove();
      setTimeout(function () { URL.revokeObjectURL(a.href); }, 10000);
    }).catch(function (e) { if (token) archiveSay('Could not download: ' + e.message); });
  }

  $('archive-back').addEventListener('click', closeArchive);
  $('dl-txt').addEventListener('click', function () { download('txt'); });
  $('dl-vtt').addEventListener('click', function () { download('vtt'); });

  function load(append) {
    var path = '/api/meetings' + (append && next ? '?cursor=' + encodeURIComponent(next) : '');
    return api('GET', path).then(function (b) {
      if (!append) list.textContent = '';
      b.meetings.forEach(function (m) { list.appendChild(row(m)); });
      next = b.next;
      more.hidden = !next;
      empty.hidden = list.children.length > 0;
    }).catch(function (e) { if (token) say('Could not load your meetings: ' + e.message); });
  }

  // ---- the lobby: a count on People, a toast per new knock, the panel ----

  var side = MeetingsCall.Side();
  side.add('people', $('lobby-panel'), $('btn-people'));
  var transcript = MeetingsCall.transcriptPanel(side, function () { return waiting.length > 0; });
  var knocked = {}; // participant ids already announced
  var waiting = [];

  function act(method, path, b) {
    if (b) b.disabled = true;
    return api(method, path).catch(function (e) { if (token) UI.toast(e.message, { kind: 'error' }); if (b) b.disabled = false; });
  }

  function lobbyRow(meetingID, p) {
    var div = document.createElement('div');
    div.className = 'lobby-row ' + p.state;
    div.setAttribute('data-pid', p.id);
    var text = document.createElement('div');
    text.className = 'text';
    var name = document.createElement('span');
    name.className = 'who';
    name.textContent = p.name + ' (guest)';
    var what = document.createElement('span');
    what.className = 'what';
    what.textContent = p.state === 'waiting' ? 'Wants to join' : 'In the call';
    text.appendChild(name); text.appendChild(what);
    div.appendChild(text);
    var base = '/api/meetings/' + encodeURIComponent(meetingID);
    if (p.state === 'waiting') {
      var admit = UI.iconButton('check', 'Admit ' + p.name, 'admit');
      admit.addEventListener('click', function () { act('POST', base + '/lobby/' + p.id + '/admit', admit); });
      var deny = UI.iconButton('close', 'Deny ' + p.name, 'deny');
      deny.addEventListener('click', function () { act('POST', base + '/lobby/' + p.id + '/deny', deny); });
      div.appendChild(admit); div.appendChild(deny);
    } else {
      var rm = UI.iconButton('minus', 'Remove ' + p.name, 'remove');
      rm.addEventListener('click', function () { act('DELETE', base + '/participants/' + p.id, rm); });
      div.appendChild(rm);
    }
    return div;
  }

  function renderSnapshot(s) {
    if (!call) return;
    var lobby = $('lobby');
    lobby.textContent = '';
    waiting = s.lobby.filter(function (p) { return p.state === 'waiting'; });
    s.lobby.forEach(function (p) { lobby.appendChild(lobbyRow(call.id, p)); });
    $('lobby-empty').hidden = s.lobby.length > 0;
    $('admit-all').hidden = waiting.length < 2;
    var count = $('lobby-count');
    count.textContent = waiting.length ? String(waiting.length) : '';
    count.hidden = !waiting.length;
    UI.label($('btn-people'), waiting.length ? 'People, ' + waiting.length + ' waiting' : 'People');
    // A new knock: a toast with Admit, unless the lobby is open already.
    waiting.forEach(function (p) {
      if (knocked[p.id]) return;
      knocked[p.id] = true;
      if (side.isOpen('people')) return;
      var id = call.id;
      UI.toast(p.name + ' wants to join', { timeout: 10000, action: { label: 'Admit', onClick: function () {
        act('POST', '/api/meetings/' + encodeURIComponent(id) + '/lobby/' + p.id + '/admit');
      } } });
    });
    MeetingsCall.showBadge(s.notice, { owner: true });
    // The Record switch only on a plugin that can record (it has a bucket).
    $('sw-record-row').hidden = !s.notice.recordable;
    [['sw-record', s.notice.record], ['sw-ai', s.notice.ai]].forEach(function (x) {
      var box = $(x[0]);
      box.checked = x[1];
      // Off can always be chosen; on only before any guest knocked.
      box.disabled = !x[1] && s.locked;
    });
    if (s.meeting.state !== 'open') meetingOver('This meeting has ended.');
  }

  $('admit-all').addEventListener('click', function () {
    if (!call) return;
    var b = this, id = call.id;
    b.disabled = true;
    waiting.reduce(function (pr, p) {
      return pr.then(function () { return api('POST', '/api/meetings/' + encodeURIComponent(id) + '/lobby/' + p.id + '/admit'); });
    }, Promise.resolve()).catch(function (e) { if (token) UI.toast(e.message, { kind: 'error' }); }).then(function () { b.disabled = false; });
  });

  // The meeting's event stream: the lobby, the notice, the live transcript.
  // Each stream sends the transcript from its start, so a new one redraws it.
  function watch(id, abort) {
    return request('GET', '/api/meetings/' + encodeURIComponent(id) + '/events', undefined, abort.signal).then(function (res) {
      if (!res.ok) throw new Error('the server answered ' + res.status);
      transcript.reset();
      return MeetingsCall.readEvents(res, function (name, data) {
        if (name === 'meeting') renderSnapshot(data);
        else if (name === 'segment') transcript.segment(data);
        else if (name === 'amend') transcript.amend(data);
        else if (name === 'partials') transcript.partials(data.partials);
        else if (name === 'transcript') transcript.status(data.status);
      });
    }).then(function () {
      // The stream ended. While this meeting is still open here and has not
      // ended, open it again.
      if (call && call.id === id && !call.ended && token) setTimeout(function () { if (call && call.id === id) watch(id, abort).catch(function () {}); }, 2000);
    });
  }

  function connState(s) { $('call-view').setAttribute('data-conn', s); }

  // ---- pre-join ----

  var pre = MeetingsCall.prejoin({ root: $('prejoin'), remember: true, onState: function (s) {
    $('btn-join').disabled = s === 'checking' || s === 'idle';
  } });

  // The invite card: the link on the stage while you are alone in the call.
  var inviteClosed = false;
  function invite(alone) { $('invite').hidden = inviteClosed || !alone || !call; }
  $('invite-close').addEventListener('click', function () { inviteClosed = true; invite(false); });
  $('invite-copy').addEventListener('click', function () { copyText($('call-url').value, $('call-url')); });
  $('invite-share').addEventListener('click', function () {
    navigator.share({ title: 'Join my meeting', url: $('call-url').value }).catch(function () { /* dismissed */ });
  });

  // enterCall goes into the call with media (the check's choice, or the
  // remembered one, minus any device that is gone).
  function enterCall(media) {
    if (!call || call.joined) return;
    call.joined = true;
    pre.stop();
    show('call-view');
    inviteClosed = false;
    invite(true);
    var id = call.id;
    // Connecting from this moment: the device lookup below is part of it.
    connState('connecting');
    MeetingsCall.usable(media).then(function (m) { if (call && call.id === id) connect(id, m); });
  }

  // openMeeting opens a meeting's pre-join: the device check, and the link.
  // The meeting's event stream starts here, so a knock is shown (and the
  // owner holds the stream: no knock notification) before Join.
  function openMeeting(id, url) {
    if (call) closeCall();
    if (archived) closeArchive();
    call = { id: id, url: url || '', abort: new AbortController(), conn: null, ended: false, joined: false };
    knocked = {};
    $('join-url').value = url || '';
    $('call-url').value = url || '';
    $('share-link').hidden = $('invite-share').hidden = !navigator.share;
    $('lobby').textContent = '';
    $('btn-use-here').hidden = true;
    callSay('');
    preSay('');
    pre.start();
    if (pre.state() === 'remembered') enterCall(pre.choice());
    else show('prejoin');
    watch(id, call.abort).catch(function (e) { if (token && call && call.id === id && e.name !== 'AbortError') preSay('Lost touch with the meeting: ' + e.message); });
  }

  $('btn-join').addEventListener('click', function () {
    if (!call || call.joined) return;
    pre.keep();
    enterCall(pre.choice());
  });
  $('prejoin-back').addEventListener('click', function () { closeCall(); load(false); });

  // connect joins the call. You are one identity ("owner") in your meeting,
  // so opening it on a second device makes the newer join win: LiveKit
  // disconnects this page with DUPLICATE_IDENTITY, and it says so, with Use
  // here to take the call back (which in turn moves the other device here),
  // instead of rejoining on its own and kicking the other back and forth.
  function connect(id, media) {
    $('btn-use-here').hidden = true;
    connState('connecting');
    call.media = media;
    api('POST', '/api/meetings/' + encodeURIComponent(id) + '/token').then(function (t) {
      if (!call || call.id !== id) return;
      return MeetingsCall.join({ url: t.livekit_url, token: t.token, ice: t.ice, media: media, onStatus: callSay,
        onRoster: function (n) { invite(n === 0); },
        // A device picked in the call's settings is the remembered one.
        onDevice: function (field, devId) {
          var p = window.MeetingsPrefs && window.MeetingsPrefs.load();
          if (p) { p[field] = devId; window.MeetingsPrefs.save(p); }
        },
        onGone: function (reason) {
          if (!call || call.id !== id || call.ended) return;
          call.conn = null;
          if (MeetingsCall.replacedElsewhere(reason)) {
            connState('elsewhere');
            callSay('Opened on another device.');
            $('btn-use-here').hidden = false;
            return;
          }
          connState('none');
          callSay('The call disconnected. Leave, then open the meeting again to rejoin.');
        } })
        .then(function (conn) { if (call && call.id === id) { call.conn = conn; connState('connected'); } else conn.leave(); });
    }).catch(function (e) {
      if (token && call && call.id === id && !call.conn) { connState('none'); callSay(e.message === "Can't connect media" ? e.message : callStatusOf(e)); }
    });
  }

  function callStatusOf(e) {
    return e && e.status === 410 ? 'This meeting has ended.' : (e && e.message ? e.message : "Can't connect media. Check your connection and try again.");
  }

  function closeCall() {
    if (!call) return;
    call.abort.abort();
    if (call.conn) call.conn.leave();
    call = null;
    pre.stop();
    invite(false);
    side.close(false);
    UI.closePop(false);
    show('list-view');
  }

  function meetingOver(text) {
    if (!call || call.ended) return;
    call.ended = true;
    if (call.conn) { call.conn.leave(); call.conn = null; }
    $('lobby').textContent = '';
    callSay(text);
  }

  UI.popover($('btn-leave'), $('leave-menu'));
  $('btn-back').addEventListener('click', function () { closeCall(); load(false); });
  $('btn-end').addEventListener('click', function () {
    if (!call) return;
    var id = call.id, b = this;
    b.disabled = true;
    api('POST', '/api/meetings/' + encodeURIComponent(id) + '/end').then(function () {
      meetingOver('This meeting has ended.');
      closeCall();
      load(false);
    }).catch(function (e) { if (token) callSay(e.message); }).then(function () { b.disabled = false; });
  });
  [['sw-record', 'record'], ['sw-ai', 'ai']].forEach(function (x) {
    $(x[0]).addEventListener('change', function () {
      if (!call) return;
      var box = this;
      api('POST', '/api/meetings/' + encodeURIComponent(call.id) + '/' + x[1], { on: box.checked })
        .then(function (b) { MeetingsCall.showBadge(b.notice, { owner: true }); })
        .catch(function (e) { box.checked = !box.checked; if (token) callSay(e.message); });
    });
  });
  $('btn-use-here').addEventListener('click', function () { if (call && !call.ended) connect(call.id, call.media); });
  $('call-copy').addEventListener('click', function () { copyText($('join-url').value, null); });
  $('copy-link').addEventListener('click', function () { copyText($('join-url').value, $('join-url')); });
  $('share-link').addEventListener('click', function () {
    navigator.share({ title: 'Join my meeting', url: $('join-url').value }).catch(function () { /* dismissed */ });
  });

  $('new-meeting').addEventListener('click', function () {
    var btn = this;
    btn.disabled = true;
    say('');
    api('POST', '/api/meetings', {}).then(function (b) {
      list.insertBefore(row(b.meeting), list.firstChild);
      empty.hidden = true;
      openMeeting(b.id, b.join_url);
    }).catch(function (e) { if (token) say('Could not make a meeting: ' + e.message); })
      .then(function () { btn.disabled = false; });
  });

  more.addEventListener('click', function () { load(true); });

  load(false);
  // Opened for one meeting (a knock notification's tap): open its pre-join
  // at once, while it is open.
  if (ref) {
    linkOf(ref).then(function (url) { openMeeting(ref, url); }).catch(function () { /* ended: the list says so */ });
  }
})();
