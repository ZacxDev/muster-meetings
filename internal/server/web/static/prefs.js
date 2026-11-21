// The owner's remembered device check: once they have finished it, their
// camera and microphone (which devices, and on or off) are kept in this
// origin's localStorage under ONE key, so New meeting goes straight into the
// call afterwards. Guests never use it (they always get the check).
//
// 🔴 This is the ONLY script that touches storage, and it stores only these
// five device fields: no credential, no name, no link
// (internal/server TestOnlyPrefsTouchesStorage). Storage that throws (blocked
// cookies, a sandbox) is "nothing remembered": the check is shown.
(function () {
  'use strict';
  var KEY = 'meetings.devices.v1';

  function load() {
    try {
      var v = JSON.parse(window.localStorage.getItem(KEY) || 'null');
      if (!v || v.v !== 1) return null;
      return { mic: !!v.mic, cam: !!v.cam, micId: String(v.micId || ''), camId: String(v.camId || '') };
    } catch (e) { return null; }
  }

  function save(c) {
    try {
      window.localStorage.setItem(KEY, JSON.stringify({ v: 1, mic: !!c.mic, cam: !!c.cam, micId: String(c.micId || ''), camId: String(c.camId || '') }));
      return true;
    } catch (e) { return false; }
  }

  window.MeetingsPrefs = { KEY: KEY, load: load, save: save };
})();
