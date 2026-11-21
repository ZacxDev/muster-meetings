// The guest page: the notice, a name, Ask to join, the wait, the call.
// The ticket the knock answers lives in this script's memory only and is
// sent in request bodies, never in a URL. The page's state is on
// <main data-state>: start, waiting, admitted, denied, ended, removed.
(function () {
  'use strict';
  var root = document.getElementById('guest');
  if (!root) return;
  var $ = function (id) { return document.getElementById(id); };
  var base = location.pathname.replace(/\/+$/, '');
  var shown = { record: root.getAttribute('data-record') === 'true', ai: root.getAttribute('data-ai') === 'true' };
  var ticket = null;
  var preview = null; // the camera preview's stream
  var conn = null;
  var abort = null;

  function state(s) { root.setAttribute('data-state', s); }
  function say(text) { $('guest-status').textContent = text || ''; }
  function callSay(text) { $('call-status').textContent = text || ''; }

  function renderNotice(n) {
    var box = $('notice');
    box.textContent = '';
    (n.text || []).forEach(function (t) {
      var p = document.createElement('p');
      p.textContent = t;
      box.appendChild(p);
    });
    shown = { record: !!n.record, ai: !!n.ai };
  }

  function post(path, body, signal) {
    var init = { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body), cache: 'no-store', credentials: 'omit' };
    if (signal) init.signal = signal;
    return fetch(base + path, init);
  }

  function stopPreview() {
    if (preview) { preview.getTracks().forEach(function (t) { t.stop(); }); preview = null; }
  }

  $('btn-preview').addEventListener('click', function () {
    var b = this;
    if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) { say('This browser cannot use a camera here.'); return; }
    navigator.mediaDevices.getUserMedia({ audio: true, video: true }).then(function (s) {
      preview = s;
      $('preview').srcObject = s;
      b.hidden = true;
    }, function () { say('The camera and microphone are off. You can still ask to join.'); });
  });

  function finish(s, text) {
    state(s);
    stopPreview();
    if (abort) { abort.abort(); abort = null; }
    if (conn) { conn.leave(); conn = null; }
    ticket = null;
    document.body.textContent = '';
    var m = document.createElement('main');
    m.className = 'notice';
    m.id = 'guest';
    m.setAttribute('data-state', s);
    var h = document.createElement('h1');
    h.textContent = 'Meetings';
    var p = document.createElement('p');
    p.id = 'message';
    p.textContent = text;
    m.appendChild(h); m.appendChild(p);
    document.body.appendChild(m);
  }

  function onAdmitted(t) {
    state('admitted');
    var media = !!preview;
    stopPreview();
    $('join-view').hidden = true;
    $('call-view').hidden = false;
    events();
    MeetingsCall.join({ url: t.livekit_url, token: t.token, ice: t.ice, media: media, onStatus: callSay,
      onGone: function () { if (root.getAttribute('data-state') === 'admitted') callSay('The call disconnected.'); } })
      .then(function (c) { conn = c; }, function () { /* the status says so */ });
  }

  // events follows the admitted guest's stream: the CURRENT notice (its
  // badge), and the end or a removal.
  function events() {
    abort = new AbortController();
    post('/events', { ticket: ticket }, abort.signal).then(function (res) {
      if (res.status === 410) { finish('ended', 'This meeting has ended.'); return; }
      if (!res.ok) { finish('removed', 'You are no longer in this meeting.'); return; }
      return MeetingsCall.readEvents(res, function (name, data) {
        if (name === 'meeting') MeetingsCall.showBadge(data.notice);
        else if (name === 'ended') finish('ended', 'This meeting has ended.');
        else if (name === 'removed') finish('removed', 'You are no longer in this meeting.');
      });
    }).catch(function () { /* left, or the network: the call's own status says so */ });
  }

  function wait() {
    state('waiting');
    var who = root.getAttribute('data-owner');
    say(who ? 'Waiting for ' + who + ' to let you in.' : 'Waiting for the host to let you in.');
    abort = new AbortController();
    post('/wait', { ticket: ticket }, abort.signal).then(function (res) {
      if (res.status === 410) { finish('ended', 'This meeting has ended.'); return; }
      if (!res.ok) { finish('removed', 'Ask to join again.'); return; }
      var done = false;
      return MeetingsCall.readEvents(res, function (name, data) {
        if (name === 'waiting' && data.notice) renderNotice(data.notice);
        else if (name === 'admitted') { done = true; abort = null; onAdmitted(data); }
        else if (name === 'denied') { done = true; finish('denied', "You weren't admitted."); }
        else if (name === 'ended') { done = true; finish('ended', data.message || 'This meeting has ended.'); }
      }).then(function () { if (!done && ticket) setTimeout(wait, 2000); });
    }).catch(function (e) { if (e.name !== 'AbortError' && ticket) setTimeout(wait, 2000); });
  }

  $('knock-form').addEventListener('submit', function (ev) {
    ev.preventDefault();
    var b = $('ask');
    var name = $('name').value.trim();
    if (!name) { say('Type your name first.'); return; }
    b.disabled = true;
    say('');
    post('/knock', { name: name, notice: shown }).then(function (res) {
      return res.json().catch(function () { return {}; }).then(function (body) {
        if (res.status === 201) { ticket = body.ticket; $('knock-form').hidden = true; wait(); return; }
        if (res.status === 409 && body.notice) {
          // The notice changed since this page loaded: show it again before
          // the guest may ask.
          renderNotice(body.notice);
          say(body.error || 'The notice changed. Read it again.');
        } else if (res.status === 410) {
          finish('ended', 'This meeting has ended.'); return;
        } else {
          say(body.error || 'That did not work. Try again.');
        }
        b.disabled = false;
      });
    }).catch(function () { say('Could not reach the meeting. Try again.'); b.disabled = false; });
  });

  $('btn-leave').addEventListener('click', function () { finish('left', 'You left the meeting.'); });
})();
