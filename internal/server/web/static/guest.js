// The guest page: the notice, the device check and a name, Ask to join, the
// wait, the call. The ticket the knock answers lives in this script's memory
// only and is sent in request bodies, never in a URL. The page's state is on
// <main data-state>: start, waiting, admitted, denied, ended, removed.
(function () {
  'use strict';
  var root = document.getElementById('guest');
  if (!root) return;
  var UI = window.MeetingsUI;
  var $ = function (id) { return document.getElementById(id); };
  var base = location.pathname.replace(/\/+$/, '');
  var shown = { record: root.getAttribute('data-record') === 'true', ai: root.getAttribute('data-ai') === 'true',
    transcribed: root.getAttribute('data-transcribed') === 'true' };
  var ticket = null;
  var conn = null;
  var abort = null;
  var media = false; // what the call publishes, from the pre-join

  function state(s) { root.setAttribute('data-state', s); }
  function say(text) { $('pre-status').textContent = text || ''; }
  function callSay(text) { $('call-status').textContent = text || ''; }

  function renderNotice(n) {
    var box = $('notice');
    box.textContent = '';
    (n.text || []).forEach(function (t) {
      var p = document.createElement('p');
      p.textContent = t;
      box.appendChild(p);
    });
    box.hidden = !(n.text || []).length;
    shown = { record: !!n.record, ai: !!n.ai, transcribed: !!n.transcribed };
  }

  function post(path, body, signal) {
    var init = { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body), cache: 'no-store', credentials: 'omit' };
    if (signal) init.signal = signal;
    // The card's routes are the origin's own (/card), not the link's: the
    // link answers 410 once the meeting has ended.
    return fetch((path.indexOf('/card') === 0 ? '' : base) + path, init);
  }

  // The pre-join check starts at once, EVERY time (a guest's page never
  // remembers one); Ask to join waits for it (granted, refused, or the
  // browser's prompt unanswered for a few seconds: the guest then joins
  // without a camera and microphone).
  var asked = false;
  var pre = MeetingsCall.prejoin({ root: $('join-view'), remember: false, name: function () { return $('name').value.trim(); }, onState: function (s) {
    $('ask').disabled = asked || s === 'checking' || s === 'idle';
  } });
  $('name').addEventListener('input', function () { pre.refresh(); });
  pre.start();

  function finish(s, text, card) {
    state(s);
    pre.stop();
    if (abort) { abort.abort(); abort = null; }
    if (conn) { conn.leave(); conn = null; }
    ticket = null;
    document.body.textContent = '';
    var m = document.createElement('main');
    m.className = 'page notice';
    m.id = 'guest';
    m.setAttribute('data-state', s);
    var h = document.createElement('h1');
    h.textContent = 'Meetings';
    var p = document.createElement('p');
    p.id = 'message';
    p.textContent = text;
    m.appendChild(h); m.appendChild(p);
    document.body.appendChild(m);
    if (card) showCard(m, card);
  }

  // The contacts card (muster's docs/design/contacts.md, "The guest card"):
  // offered by the server only when contacts is configured and accepts this
  // meeting's owner, once per guest per meeting, after End or the guest's
  // own Leave. Optional: Skip is as easy as Send, closing the tab is a
  // skip, nothing is pre-ticked. Its id lives in this function's memory
  // only, and is sent in a body, never a URL.
  // With notes (the server mails meeting notes, and says so in the card),
  // the consent also says the address gets them.
  function cardText(owner, notes) {
    var who = owner || 'the host';
    return {
      title: 'Stay in touch with ' + who + '?',
      consent: notes
        ? 'Your name and email go to ' + who + ', who will see that you were in this meeting, and you get the meeting notes when ' + who + ' sends them. Nothing else is shared.'
        : 'Your name and email go to ' + who + ', who will see that you were in this meeting. Nothing else is shared.'
    };
  }

  function el(tag, cls, text) {
    var x = document.createElement(tag);
    if (cls) x.className = cls;
    if (text != null) x.textContent = text;
    return x;
  }

  function field(id, label, input) {
    var f = el('div', 'mu-field');
    var l = el('label', 'mu-label', label);
    l.setAttribute('for', id);
    input.id = id;
    input.className = 'mu-input';
    var err = el('p', 'mu-error card__error');
    err.id = id + '-error';
    err.hidden = true;
    input.setAttribute('aria-describedby', err.id);
    f.appendChild(l); f.appendChild(input); f.appendChild(err);
    return { box: f, input: input, err: err };
  }

  function showCard(page, card) {
    if (document.getElementById('card')) return;
    var t = cardText(card.owner, !!card.notes);
    var sec = el('section', 'mu-card card-ask');
    sec.id = 'card';
    sec.setAttribute('aria-labelledby', 'card-title');
    var head = el('div', 'card-ask__head');
    var ic = el('span', 'card-ask__icon');
    ic.setAttribute('data-icon', 'people');
    ic.setAttribute('aria-hidden', 'true');
    var h = el('h2', null, t.title);
    h.id = 'card-title';
    head.appendChild(ic); head.appendChild(h);
    var consent = el('p', 'card-ask__consent', t.consent);
    consent.id = 'card-consent';
    var form = el('form', 'card-ask__form');
    form.id = 'card-form';
    form.noValidate = true;
    var nameIn = document.createElement('input');
    nameIn.type = 'text'; nameIn.maxLength = 40; nameIn.autocomplete = 'name'; nameIn.required = true;
    nameIn.value = card.name || '';
    var emailIn = document.createElement('input');
    emailIn.type = 'email'; emailIn.maxLength = 254; emailIn.autocomplete = 'email'; emailIn.required = true;
    emailIn.setAttribute('inputmode', 'email'); emailIn.setAttribute('autocapitalize', 'off'); emailIn.spellcheck = false;
    var fn = field('card-name', 'Name', nameIn), fe = field('card-email', 'Email', emailIn);
    var status = el('p', 'status card-ask__status');
    status.id = 'card-status';
    status.setAttribute('role', 'status');
    status.setAttribute('aria-live', 'polite');
    var actions = el('div', 'card-ask__actions');
    var send = el('button', 'mu-btn mu-btn--primary', 'Send');
    send.type = 'submit'; send.id = 'card-send';
    send.setAttribute('data-icon', 'send');
    var skip = el('button', 'mu-btn mu-btn--ghost', 'Skip');
    skip.type = 'button'; skip.id = 'card-skip';
    actions.appendChild(send); actions.appendChild(skip);
    form.appendChild(fn.box); form.appendChild(fe.box); form.appendChild(status); form.appendChild(actions);
    sec.appendChild(head); sec.appendChild(consent); sec.appendChild(form);
    page.appendChild(sec);
    if (UI && UI.decorate) UI.decorate(sec);
    var id = card.id;
    var busy = false;

    function bad(f, msg) {
      f.input.setAttribute('aria-invalid', 'true');
      f.err.textContent = msg;
      f.err.hidden = false;
    }
    function clear() {
      [fn, fe].forEach(function (f) { f.input.removeAttribute('aria-invalid'); f.err.hidden = true; f.err.textContent = ''; });
      status.textContent = '';
    }
    function done(text) {
      id = null;
      sec.setAttribute('data-done', 'true');
      sec.textContent = '';
      var p = el('p', 'card-ask__done');
      p.id = 'card-done';
      p.setAttribute('role', 'status');
      var ok = el('span', 'card-ask__icon');
      ok.setAttribute('data-icon', 'check');
      ok.setAttribute('aria-hidden', 'true');
      p.appendChild(ok);
      p.appendChild(el('span', null, text));
      sec.appendChild(p);
      if (UI && UI.decorate) UI.decorate(sec);
    }
    function gone() {
      id = null;
      sec.setAttribute('data-leaving', 'true');
      var dur = parseFloat(getComputedStyle(sec).transitionDuration) || 0;
      setTimeout(function () { if (sec.parentNode) sec.parentNode.removeChild(sec); }, dur * 1000);
    }

    form.addEventListener('submit', function (ev) {
      ev.preventDefault();
      if (busy || !id) return;
      clear();
      var n = nameIn.value.trim(), e = emailIn.value.trim();
      if (!n) { bad(fn, 'Type your name.'); nameIn.focus(); return; }
      if (!e || e.indexOf('@') < 1) { bad(fe, 'Type one email address, like name@example.com.'); emailIn.focus(); return; }
      busy = true; send.disabled = true; skip.disabled = true;
      post('/card', { id: id, name: n, email: e }).then(function (res) {
        return res.json().catch(function () { return {}; }).then(function (b) {
          if (res.status === 200) { done(b.message || 'Sent.'); return; }
          if (res.status === 400 && b.field === 'name') { bad(fn, b.error); nameIn.focus(); }
          else if (res.status === 400 && b.field === 'email') { bad(fe, b.error); emailIn.focus(); }
          else if (res.status === 404) { done(b.error || 'This form has expired.'); return; }
          else status.textContent = b.error || 'That did not work. Try again.';
        });
      }).catch(function () { status.textContent = 'Could not reach the meeting. Try again.'; })
        .then(function () { busy = false; send.disabled = false; skip.disabled = false; });
    });
    skip.addEventListener('click', function () {
      if (busy || !id) return;
      var x = id;
      gone();
      post('/card/skip', { id: x }).catch(function () { /* closing is a skip too */ });
    });
  }

  // needToken: the page wants the next token /events sends (a Retry).
  var needToken = false;

  function connect(t) {
    var r = $('btn-retry');
    r.hidden = true;
    MeetingsCall.join({ url: t.livekit_url, token: t.token, ice: t.ice, media: media, onStatus: callSay,
      onGone: function () {
        conn = null;
        if (root.getAttribute('data-state') === 'admitted') { callSay('The call disconnected.'); r.hidden = false; }
      } })
      .then(function (c) { conn = c; }, function () { r.hidden = false; /* the status says "Can't connect media" */ });
  }

  function onAdmitted(t) {
    state('admitted');
    media = pre.choice();
    pre.stop();
    $('join-view').hidden = true;
    $('call-view').hidden = false;
    events();
    connect(t);
  }

  // The live transcript, the same as the owner's panel. Each stream sends it
  // from the start, so a new stream (a Retry) redraws it.
  var side = MeetingsCall.Side();
  var transcript = MeetingsCall.transcriptPanel(side);
  var actionItems = MeetingsCall.itemsPanel(side);

  // events follows the admitted guest's stream: a fresh token at its start
  // (used on a Retry), the CURRENT notice (its badge), the live transcript,
  // and the end or a removal.
  function events() {
    if (abort) abort.abort();
    abort = new AbortController();
    transcript.reset();
    actionItems.reset();
    post('/events', { ticket: ticket }, abort.signal).then(function (res) {
      if (res.status === 410) { finish('ended', 'This meeting has ended.'); return; }
      if (!res.ok) { finish('removed', 'You are no longer in this meeting.'); return; }
      return MeetingsCall.readEvents(res, function (name, data) {
        if (name === 'token') { if (needToken) { needToken = false; connect(data); } }
        else if (name === 'meeting') MeetingsCall.showBadge(data.notice);
        else if (name === 'segment') transcript.segment(data);
        else if (name === 'amend') transcript.amend(data);
        else if (name === 'partials') transcript.partials(data.partials);
        else if (name === 'transcript') transcript.status(data.status);
        else if (name === 'items') actionItems.update(data);
        else if (name === 'ended') finish('ended', 'This meeting has ended.', data && data.card);
        else if (name === 'removed') finish('removed', 'You are no longer in this meeting.');
      });
    }).catch(function () { /* left, replaced by a Retry, or the network: the call's own status says so */ });
  }

  // Retry (design: "Can't connect media" with a retry): a fresh token from
  // /events, and the call again, without knocking: the guest is still
  // admitted until the owner removes them, they leave, or the meeting ends.
  $('btn-retry').addEventListener('click', function () {
    this.hidden = true;
    if (conn) { conn.leave(); conn = null; }
    callSay('Connecting…');
    needToken = true;
    events();
  });

  function wait() {
    state('waiting');
    var who = root.getAttribute('data-owner');
    $('waiting').hidden = false;
    $('guest-status').textContent = who ? 'Waiting for ' + who + ' to let you in.' : 'Waiting for the host to let you in.';
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
    var input = $('name');
    var name = input.value.trim();
    if (!name) { input.setAttribute('aria-invalid', 'true'); say('Type your name first.'); input.focus(); return; }
    input.removeAttribute('aria-invalid');
    asked = true;
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
        asked = false;
        b.disabled = pre.state() === 'checking';
      });
    }).catch(function () { say('Could not reach the meeting. Try again.'); asked = false; b.disabled = false; });
  });

  $('btn-leave').addEventListener('click', function () {
    var t = ticket;
    finish('left', 'You left the meeting.');
    // The Leave answer is today's 204, or, with contacts configured, a 200
    // carrying the card.
    if (t) post('/leave', { ticket: t }).then(function (res) {
      if (res.status !== 200) return;
      return res.json().then(function (b) { if (b && b.card) showCard(document.getElementById('guest'), b.card); });
    }).catch(function () { /* gone already */ });
  });
})();
