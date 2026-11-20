// The owner app, inside muster's frame. The session token lives in this
// script's memory only: it is read from the page once, the attribute is
// removed, and it is never written to storage or a cookie. A reload finds no
// token and the server says "Open Meetings from muster again".
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

  function say(text) { status.textContent = text || ''; }

  function ended() {
    token = null;
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

  function api(method, path, body) {
    if (!token) return Promise.reject(new Error('no session'));
    var init = { method: method, headers: { 'Authorization': 'Bearer ' + token }, cache: 'no-store', credentials: 'omit' };
    if (body !== undefined) {
      init.headers['Content-Type'] = 'application/json';
      init.body = JSON.stringify(body);
    }
    return fetch(path, init).then(function (res) {
      if (res.status === 401) { ended(); throw new Error('session ended'); }
      return res.json().catch(function () { return {}; }).then(function (b) {
        if (!res.ok) throw new Error(b.error || ('the server answered ' + res.status));
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
    li.appendChild(t); li.appendChild(meta);
    return li;
  }

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

  $('new-meeting').addEventListener('click', function () {
    var btn = this;
    btn.disabled = true;
    say('');
    api('POST', '/api/meetings', {}).then(function (b) {
      showLink(b.join_url);
      list.insertBefore(row(b.meeting), list.firstChild);
      empty.hidden = true;
    }).catch(function (e) { if (token) say('Could not make a meeting: ' + e.message); })
      .then(function () { btn.disabled = false; });
  });

  $('copy-link').addEventListener('click', function () {
    var input = $('join-url');
    var done = function () { say('Link copied.'); };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(input.value).then(done, function () { input.select(); say('Press Ctrl+C (or ⌘C) to copy the link.'); });
    } else {
      input.select();
      say('Press Ctrl+C (or ⌘C) to copy the link.');
    }
  });

  $('share-link').addEventListener('click', function () {
    navigator.share({ title: 'Join my meeting', url: $('join-url').value }).catch(function () { /* dismissed */ });
  });

  more.addEventListener('click', function () { load(true); });

  load(false);
})();
