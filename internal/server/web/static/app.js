// The owner app, inside muster's frame. The session token lives in this
// script's memory only: it is read from the page once, the attribute is
// removed, and it is never written to storage or a cookie. A reload finds no
// token and the server says "Open Meetings from muster again".
//
// The whole owner flow: New meeting, which puts you in the call with the
// link to copy. Guests who knock appear at the top of the call; one tap
// admits them. Every name a guest typed is rendered with textContent only.
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

  var $ = function (id) { return document.getElementById(id); };
  var list = $('meetings'), empty = $('empty'), more = $('more'), status = $('status');
  var next = null;
  var call = null; // the open call: {id, abort, conn, ended}

  function say(text) { status.textContent = text || ''; }
  function callSay(text) { $('call-status').textContent = text || ''; }

  function ended() {
    token = null;
    if (call) closeCall();
    document.body.textContent = '';
    var m = document.createElement('main');
    m.className = 'notice';
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
  function copyText(text, input, sayFn) {
    var fallback = function () { input.select(); sayFn('Press Ctrl+C (or ⌘C) to copy the link.'); };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      return navigator.clipboard.writeText(text).then(function () { sayFn('Link copied.'); }, fallback);
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

  function button(text, cls, onClick) {
    var b = document.createElement('button');
    b.type = 'button';
    b.textContent = text;
    if (cls) b.className = cls;
    b.addEventListener('click', onClick);
    return b;
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
    if (linkLive(m)) {
      actions.appendChild(button('Open', 'primary open-row', function () {
        linkOf(m.id).then(function (url) { openCall(m.id, url); })
          .catch(function (e) { if (token) say('Could not open the meeting: ' + e.message); });
      }));
      actions.appendChild(button('Copy link', 'copy-row', function () {
        say('');
        linkOf(m.id).then(function (url) { showLink(url); return copyText(url, $('join-url'), say); })
          .catch(function (e) { if (token) say('Could not get the link: ' + e.message); });
      }));
    }
    actions.appendChild(button('Transcript', 'transcript-row', function () { openArchive(m); }));
    li.appendChild(actions);
    return li;
  }

  // ---- the archive: one meeting's transcript, and its downloads ----

  var archived = null; // the meeting the archive view shows

  function archiveSay(text) { $('archive-status').textContent = text || ''; }

  function openArchive(m) {
    archived = m;
    $('list-view').hidden = true;
    $('archive-view').hidden = false;
    $('archive-lines').textContent = '';
    $('archive-empty').hidden = true;
    var meta = [when(m.created_at), m.state === 'open' ? 'Open' : 'Ended'];
    var d = duration(m);
    if (d) meta.push(d);
    $('archive-title').textContent = m.title || 'Transcript';
    $('archive-meta').textContent = meta.join(' · ');
    archiveSay('Loading…');
    api('GET', '/api/meetings/' + encodeURIComponent(m.id) + '/transcript').then(function (b) {
      if (archived !== m) return;
      archiveSay('');
      var list = $('archive-lines');
      b.segments.forEach(function (g) { list.appendChild(MeetingsCall.lineEl(g)); });
      $('archive-empty').hidden = b.segments.length > 0;
    }).catch(function (e) { if (token && archived === m) archiveSay('Could not load the transcript: ' + e.message); });
  }

  function closeArchive() {
    archived = null;
    $('archive-view').hidden = true;
    $('list-view').hidden = false;
  }

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

  function showLink(url) {
    $('join-url').value = url;
    $('link-panel').hidden = false;
    $('share-link').hidden = !navigator.share;
  }

  // ---- the call ----

  function lobbyRow(meetingID, p, waitingCount) {
    var div = document.createElement('div');
    div.className = 'lobby-row ' + p.state;
    div.setAttribute('data-pid', p.id);
    var name = document.createElement('span');
    name.className = 'who';
    name.textContent = p.name + ' (guest)';
    var what = document.createElement('span');
    what.className = 'what';
    what.textContent = p.state === 'waiting' ? ' wants to join' : ' is in the call';
    div.appendChild(name); div.appendChild(what);
    var base = '/api/meetings/' + encodeURIComponent(meetingID);
    var act = function (method, path, b) {
      return function () {
        b.disabled = true;
        api(method, path).catch(function (e) { if (token) callSay(e.message); b.disabled = false; });
      };
    };
    if (p.state === 'waiting') {
      var admit = button('Admit', 'primary admit', null);
      admit.addEventListener('click', act('POST', base + '/lobby/' + p.id + '/admit', admit));
      var deny = button('Deny', 'deny', null);
      deny.addEventListener('click', act('POST', base + '/lobby/' + p.id + '/deny', deny));
      div.appendChild(admit); div.appendChild(deny);
    } else {
      var rm = button('Remove', 'remove', null);
      rm.addEventListener('click', act('DELETE', base + '/participants/' + p.id, rm));
      div.appendChild(rm);
    }
    return div;
  }

  function renderSnapshot(s) {
    if (!call) return;
    var lobby = $('lobby');
    lobby.textContent = '';
    var waiting = s.lobby.filter(function (p) { return p.state === 'waiting'; });
    if (waiting.length > 1) {
      var all = button('Admit all (' + waiting.length + ')', 'primary admit-all', function () {
        all.disabled = true;
        waiting.reduce(function (pr, p) {
          return pr.then(function () { return api('POST', '/api/meetings/' + encodeURIComponent(call.id) + '/lobby/' + p.id + '/admit'); });
        }, Promise.resolve()).catch(function (e) { if (token) callSay(e.message); });
      });
      lobby.appendChild(all);
    }
    s.lobby.forEach(function (p) { lobby.appendChild(lobbyRow(call.id, p, waiting.length)); });
    MeetingsCall.showBadge(s.notice);
    [['sw-record', s.notice.record], ['sw-ai', s.notice.ai]].forEach(function (x) {
      var box = $(x[0]);
      box.checked = x[1];
      // Off can always be chosen; on only before any guest knocked.
      box.disabled = !x[1] && s.locked;
    });
    if (s.meeting.state !== 'open') meetingOver('This meeting has ended.');
  }

  // The live transcript panel. Each event stream sends the transcript from
  // its start, so a new stream redraws it.
  var transcript = MeetingsCall.Transcript($('transcript'), $('transcript-status'), $('panels'));

  function watch(id, abort) {
    return request('GET', '/api/meetings/' + encodeURIComponent(id) + '/events', undefined, abort.signal).then(function (res) {
      if (!res.ok) throw new Error('the server answered ' + res.status);
      transcript.reset();
      return MeetingsCall.readEvents(res, function (name, data) {
        if (name === 'meeting') renderSnapshot(data);
        else if (name === 'segment') transcript.segment(data);
        else if (name === 'partials') transcript.partials(data.partials);
        else if (name === 'transcript') transcript.status(data.status);
      });
    }).then(function () {
      // The stream ended. While the call view is still this meeting's and it
      // has not ended, open it again.
      if (call && call.id === id && !call.ended && token) setTimeout(function () { if (call && call.id === id) watch(id, abort).catch(function () {}); }, 2000);
    });
  }

  function connState(s) { $('call-view').setAttribute('data-conn', s); }

  // connect joins the call. You are one identity ("owner") in your meeting,
  // so opening it on a second device makes the newer join win: LiveKit
  // disconnects this page with DUPLICATE_IDENTITY, and it says so, with Use
  // here to take the call back (which in turn moves the other device here),
  // instead of rejoining on its own and kicking the other back and forth.
  function connect(id) {
    $('btn-use-here').hidden = true;
    connState('connecting');
    api('POST', '/api/meetings/' + encodeURIComponent(id) + '/token').then(function (t) {
      if (!call || call.id !== id) return;
      return MeetingsCall.join({ url: t.livekit_url, token: t.token, ice: t.ice, media: true, onStatus: callSay,
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
          callSay('The call disconnected. Press Back, then Open, to rejoin.');
        } })
        .then(function (conn) { if (call && call.id === id) { call.conn = conn; connState('connected'); } else conn.leave(); });
    }).catch(function (e) {
      if (token && call && call.id === id && !call.conn) { connState('none'); callSay(e.message === "Can't connect media" ? e.message : callStatusOf(e)); }
    });
  }

  function callStatusOf(e) {
    return e && e.status === 410 ? 'This meeting has ended.' : (e && e.message ? e.message : "Can't connect media. Check your connection and try again.");
  }

  function openCall(id, url) {
    if (call) closeCall();
    if (archived) closeArchive();
    call = { id: id, abort: new AbortController(), conn: null, ended: false };
    $('list-view').hidden = true;
    $('call-view').hidden = false;
    $('call-url').value = url || '';
    $('lobby').textContent = '';
    $('btn-use-here').hidden = true;
    callSay('');
    watch(id, call.abort).catch(function (e) { if (token && call && call.id === id && e.name !== 'AbortError') callSay('Lost touch with the meeting: ' + e.message); });
    connect(id);
  }

  function closeCall() {
    if (!call) return;
    call.abort.abort();
    if (call.conn) call.conn.leave();
    call = null;
    $('call-view').hidden = true;
    $('list-view').hidden = false;
  }

  function meetingOver(text) {
    if (!call || call.ended) return;
    call.ended = true;
    if (call.conn) { call.conn.leave(); call.conn = null; }
    $('lobby').textContent = '';
    callSay(text);
  }

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
        .then(function (b) { MeetingsCall.showBadge(b.notice); })
        .catch(function (e) { box.checked = !box.checked; if (token) callSay(e.message); });
    });
  });
  $('btn-use-here').addEventListener('click', function () { if (call && !call.ended) connect(call.id); });
  $('call-copy').addEventListener('click', function () { copyText($('call-url').value, $('call-url'), callSay); });

  $('new-meeting').addEventListener('click', function () {
    var btn = this;
    btn.disabled = true;
    say('');
    api('POST', '/api/meetings', {}).then(function (b) {
      showLink(b.join_url);
      list.insertBefore(row(b.meeting), list.firstChild);
      empty.hidden = true;
      openCall(b.id, b.join_url);
    }).catch(function (e) { if (token) say('Could not make a meeting: ' + e.message); })
      .then(function () { btn.disabled = false; });
  });

  $('copy-link').addEventListener('click', function () { copyText($('join-url').value, $('join-url'), say); });

  $('share-link').addEventListener('click', function () {
    navigator.share({ title: 'Join my meeting', url: $('join-url').value }).catch(function () { /* dismissed */ });
  });

  more.addEventListener('click', function () { load(true); });

  load(false);
  // Opened for one meeting (a knock notification's tap): open its call at
  // once, while it is open.
  if (ref) {
    linkOf(ref).then(function (url) { showLink(url); openCall(ref, url); }).catch(function () { /* ended: the list says so */ });
  }
})();
