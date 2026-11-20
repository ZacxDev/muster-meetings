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

  // linkLive: the server still gives this meeting's link (open, under 24 h).
  function linkLive(m) { return m.state === 'open' && new Date(m.link_expires_at) > new Date(); }

  // copyText copies, or falls back to selecting the link panel's field.
  function copyText(text) {
    var input = $('join-url');
    var fallback = function () { input.select(); say('Press Ctrl+C (or ⌘C) to copy the link.'); };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      return navigator.clipboard.writeText(text).then(function () { say('Link copied.'); }, fallback);
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
    if (linkLive(m)) {
      var actions = document.createElement('div');
      actions.className = 'actions';
      var copy = document.createElement('button');
      copy.type = 'button';
      copy.className = 'copy-row';
      copy.textContent = 'Copy link';
      copy.addEventListener('click', function () {
        say('');
        linkOf(m.id).then(function (url) { showLink(url); return copyText(url); })
          .catch(function (e) { if (token) say('Could not get the link: ' + e.message); });
      });
      actions.appendChild(copy);
      if (navigator.share) {
        var share = document.createElement('button');
        share.type = 'button';
        share.className = 'share-row';
        share.textContent = 'Share';
        share.addEventListener('click', function () {
          linkOf(m.id).then(function (url) { return navigator.share({ title: 'Join my meeting', url: url }); })
            .catch(function () { /* dismissed or expired */ });
        });
        actions.appendChild(share);
      }
      li.appendChild(actions);
    }
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

  $('copy-link').addEventListener('click', function () { copyText($('join-url').value); });

  $('share-link').addEventListener('click', function () {
    navigator.share({ title: 'Join my meeting', url: $('join-url').value }).catch(function () { /* dismissed */ });
  });

  more.addEventListener('click', function () { load(true); });

  load(false);
  // Opened for one meeting (a notification's tap): show its link at once,
  // while it has one.
  if (ref) {
    linkOf(ref).then(showLink).catch(function () { /* ended: the list says so */ });
  }
})();
