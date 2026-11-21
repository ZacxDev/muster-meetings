// A FAKE of the slice of livekit-client 2.25's browser API that the plugin's
// call.js uses, for the UI specs (e2e/ui-*.spec.ts). A spec serves it in
// place of /static/vendor/livekit-client-2.25.0.umd.js (page.route), so the
// page drives a room with no media server, and the spec drives the room's
// OTHER people through window.__lk: who joins, who mutes or turns the camera
// off, who speaks, each one's connection quality, who shares a screen.
//
// 🔴 What this proves and what it does not. It proves the page draws what
// LiveKit's events say, for the events and fields below (names and payload
// shapes copied from 2.25: RoomEvent.* strings, the (publication,
// participant) arguments, Participant.isMicrophoneEnabled / isCameraEnabled /
// connectionQuality, LocalTrackUnpublished when a shared screen's track ends).
// It does NOT prove LiveKit sends them: e2e/ui-real-livekit.spec.ts runs the
// muting and camera indicators against a real livekit-server.
(function () {
  'use strict';

  function Emitter() { this._h = {}; }
  Emitter.prototype.on = function (e, f) { (this._h[e] = this._h[e] || []).push(f); return this; };
  Emitter.prototype.off = function (e, f) { this._h[e] = (this._h[e] || []).filter(function (x) { return x !== f; }); return this; };
  Emitter.prototype.emit = function (e) {
    var args = Array.prototype.slice.call(arguments, 1);
    (this._h[e] || []).slice().forEach(function (f) { f.apply(null, args); });
  };

  var RoomEvent = {
    ParticipantConnected: 'participantConnected', ParticipantDisconnected: 'participantDisconnected',
    TrackPublished: 'trackPublished', TrackUnpublished: 'trackUnpublished',
    TrackSubscribed: 'trackSubscribed', TrackUnsubscribed: 'trackUnsubscribed',
    TrackMuted: 'trackMuted', TrackUnmuted: 'trackUnmuted',
    LocalTrackPublished: 'localTrackPublished', LocalTrackUnpublished: 'localTrackUnpublished',
    ActiveSpeakersChanged: 'activeSpeakersChanged', ConnectionQualityChanged: 'connectionQualityChanged',
    ParticipantNameChanged: 'participantNameChanged',
    Reconnecting: 'reconnecting', Reconnected: 'reconnected', Disconnected: 'disconnected'
  };
  var Source = { Camera: 'camera', Microphone: 'microphone', ScreenShare: 'screen_share', ScreenShareAudio: 'screen_share_audio', Unknown: 'unknown' };
  var ConnectionQuality = { Excellent: 'excellent', Good: 'good', Poor: 'poor', Lost: 'lost', Unknown: 'unknown' };
  var DisconnectReason = { UNKNOWN_REASON: 0, CLIENT_INITIATED: 1, DUPLICATE_IDENTITY: 2, SERVER_SHUTDOWN: 3, PARTICIPANT_REMOVED: 4, ROOM_DELETED: 5 };

  // A still picture for a video track: one colour per person, so a
  // screenshot is the same every time.
  var HUES = ['#3f6e8c', '#7a4f8c', '#4f8c63', '#8c6a3f', '#8c3f4f'];
  function still(seed, label) {
    var c = document.createElement('canvas');
    c.width = 320; c.height = 180;
    var g = c.getContext('2d');
    g.fillStyle = HUES[seed % HUES.length];
    g.fillRect(0, 0, 320, 180);
    g.fillStyle = 'rgba(255,255,255,.85)';
    g.font = '600 22px sans-serif';
    g.textAlign = 'center';
    g.fillText(label, 160, 98);
    var s = c.captureStream(0);
    // captureStream(0) sends a frame only on request; ask for one now and
    // again shortly (the first can be dropped before a sink attaches).
    var t = s.getVideoTracks()[0];
    [0, 50, 250].forEach(function (ms) { setTimeout(function () { if (t.requestFrame) t.requestFrame(); }, ms); });
    return s;
  }

  var seeds = 0;
  function Track(kind, source, label) {
    this.kind = kind;
    this.source = source;
    this.sid = 'TR_' + Math.random().toString(36).slice(2, 10);
    this._els = [];
    this._stream = kind === 'video' ? still(seeds++, label || '') : null;
  }
  Track.prototype.attach = function () {
    var el = document.createElement(this.kind === 'audio' ? 'audio' : 'video');
    el.autoplay = true;
    if (this._stream) el.srcObject = this._stream;
    this._els.push(el);
    return el;
  };
  Track.prototype.detach = function () { var e = this._els; this._els = []; return e; };
  Track.Source = Source;

  function Publication(track) {
    this.track = track; this.kind = track.kind; this.source = track.source; this.trackSid = track.sid; this.isMuted = false;
  }

  function Participant(identity, name, local) {
    Emitter.call(this);
    this.identity = identity; this.name = name; this.isLocal = !!local;
    this.connectionQuality = ConnectionQuality.Unknown;
    this.audioLevel = 0; this.isSpeaking = false;
    this.trackPublications = new Map();
  }
  Participant.prototype = Object.create(Emitter.prototype);
  Participant.prototype.getTrackPublication = function (s) { return this.trackPublications.get(s); };
  function enabled(p, s) { var pub = p.trackPublications.get(s); return !!pub && !pub.isMuted; }
  Object.defineProperty(Participant.prototype, 'isMicrophoneEnabled', { get: function () { return enabled(this, Source.Microphone); } });
  Object.defineProperty(Participant.prototype, 'isCameraEnabled', { get: function () { return enabled(this, Source.Camera); } });
  Object.defineProperty(Participant.prototype, 'isScreenShareEnabled', { get: function () { return enabled(this, Source.ScreenShare); } });

  var KIND = { camera: 'video', microphone: 'audio', screen_share: 'video', screen_share_audio: 'audio' };

  function LocalParticipant(room) { Participant.call(this, '', '', true); this._room = room; }
  LocalParticipant.prototype = Object.create(Participant.prototype);
  // _set is LiveKit's set*Enabled: the first enable publishes a track;
  // later ones unmute it; a disable mutes a camera or microphone, and
  // unpublishes a shared screen.
  LocalParticipant.prototype._set = function (source, on) {
    var room = this._room, pub = this.trackPublications.get(source);
    window.__lk.calls.push(source + '=' + on);
    if (window.__lk.refuse[source]) {
      var e = new Error('Permission denied'); e.name = window.__lk.refuse[source];
      return Promise.reject(e);
    }
    if (on) {
      if (!pub) {
        pub = new Publication(new Track(KIND[source], source, this.name || 'You'));
        this.trackPublications.set(source, pub);
        room.emit(RoomEvent.LocalTrackPublished, pub, this);
      } else if (pub.isMuted) {
        pub.isMuted = false;
        room.emit(RoomEvent.TrackUnmuted, pub, this);
      }
    } else if (pub) {
      if (source === Source.ScreenShare) {
        this.trackPublications.delete(source);
        room.emit(RoomEvent.LocalTrackUnpublished, pub, this);
      } else if (!pub.isMuted) {
        pub.isMuted = true;
        room.emit(RoomEvent.TrackMuted, pub, this);
      }
    }
    return Promise.resolve(pub);
  };
  LocalParticipant.prototype.setMicrophoneEnabled = function (on) { return this._set(Source.Microphone, on); };
  LocalParticipant.prototype.setCameraEnabled = function (on) { return this._set(Source.Camera, on); };
  LocalParticipant.prototype.setScreenShareEnabled = function (on) { return this._set(Source.ScreenShare, on); };
  LocalParticipant.prototype.enableCameraAndMicrophone = function () {
    return Promise.all([this.setMicrophoneEnabled(true), this.setCameraEnabled(true)]);
  };

  function claims(token) {
    try {
      var p = token.split('.')[1].replace(/-/g, '+').replace(/_/g, '/');
      return JSON.parse(atob(p + '==='.slice((p.length + 3) % 4)));
    } catch (e) { return {}; }
  }

  function Room(options) {
    Emitter.call(this);
    this.options = options || {};
    this.localParticipant = new LocalParticipant(this);
    this.remoteParticipants = new Map();
    this.state = 'disconnected';
    window.__lk.rooms.push(this);
  }
  Room.prototype = Object.create(Emitter.prototype);
  Room.prototype.connect = function (url, token, opts) {
    var self = this, c = claims(token);
    this.url = url; this.token = token; this.connectOptions = opts;
    this.name = (c.video && c.video.room) || '';
    this.localParticipant.identity = c.sub || '';
    this.localParticipant.name = c.name || '';
    if (window.__lk.failConnect) return Promise.reject(new Error('could not establish signal connection'));
    return new Promise(function (resolve) { setTimeout(function () { self.state = 'connected'; resolve(); }, 10); });
  };
  Room.prototype.disconnect = function () {
    if (this.state === 'disconnected') return Promise.resolve();
    this.state = 'disconnected';
    this.emit(RoomEvent.Disconnected, DisconnectReason.CLIENT_INITIATED);
    return Promise.resolve();
  };

  function room() {
    var r = window.__lk.rooms.filter(function (x) { return x.state === 'connected'; });
    if (!r.length) throw new Error('no connected room');
    return r[r.length - 1];
  }
  function remote(id) {
    var p = room().remoteParticipants.get(id);
    if (!p) throw new Error('no participant ' + id);
    return p;
  }
  function who(id) { var r = room(); return id === r.localParticipant.identity ? r.localParticipant : remote(id); }

  function publish(r, p, source, muted) {
    var pub = new Publication(new Track(KIND[source], source, p.name));
    pub.isMuted = !!muted;
    p.trackPublications.set(source, pub);
    r.emit(RoomEvent.TrackPublished, pub, p);
    r.emit(RoomEvent.TrackSubscribed, pub.track, pub, p);
    return pub;
  }

  // The spec's handle on the room's other people.
  window.__lk = {
    rooms: [], calls: [], refuse: {}, failConnect: false,
    room: room,
    // addRemote joins someone: {mic, cam} (default both on), {quality}.
    addRemote: function (id, name, o) {
      o = o || {};
      var r = room(), p = new Participant(id, name, false);
      p.connectionQuality = o.quality || ConnectionQuality.Unknown;
      r.remoteParticipants.set(id, p);
      r.emit(RoomEvent.ParticipantConnected, p);
      publish(r, p, Source.Microphone, o.mic === false);
      publish(r, p, Source.Camera, o.cam === false);
      return id;
    },
    removeRemote: function (id) {
      var r = room(), p = remote(id);
      p.trackPublications.forEach(function (pub) { r.emit(RoomEvent.TrackUnsubscribed, pub.track, pub, p); });
      r.remoteParticipants.delete(id);
      r.emit(RoomEvent.ParticipantDisconnected, p);
    },
    // mute sets a remote track's muted state, as LiveKit's TrackMuted and
    // TrackUnmuted report it.
    mute: function (id, source, muted) {
      var r = room(), p = remote(id), pub = p.trackPublications.get(source);
      pub.isMuted = !!muted;
      r.emit(muted ? RoomEvent.TrackMuted : RoomEvent.TrackUnmuted, pub, p);
    },
    // share starts or stops a remote screen share.
    share: function (id, on) {
      var r = room(), p = remote(id);
      if (on) { publish(r, p, Source.ScreenShare, false); return; }
      var pub = p.trackPublications.get(Source.ScreenShare);
      p.trackPublications.delete(Source.ScreenShare);
      r.emit(RoomEvent.TrackUnsubscribed, pub.track, pub, p);
      r.emit(RoomEvent.TrackUnpublished, pub, p);
    },
    // speakers is LiveKit's ActiveSpeakersChanged: the people speaking now,
    // loudest first, with their audio levels.
    speakers: function (ids) {
      var r = room();
      var ps = ids.map(function (id, i) { var p = who(id); p.audioLevel = 0.9 - i * 0.1; p.isSpeaking = true; return p; });
      r.remoteParticipants.forEach(function (p) { if (ids.indexOf(p.identity) < 0) { p.audioLevel = 0; p.isSpeaking = false; } });
      r.emit(RoomEvent.ActiveSpeakersChanged, ps);
    },
    quality: function (id, q) {
      var r = room(), p = who(id);
      p.connectionQuality = q;
      r.emit(RoomEvent.ConnectionQualityChanged, q, p);
    },
    // endLocalShare is the browser's own "Stop sharing": the screen track
    // ends, and LiveKit unpublishes it (LocalParticipant's handler of the
    // track's ended event).
    endLocalShare: function () {
      var r = room(), lp = r.localParticipant, pub = lp.trackPublications.get(Source.ScreenShare);
      if (!pub) throw new Error('not sharing');
      lp.trackPublications.delete(Source.ScreenShare);
      r.emit(RoomEvent.LocalTrackUnpublished, pub, lp);
    },
    // kick ends the call from LiveKit's side with a reason.
    kick: function (reason) { var r = room(); r.state = 'disconnected'; r.emit(RoomEvent.Disconnected, DisconnectReason[reason]); }
  };

  window.LivekitClient = { Room: Room, RoomEvent: RoomEvent, Track: Track, ConnectionQuality: ConnectionQuality, DisconnectReason: DisconnectReason };
})();
