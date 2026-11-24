package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/frames"
	"github.com/ZacxDev/muster-meetings/internal/livekit"
	"github.com/ZacxDev/muster-meetings/internal/lobby"
	"github.com/ZacxDev/muster-meetings/internal/s3"
	"github.com/ZacxDev/muster-meetings/internal/store"
	"github.com/ZacxDev/muster-meetings/internal/timeline"
)

// The recording (design: phase 5; "Media: LiveKit, TURN, Egress",
// "Retention", "Failure modes"), and the time-aligned bundle the post-call
// analysis reads (the operator's decision 4).
//
//   - When the first guest is admitted to a meeting with Record on, the
//     plugin starts a room composite egress (LiveKit's grid layout, MP4) whose
//     file Egress uploads to the bucket with its OWN S3 configuration. The
//     part's start in meeting time is the egress's own report of when its
//     file began (FileInfo.started_at), never when the webhook arrived.
//   - A start LiveKit refuses is retried with the track egress's backoff
//     (1, 2, 4 … 30 s); an egress that ends while the meeting is live and
//     Record is on (it died) is restarted the same way as a new PART. The
//     stretch in between is a recording gap, and while it is open after a
//     failure everyone's badge says "Recording failed".
//   - Record off stops the egress; End meeting stops it (and every other end
//     deletes the room, which ends it).
//   - The owner plays it in the archive (signed /media URLs), and may mint a
//     share link (/r/<token>) that plays it with no muster sign-in until the
//     recording's purge time. An hourly sweeper deletes each part's object,
//     then its row.
//
// 🔴 With no bucket configured (Config.S3 nil) none of this exists: no
// egress, no route, no sweeper, no screen-share rows, and every answer is
// what it was before the recording (nourl_test.go's goldens).

// Recording says the plugin records (it has a bucket).
func (s *Server) Recording() bool { return s.cfg.S3 != nil }

// recorder is the recording's in-memory side: each meeting's start loop, and
// its egress restarts since its last healthy part.
type recorder struct {
	mu       sync.Mutex
	runs     map[string]*run
	restarts map[string]int
	// frames bounds the ffmpeg runs at once.
	frames chan struct{}
}

func newRecorder() *recorder {
	// 🔴 One ffmpeg at a time (frames.go: one decode thread, ~one frame's
	// buffers): two 1080p runs measured ~198 MiB each without -threads, beside
	// a plugin whose memory limit is 256 MiB.
	return &recorder{runs: map[string]*run{}, restarts: map[string]int{}, frames: make(chan struct{}, 1)}
}

// noticeOf is the notice of a meeting for the streams: noticeFor, with the
// badge saying "Recording failed" while the recording should run and does
// not (store.RecordingStatus). Without a bucket it is noticeFor exactly.
func (s *Server) noticeOf(ctx context.Context, meetingID string, record, ai bool) lobby.Notice {
	n := s.noticeFor(record, ai)
	if !s.Recording() || !record {
		return n
	}
	if st, err := s.store.RecordingStatus(ctx, meetingID); err == nil && st == store.RecStatusFailed {
		return n.WithRecordingFailed()
	}
	return n
}

// ---- starting and stopping ----

// startRecording runs the meeting's start loop (replacing one running).
func (s *Server) startRecording(meetingID string) { s.startRecordingAfter(meetingID, 0) }

func (s *Server) startRecordingAfter(meetingID string, wait time.Duration) {
	if !s.Recording() {
		return
	}
	ctx, cancel := context.WithCancel(s.bg)
	me := &run{cancel: cancel}
	s.rec.mu.Lock()
	if r := s.rec.runs[meetingID]; r != nil {
		r.cancel()
	}
	s.rec.runs[meetingID] = me
	s.rec.mu.Unlock()
	go func() {
		if wait > 0 {
			select {
			case <-ctx.Done():
			case <-s.after(wait):
			}
		}
		if ctx.Err() == nil {
			s.recordLoop(ctx, meetingID)
		}
		cancel()
		s.rec.mu.Lock()
		if s.rec.runs[meetingID] == me {
			delete(s.rec.runs, meetingID)
		}
		s.rec.mu.Unlock()
	}()
}

func (s *Server) cancelRecording(meetingID string) {
	s.rec.mu.Lock()
	if r := s.rec.runs[meetingID]; r != nil {
		r.cancel()
		delete(s.rec.runs, meetingID)
	}
	s.rec.mu.Unlock()
}

// recordLoop makes the meeting's next part and asks LiveKit for its egress,
// retrying a refusal with backoff until one is accepted, or the meeting is no
// longer live with Record on, or a part is already running
// (store.BeginRecording decides, in one transaction).
func (s *Server) recordLoop(ctx context.Context, meetingID string) {
	backoff := s.cfg.EgressBackoff
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return
		}
		start, err := s.store.MeetingStart(ctx, meetingID)
		if err != nil {
			return
		}
		p, err := s.store.BeginRecording(ctx, meetingID, s.sinceStart(start), s.cfg.Now())
		if err != nil {
			if !errors.Is(err, store.ErrRecordingRunning) && !errors.Is(err, store.ErrNotRecorded) {
				s.cfg.Log.Printf("recording: meeting %s: %v", meetingID, err)
			}
			return
		}
		s.hub.Changed(meetingID)
		// 🔴 The start is never cancelled half-way (Record off cancels this
		// loop): LiveKit may start the egress whatever happens to our side of
		// the request, so its answer is always read and the egress stored,
		// and then the meeting is asked again whether it is still wanted. Off
		// or an end that landed while the start was in flight stops it here;
		// one that lands after this is stopped by stopRecording (it now finds
		// the egress), and the reconcile stops anything both missed.
		info, err := s.cfg.LiveKit.StartRoomCompositeEgress(context.WithoutCancel(ctx), store.RoomName(meetingID), p.ObjectKey)
		if err == nil {
			if err := s.store.SetRecordingEgress(context.WithoutCancel(ctx), p.ID, info.EgressID); err != nil {
				s.cfg.Log.Printf("recording: meeting %s part %d: storing its egress: %v", meetingID, p.Part, err)
			}
			s.cfg.Log.Printf("recording: meeting %s part %d: egress started", meetingID, p.Part)
			if ref, err := s.store.RecordingByEgress(context.WithoutCancel(ctx), info.EgressID, s.cfg.Now()); err == nil && !wanted(ref) {
				s.stopUnwanted(context.WithoutCancel(ctx), ref, info.EgressID)
			}
			return
		}
		// The part is failed (its gap stays open); the next attempt is a new
		// part, so a start LiveKit did run after all is told apart by its key.
		_ = s.store.RecordingStartRefused(context.WithoutCancel(ctx), p.ID, lkKind(err), s.cfg.Now())
		s.hub.Changed(meetingID)
		if ctx.Err() != nil {
			return
		}
		wait := backoff[min(attempt, len(backoff)-1)]
		s.cfg.Log.Printf("recording: meeting %s part %d: starting the egress failed (%s); retrying in %s", meetingID, p.Part, lkKind(err), wait)
		select {
		case <-ctx.Done():
			return
		case <-s.after(wait):
		}
	}
}

// stopRecording stops the meeting's recording on purpose (Record off, End
// meeting): no restart, its current egress stopped. atMS, when set, closes
// an open recording gap there (Record off: nothing is meant to be recorded
// from then on; an end closes it at the end itself, store.closeEnded).
func (s *Server) stopRecording(ctx context.Context, meetingID string, atMS *int64) {
	if !s.Recording() {
		return
	}
	s.cancelRecording(meetingID)
	// Every egress of the meeting whose end has not been seen: the current
	// part's, and any whose start had failed and that LiveKit ran anyway. One
	// whose stop fails is stopped by the reconcile (onRecordingEgress).
	if live, err := s.store.LiveEgressesOf(ctx, meetingID); err == nil {
		for _, p := range live {
			if err := s.cfg.LiveKit.StopEgress(ctx, p.EgressID); err != nil {
				s.cfg.Log.Printf("recording: meeting %s part %d: stopping its egress: %s", meetingID, p.Part, lkKind(err))
			}
		}
	}
	if atMS != nil {
		_ = s.store.CloseRecordingGap(ctx, meetingID, *atMS)
	}
	s.rec.mu.Lock()
	delete(s.rec.restarts, meetingID)
	s.rec.mu.Unlock()
	s.hub.Changed(meetingID)
}

// resumeRecording is the start of a restarted plugin: a room composite does
// not depend on the plugin (it records LiveKit's room), so a running part
// goes on and its webhooks arrive; only a meeting that was WAITING to retry
// (a gap open, no part running) has lost its loop, and it is started again.
func (s *Server) resumeRecording(ctx context.Context) {
	if !s.Recording() {
		return
	}
	ids, err := s.store.ToResume(ctx, s.cfg.Now())
	if err != nil {
		s.cfg.Log.Printf("recording: resuming: %v", err)
		return
	}
	for _, id := range ids {
		s.startRecording(id)
	}
}

// ---- the egress's webhooks ----

// fileOf is the egress's first file result that says when it began.
func fileOf(info livekit.EgressInfo) *livekit.FileInfo {
	for i := range info.Files {
		if info.Files[i].StartedAt > 0 {
			return &info.Files[i]
		}
	}
	return nil
}

// wanted is THE rule for a part's egress: it may run only while its meeting
// is live with Record on. Every path that sees an egress (its start, its
// webhooks, the reconcile) asks it.
func wanted(ref store.RecordingRef) bool { return ref.MeetingLive && ref.Record }

// stopUnwanted stops an egress its meeting no longer wants (Record off, or
// the meeting over). A stop that fails is logged; the next webhook or
// reconcile pass of the still-running egress asks again, until it ends.
func (s *Server) stopUnwanted(ctx context.Context, ref store.RecordingRef, egressID string) {
	if err := s.cfg.LiveKit.StopEgress(ctx, egressID); err != nil {
		s.cfg.Log.Printf("recording: meeting %s part %d: stopping an egress the meeting no longer wants: %s (the reconcile retries)", ref.MeetingID, ref.Part, lkKind(err))
		return
	}
	s.cfg.Log.Printf("recording: meeting %s part %d: stopped an egress the meeting no longer wants", ref.MeetingID, ref.Part)
}

// onRecordingEgress acts on an egress webhook (or a reconcile's ListEgress
// answer) if the egress records a part. It answers owned (a part's egress,
// acted on), or not owned (no part's: a track egress, or none of the
// plugin's), or an error when the database could not say, and then NOTHING
// is decided about the egress (🔴 above all, it is not stopped as unowned).
// A part is found by its egress id, else by its file's key (a start that
// LiveKit timed out at but Egress ran: its key names the part it was asked
// for).
func (s *Server) onRecordingEgress(ctx context.Context, info livekit.EgressInfo) (owned bool, err error) {
	if !s.Recording() {
		return false, nil
	}
	now := s.cfg.Now()
	if testHookRecordingLookup != nil {
		if err := testHookRecordingLookup(); err != nil {
			return false, err
		}
	}
	ref, err := s.store.RecordingByEgress(ctx, info.EgressID, now)
	if errors.Is(err, store.ErrNotFound) && len(info.Files) > 0 && info.Files[0].Filename != "" {
		ref, err = s.store.RecordingByKey(ctx, info.Files[0].Filename, now)
		if err == nil && ref.EgressID != "" && ref.EgressID != info.EgressID {
			return true, nil // the key is another egress's part: not this one's
		}
		if err == nil && ref.EgressID == "" {
			_ = s.store.SetRecordingEgress(ctx, ref.ID, info.EgressID)
			ref.EgressID = info.EgressID
		}
	}
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		s.cfg.Log.Printf("recording: an egress's part could not be read: %v", err)
		return false, err
	}
	switch {
	case info.Ended():
		s.onRecordingEnded(ctx, ref, info)
		return true, nil
	case fileOf(info) != nil && ref.State != store.RecFailed:
		// (A failed part's egress beginning is no part of the recording:
		// it closes no gap.)
		s.onRecordingBegan(ctx, ref, info)
	}
	// 🔴 A running egress its meeting no longer wants is stopped, on every
	// report of it (a webhook, a reconcile pass): a stop that failed once is
	// tried again until its end is seen. A part marked failed (its start was
	// refused, or timed out while LiveKit ran it anyway) is never the
	// recording, so its egress is never wanted. One whose stop LiveKit has
	// accepted (ENDING: it is uploading) is left to end.
	if (ref.State == store.RecFailed || !wanted(ref)) && info.Status != "EGRESS_ENDING" {
		s.stopUnwanted(ctx, ref, info.EgressID)
	}
	return true, nil
}

// testHookRecordingLookup, when set (by tests only), fails the part lookup
// of onRecordingEgress: a database that cannot answer.
var testHookRecordingLookup func() error

// onRecordingBegan records the part's file start, in meeting time, from the
// egress's OWN report (its file's started_at): the badge's gap closes there.
func (s *Server) onRecordingBegan(ctx context.Context, ref store.RecordingRef, info livekit.EgressInfo) {
	f := fileOf(info)
	if f == nil || ref.StartMS != nil {
		return
	}
	at := timeline.MeetingMS(f.StartedAt, ref.MeetingStart.UnixMilli())
	if _, err := s.store.RecordingBegan(ctx, ref.ID, at); err != nil {
		s.cfg.Log.Printf("recording: meeting %s part %d: %v", ref.MeetingID, ref.Part, err)
		return
	}
	s.cfg.Log.Printf("recording: meeting %s part %d: its file began at meeting time %d ms", ref.MeetingID, ref.Part, at)
	s.hub.Changed(ref.MeetingID)
}

// onRecordingEnded records the part's end: its file's start, length and
// size as the egress reported them. 🔴 A part that ends while its meeting is
// live with Record on was not ended by the plugin (Record off and every end
// make that false first): it died, so a gap opens where its file ended and
// a new part is started on the backoff.
func (s *Server) onRecordingEnded(ctx context.Context, ref store.RecordingRef, info livekit.EgressInfo) {
	e := store.RecordingEnd{State: store.RecComplete}
	if info.Status != "EGRESS_COMPLETE" {
		e.State = store.RecFailed
		e.Error = info.Status // a fixed enum name; never Egress's message
	}
	if f := fileOf(info); f != nil {
		st := timeline.MeetingMS(f.StartedAt, ref.MeetingStart.UnixMilli())
		e.StartMS = &st
		if f.Duration > 0 {
			d := f.Duration / 1_000_000
			e.DurationMS = &d
		}
		if f.Size > 0 {
			b := f.Size
			e.Bytes = &b
		}
	}
	want := wanted(ref)
	if want {
		at := s.sinceStart(ref.MeetingStart)
		if e.StartMS != nil && e.DurationMS != nil {
			at = *e.StartMS + *e.DurationMS
		}
		e.GapAtMS = &at
	}
	moved, err := s.store.EndRecording(ctx, ref.ID, e, s.cfg.Now())
	if err != nil {
		s.cfg.Log.Printf("recording: meeting %s part %d: storing its end: %v", ref.MeetingID, ref.Part, err)
		return
	}
	// 🔴 Its egress has ended (EndRecording records it, whatever the part's
	// state): if the recording was deleted (Delete now, or its purge time)
	// while this egress still ran, its object, now final, is deleted at once.
	s.purgeMeeting(ctx, ref.MeetingID)
	// 🔴 A duplicate or late end (the part had already ended) changes
	// nothing else: no restart, which would cancel a start in flight.
	if !moved {
		return
	}
	s.cfg.Log.Printf("recording: meeting %s part %d ended (%s)", ref.MeetingID, ref.Part, info.Status)
	s.hub.Changed(ref.MeetingID)
	if want {
		wait := s.recordingRestartWait(ref.MeetingID, e.DurationMS)
		s.cfg.Log.Printf("recording: meeting %s: its egress ended while the meeting is recorded; a new part in %s", ref.MeetingID, wait)
		s.startRecordingAfter(ref.MeetingID, wait)
	}
}

// recordingRestartWait is the backoff's step for the meeting's restarts
// since its last healthy part (one that ran HealthyStream or longer).
func (s *Server) recordingRestartWait(meetingID string, ran *int64) time.Duration {
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	if ran != nil && time.Duration(*ran)*time.Millisecond >= HealthyStream {
		s.rec.restarts[meetingID] = 0
	}
	n := s.rec.restarts[meetingID]
	s.rec.restarts[meetingID] = n + 1
	b := s.cfg.EgressBackoff
	return b[min(n, len(b)-1)]
}

// LostEgress is how long a part may run with LiveKit knowing nothing of its
// egress (its Redis restarted, say) before the reconcile calls it failed.
const LostEgress = 2 * time.Minute

// reconcileRecordings asks LiveKit about every part whose egress's end has
// not been seen (a webhook can be lost; a start's answer can be lost), and
// acts as the webhooks would have:
//
//   - a part with an egress id: its egress is reported (onRecordingEgress:
//     ended, began, or stopped while unwanted, again every pass until its
//     end is seen); one LiveKit has not listed for LostEgress, counted from
//     the first pass that missed it, is gone.
//   - a part with NO egress id (its start timed out, or the plugin stopped
//     mid-start): LiveKit's egresses in the meeting's room are listed, and
//     one whose file is the part's key is adopted by it (and so stopped if
//     unwanted); any other composite there writing under meetings/ that no
//     part owns is stopped, so nothing records or uploads unowned. A part
//     none matches for LostEgress (and while the room lists no unidentified
//     egress that names no file yet) has no egress: it is resolved (failed,
//     its egress ended), so a deleted recording's rows can go, and a meeting
//     still recorded starts its next part.
//
// A list LiveKit will not answer skips that part or room, never the pass;
// an egress whose part the database cannot read is left as it is.
func (s *Server) reconcileRecordings(ctx context.Context) {
	if !s.Recording() {
		return
	}
	parts, err := s.store.RunningRecordings(ctx)
	if err != nil {
		return
	}
	now := s.cfg.Now()
	// 🔴 lost is LostEgress after the first pass that could not see the
	// egress (cleared when a pass sees it again), never after its request:
	// one empty answer (LiveKit's Redis restarting) does not end an egress
	// that has run for an hour.
	lost := func(p store.OpenPart, seen bool) bool {
		since, err := s.store.SeenEgress(ctx, p.ID, seen, now)
		return err == nil && !seen && now.Sub(since) >= LostEgress
	}
	unowned := map[string][]store.OpenPart{}
	var order []string
	for _, p := range parts {
		if p.EgressID == "" {
			if unowned[p.MeetingID] == nil {
				order = append(order, p.MeetingID)
			}
			unowned[p.MeetingID] = append(unowned[p.MeetingID], p)
			continue
		}
		items, err := s.cfg.LiveKit.ListEgress(ctx, p.EgressID)
		if err != nil {
			// One part LiveKit will not answer for: the next is still asked.
			s.cfg.Log.Printf("reconcile: meeting %s part %d: LiveKit: %s", p.MeetingID, p.Part, lkKind(err))
			continue
		}
		seen := false
		for _, it := range items {
			if it.EgressID == p.EgressID {
				seen = true
				_, _ = s.onRecordingEgress(ctx, it)
			}
		}
		if lost(p, seen) {
			// As an end would (EndRecording records its egress ended, opens
			// the gap and restarts a recorded meeting).
			_, _ = s.onRecordingEgress(ctx, livekit.EgressInfo{EgressID: p.EgressID, Status: "EGRESS_ABORTED"})
		}
	}
	for _, id := range order {
		items, err := s.cfg.LiveKit.ListRoomEgress(ctx, store.RoomName(id))
		if err != nil {
			s.cfg.Log.Printf("reconcile: meeting %s: LiveKit: %s", id, lkKind(err))
			continue
		}
		keys := map[string]bool{}
		// unsure: the room lists an egress, not ended, with no file named
		// yet that is no track's (a composite still STARTING may name none):
		// it could be one of these parts', so none of them is called gone.
		unsure, failed := false, false
		for _, it := range items {
			if len(it.Files) == 0 || it.Files[0].Filename == "" {
				if !it.Ended() {
					if _, err := s.store.TrackByEgress(ctx, it.EgressID, now); errors.Is(err, store.ErrNotFound) {
						unsure = true
					}
				}
				continue // no file to judge it by
			}
			key := it.Files[0].Filename
			keys[key] = true
			owned, err := s.onRecordingEgress(ctx, it)
			if err != nil {
				failed = true
				continue // the database could not say whose it is: nothing is stopped
			}
			if owned {
				continue // a part's (adopted by its key if it had no id)
			}
			if strings.HasPrefix(key, "meetings/") && !it.Ended() && it.Status != "EGRESS_ENDING" {
				s.cfg.Log.Printf("reconcile: meeting %s: a composite egress no part owns; stopping it", id)
				if err := s.cfg.LiveKit.StopEgress(ctx, it.EgressID); err != nil {
					s.cfg.Log.Printf("reconcile: stopping it: %s (retried next pass)", lkKind(err))
				}
			}
		}
		for _, p := range unowned[id] {
			if lost(p, keys[p.ObjectKey] || unsure || failed) {
				s.goneEgress(ctx, p)
			}
		}
	}
}

// goneEgress resolves a part whose egress LiveKit has not listed for
// LostEgress: its egress has ended (none runs), so its object is final and
// it is due if its recording was deleted; a meeting still recorded with no
// part running starts its next, on the restart backoff (its attempt count
// kept), and only when no start loop of the meeting is running already (a
// running loop retries by itself; replacing it could drop a start in
// flight).
func (s *Server) goneEgress(ctx context.Context, p store.OpenPart) {
	if err := s.store.EgressGone(ctx, p.ID, "lost", s.cfg.Now()); err != nil {
		s.cfg.Log.Printf("reconcile: meeting %s part %d: %v", p.MeetingID, p.Part, err)
		return
	}
	s.cfg.Log.Printf("reconcile: meeting %s part %d: LiveKit has not listed its egress for %s; its egress is gone", p.MeetingID, p.Part, LostEgress)
	s.purgeMeeting(ctx, p.MeetingID)
	s.hub.Changed(p.MeetingID)
	s.rec.mu.Lock()
	running := s.rec.runs[p.MeetingID] != nil
	s.rec.mu.Unlock()
	if running {
		return
	}
	if ids, err := s.store.ToResume(ctx, s.cfg.Now()); err == nil {
		for _, id := range ids {
			if id == p.MeetingID {
				s.startRecordingAfter(id, s.recordingRestartWait(id, nil))
			}
		}
	}
}

// ---- screen shares (the analysis samples frames inside them) ----

// onScreenShare records a screen share's start, by the webhook's arrival:
// LiveKit says when a track was published only by sending the webhook.
func (s *Server) onScreenShare(ctx context.Context, room, identity string, tr livekit.TrackInfo) {
	if !s.Recording() || !tr.ScreenShare() || !validTrackID(tr.SID) {
		return
	}
	ok, err := s.store.MayBeInRoom(ctx, room, identity, s.cfg.Now())
	if err != nil || !ok {
		return
	}
	id, _, err := s.store.MeetingOfRoom(ctx, room)
	if err != nil {
		return
	}
	start, err := s.store.MeetingStart(ctx, id)
	if err != nil {
		return
	}
	name, guest, err := s.store.NameInRoom(ctx, room, identity)
	if err != nil {
		return
	}
	speaker := name
	if guest {
		speaker = guestLabel(name)
	} else if speaker == "" {
		speaker = "Host"
	}
	if _, err := s.store.StartScreenShare(ctx, room, tr.SID, identity, speaker, s.sinceStart(start), s.cfg.Now()); err != nil {
		s.cfg.Log.Printf("recording: a screen share was not recorded: %v", err)
	}
}

func (s *Server) onScreenShareEnded(ctx context.Context, tr livekit.TrackInfo) {
	if !s.Recording() || !validTrackID(tr.SID) {
		return
	}
	_, _ = s.store.EndScreenShare(ctx, tr.SID, s.sinceStart)
}

func validTrackID(sid string) bool {
	return len(sid) > 2 && len(sid) <= 64 && strings.HasPrefix(sid, "TR_")
}

// ---- retention ----

// SweepRecordings deletes every part past its meeting's purge time: its
// object first, then its row. 🔴 A part whose object could not be deleted
// (the store down) keeps its row for the next pass; an object already gone
// is not an error (s3.Delete), so a pass that died between the two steps is
// simply repeated. It logs counts, never a key, a title or a name.
func (s *Server) SweepRecordings(ctx context.Context) (deleted, left int) {
	if !s.Recording() {
		return 0, 0
	}
	now := s.cfg.Now()
	due, err := s.store.DueRecordings(ctx, now, 500)
	if err != nil {
		s.cfg.Log.Printf("retention: %v", err)
		return 0, 0
	}
	deleted, left = s.purge(ctx, due)
	if _, err := s.store.PurgeRecordingMeta(ctx, now); err != nil {
		s.cfg.Log.Printf("retention: %v", err)
	}
	if deleted+left > 0 {
		s.cfg.Log.Printf("retention: %d recording parts deleted, %d left for the next pass", deleted, left)
	}
	return deleted, left
}

// purgeMeeting deletes the due parts of one meeting (purge) and, once none
// is left, its share links and gaps. Nothing is due unless the meeting's
// purge time has passed.
func (s *Server) purgeMeeting(ctx context.Context, meetingID string) (deleted, left int) {
	now := s.cfg.Now()
	due, err := s.store.DueRecordingsOf(ctx, meetingID, now)
	if err != nil || len(due) == 0 {
		return 0, 0
	}
	deleted, left = s.purge(ctx, due)
	if _, err := s.store.PurgeRecordingMeta(ctx, now); err != nil {
		s.cfg.Log.Printf("recording: %v", err)
	}
	return deleted, left
}

// purge deletes each due part's object, then its row: the one step the
// sweeper and Delete now share. A part whose object the store would not
// delete keeps its row (left), and is due again on the next pass.
func (s *Server) purge(ctx context.Context, due []store.Due) (deleted, left int) {
	for _, d := range due {
		if d.ObjectKey != "" {
			if err := s.cfg.S3.Delete(ctx, d.ObjectKey); err != nil {
				left++
				continue
			}
		}
		if err := s.store.DeleteRecordingRow(ctx, d.ID); err != nil {
			left++
			continue
		}
		deleted++
	}
	return deleted, left
}

// DefaultRetentionEvery is the sweeper's period (design: "An hourly
// sweeper").
const DefaultRetentionEvery = time.Hour

func (s *Server) runRetention(ctx context.Context) {
	if !s.Recording() {
		return
	}
	every := s.cfg.RetentionEvery
	if every <= 0 {
		every = DefaultRetentionEvery
	}
	go func() {
		s.SweepRecordings(ctx)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.SweepRecordings(ctx)
			}
		}
	}()
}

// ---- the owner's routes ----

func (s *Server) recordingRoutes() {
	s.mux.HandleFunc("GET /api/settings", s.owner(s.handleGetSettings))
	s.mux.HandleFunc("PUT /api/settings", s.owner(s.handlePutSettings))
	s.mux.HandleFunc("GET /api/meetings/{id}/recording", s.owner(s.handleRecording))
	s.mux.HandleFunc("DELETE /api/meetings/{id}/recording", s.owner(s.handleDeleteRecording))
	s.mux.HandleFunc("POST /api/meetings/{id}/recording-url", s.owner(s.handleRecordingURL))
	s.mux.HandleFunc("POST /api/meetings/{id}/share", s.owner(s.handleShare))
	s.mux.HandleFunc("DELETE /api/meetings/{id}/share", s.owner(s.handleUnshare))
	s.mux.HandleFunc("GET /api/meetings/{id}/timeline", s.owner(s.handleTimeline))
	s.mux.HandleFunc("GET /api/meetings/{id}/frame", s.owner(s.handleFrame))
	s.mux.HandleFunc("GET /media/{id}/{part}", s.handleMedia)
	s.mux.HandleFunc("GET /r/{token}", s.handleSharePage)
	s.mux.HandleFunc("GET /r/{token}/{part}", s.handleShareMedia)
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request, repo *store.OwnerRepo) {
	v, err := repo.Settings(r.Context())
	if err != nil {
		s.meetingErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": v, "choices": map[string]any{"recording_retention_days": store.RetentionChoices}})
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request, repo *store.OwnerRepo) {
	var in struct {
		Days *int `json:"recording_retention_days"`
	}
	if !readJSON(w, r, 256, &in) {
		return
	}
	if in.Days == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": `send {"recording_retention_days": 30}, 90, 365, or 0 (forever)`})
		return
	}
	err := repo.SetSettings(r.Context(), store.Settings{RecordingRetentionDays: *in.Days}, s.cfg.Now())
	switch {
	case errors.Is(err, store.ErrRetention):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Recordings are kept 30, 90 or 365 days, or 0: forever."})
		return
	case errors.Is(err, store.ErrNoSession):
		w.Header().Set("WWW-Authenticate", `Bearer realm="meetings"`)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "no live owner session: " + EnterAgain})
		return
	case err != nil:
		s.meetingErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": store.Settings{RecordingRetentionDays: *in.Days}})
}

// recordingView is one meeting's recording as its owner sees it. Deleting
// says it was deleted (Delete now, or its purge time) and some part's object
// is still waiting for the store to delete it.
type recordingView struct {
	Parts    []store.Recording `json:"parts"`
	Gaps     []store.Gap       `json:"gaps"`
	PurgeAt  *time.Time        `json:"purge_at"`
	Share    store.ShareState  `json:"share"`
	Deleting bool              `json:"deleting"`
}

// partsOf is one of the owner's meetings' parts and gaps as they may be
// served: none once its purge time has passed (Delete now sets it to the
// moment), whatever rows still wait for their objects to go; deleting says
// some do. 🔴 Every owner route that serves the recording (its view, its
// media URLs, its frames, the timeline) reads it here.
func (s *Server) partsOf(ctx context.Context, repo *store.OwnerRepo, id string) (parts []store.Recording, gaps []store.Gap, purge *time.Time, deleting bool, err error) {
	parts, gaps, err = repo.Recordings(ctx, id)
	if err != nil {
		return nil, nil, nil, false, err
	}
	if purge, err = repo.PurgeRecordingAt(ctx, id); err != nil {
		return nil, nil, nil, false, err
	}
	if purge != nil && !s.cfg.Now().Before(*purge) {
		return []store.Recording{}, []store.Gap{}, purge, len(parts) > 0, nil
	}
	return parts, gaps, purge, false, nil
}

func (s *Server) recordingOf(ctx context.Context, repo *store.OwnerRepo, id string) (recordingView, error) {
	parts, gaps, purge, deleting, err := s.partsOf(ctx, repo, id)
	if err != nil {
		return recordingView{}, err
	}
	share, err := repo.ShareOf(ctx, id, s.cfg.Now())
	if err != nil {
		return recordingView{}, err
	}
	return recordingView{Parts: parts, Gaps: gaps, PurgeAt: purge, Share: share, Deleting: deleting}, nil
}

// handleDeleteRecording is Delete now: DELETE /api/meetings/{id}/recording,
// on an ended meeting of the owner's. 🔴 One transaction makes the
// recording unservable at once (its purge time is now, every share link
// revoked: store.DeleteRecording); then each part's object is deleted and
// then its row (purge). The store down refuses no part of that: the parts
// it would not delete stay queued and the answer says pending; the hourly
// sweeper finishes them, and the archive shows "Deleting…" meanwhile.
func (s *Server) handleDeleteRecording(w http.ResponseWriter, r *http.Request, repo *store.OwnerRepo) {
	id := r.PathValue("id")
	now := s.cfg.Now()
	n, err := repo.DeleteRecording(r.Context(), id, now)
	switch {
	case errors.Is(err, store.ErrRecordingLive):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "End the meeting before deleting its recording."})
		return
	case errors.Is(err, store.ErrNoRecording):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "This meeting has no recording."})
		return
	case errors.Is(err, store.ErrNoSession):
		w.Header().Set("WWW-Authenticate", `Bearer realm="meetings"`)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "no live owner session: " + EnterAgain})
		return
	case err != nil:
		s.meetingErr(w, err)
		return
	}
	// A part whose egress has not ended (still ENDING while it uploads) is
	// not due yet: its end deletes it (onRecordingEnded). It counts as pending.
	deleted, left := s.purgeMeeting(r.Context(), id)
	pending := n - deleted
	s.cfg.Log.Printf("recording: meeting %s: deleted now (%d parts; %d objects deleted, %d left for the store, %d waiting for their egress to end)",
		id, n, deleted, left, pending-left)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted, "pending": pending})
}

func (s *Server) handleRecording(w http.ResponseWriter, r *http.Request, repo *store.OwnerRepo) {
	v, err := s.recordingOf(r.Context(), repo, r.PathValue("id"))
	if err != nil {
		s.meetingErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// MediaURLLifetime is how long a signed media URL plays (design: "valid 15
// minutes").
const MediaURLLifetime = 15 * time.Minute

// mediaSig is the HMAC of a media path and its expiry (Unix seconds), base64url.
func (s *Server) mediaSig(path string, exp int64) string {
	m := hmac.New(sha256.New, s.mediaKey)
	m.Write([]byte(path + "\n" + strconv.FormatInt(exp, 10)))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// MediaPath is a part's media route.
func MediaPath(meetingID string, part int) string {
	return "/media/" + meetingID + "/" + strconv.Itoa(part)
}

// handleRecordingURL is POST /api/meetings/{id}/recording-url: a signed URL
// per playable part (a <video src> cannot carry the bearer), each valid
// MediaURLLifetime, with the part's place in meeting time.
func (s *Server) handleRecordingURL(w http.ResponseWriter, r *http.Request, repo *store.OwnerRepo) {
	id := r.PathValue("id")
	parts, _, _, _, err := s.partsOf(r.Context(), repo, id)
	if err != nil {
		s.meetingErr(w, err)
		return
	}
	exp := s.cfg.Now().Add(MediaURLLifetime).Unix()
	type out struct {
		timeline.Part
		URL string `json:"url"`
	}
	list := []out{}
	for _, p := range parts {
		if !p.Playable() {
			continue
		}
		path := MediaPath(id, p.Part)
		list = append(list, out{Part: p.TimelinePart(), URL: path + "?exp=" + strconv.FormatInt(exp, 10) + "&sig=" + s.mediaSig(path, exp)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"parts": list, "expires_at": time.Unix(exp, 0).UTC()})
}

// handleMedia is GET /media/{id}/{part}?exp=&sig=: the part streamed from the
// store, with Range. 🔴 The signature covers the path (so the meeting and the
// part) and the expiry; a wrong, expired or other path's signature is 403.
func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	id, partS := r.PathValue("id"), r.PathValue("part")
	q := r.URL.Query()
	exp, err := strconv.ParseInt(q.Get("exp"), 10, 64)
	path := MediaPath(id, atoi(partS))
	if err != nil || path != "/media/"+id+"/"+partS || !store.ValidMeetingID(id) ||
		!hmac.Equal([]byte(q.Get("sig")), []byte(s.mediaSig(path, exp))) || s.cfg.Now().Unix() >= exp {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "this media link has expired: open the recording again"})
		return
	}
	p, err := s.store.PlayablePart(r.Context(), id, atoi(partS), s.cfg.Now())
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such recording"})
		return
	}
	s.streamObject(w, r, p.ObjectKey)
}

func atoi(v string) int {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || strconv.Itoa(n) != v {
		return 0
	}
	return n
}

// streamObject answers key from the store, passing on one byte range (seek)
// and the store's status for it (200, 206, 416).
func (s *Server) streamObject(w http.ResponseWriter, r *http.Request, key string) {
	resp, err := s.cfg.S3.Get(r.Context(), key, r.Header.Get("Range"))
	if err != nil {
		s.cfg.Log.Printf("media: the store: %v", err)
		if s3.NotFound(err) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such recording"})
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "The recording cannot be read right now. Try again in a moment."})
		return
	}
	defer resp.Body.Close()
	h := w.Header()
	h.Set("Content-Type", "video/mp4")
	h.Set("Accept-Ranges", "bytes")
	h.Set("Cache-Control", "private, no-store")
	for _, k := range []string{"Content-Length", "Content-Range", "ETag", "Last-Modified"} {
		if v := resp.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, resp.Body)
	}
}

// ---- the share link ----

// ShareURL is a share link for token.
func (s *Server) ShareURL(token string) string { return s.cfg.PublicURL + "/r/" + token }

func (s *Server) handleShare(w http.ResponseWriter, r *http.Request, repo *store.OwnerRepo) {
	tok, exp, err := repo.Share(r.Context(), r.PathValue("id"), s.cfg.Now())
	switch {
	case errors.Is(err, store.ErrNotShareable):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "There is no finished recording to share yet."})
		return
	case errors.Is(err, store.ErrNoSession):
		w.Header().Set("WWW-Authenticate", `Bearer realm="meetings"`)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "no live owner session: " + EnterAgain})
		return
	case err != nil:
		s.meetingErr(w, err)
		return
	}
	// expires_at null: the recording is kept forever, so the link opens
	// until it is turned off or the recording deleted.
	writeJSON(w, http.StatusCreated, map[string]any{"url": s.ShareURL(tok), "expires_at": exp})
}

func (s *Server) handleUnshare(w http.ResponseWriter, r *http.Request, repo *store.OwnerRepo) {
	ok, err := repo.RevokeShare(r.Context(), r.PathValue("id"), s.cfg.Now())
	if err != nil {
		s.meetingErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": ok})
}

// sharePage is the share link's page.
type sharePage struct {
	Missing bool
	Title   string
	When    string
	Until   string
	Many    bool
	Parts   []sharePart
	Gaps    []string
}

type sharePart struct{ Label, Src string }

// shareCSP is the share page's policy: its own styles and media, nothing
// else (it has no script), framed by nobody.
const shareCSP = "default-src 'none'; style-src 'self'; img-src 'self'; media-src 'self'; font-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

// shareMissing answers every share link that opens nothing. 🔴 One answer,
// byte for byte, for an unknown, malformed, revoked or expired token, a part
// that is not there, and the page and its media alike: nothing tells a
// guesser a token ever existed.
func (s *Server) shareMissing(w http.ResponseWriter) {
	s.renderShare(w, http.StatusNotFound, sharePage{Missing: true})
}

func (s *Server) renderShare(w http.ResponseWriter, code int, p sharePage) {
	h := w.Header()
	h.Set("Content-Security-Policy", shareCSP)
	h.Set("Cache-Control", "no-store")
	h.Set("X-Robots-Tag", "noindex")
	h.Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	if err := pageTmpl.ExecuteTemplate(w, "share.html", p); err != nil {
		s.cfg.Log.Printf("page share.html: %v", err)
	}
}

func (s *Server) handleSharePage(w http.ResponseWriter, r *http.Request) {
	sh, err := s.store.ShareByToken(r.Context(), r.PathValue("token"), s.cfg.Now())
	if err != nil {
		s.shareMissing(w)
		return
	}
	until := "until it is turned off or deleted"
	if sh.ExpiresAt != nil {
		until = "until " + sh.ExpiresAt.Format("2 Jan 2006")
	}
	p := sharePage{Title: sh.Title, When: sh.CreatedAt.Format("2 Jan 2006"), Until: until, Many: len(sh.Parts) > 1}
	for i, part := range sh.Parts {
		p.Parts = append(p.Parts, sharePart{Label: "Part " + strconv.Itoa(i+1), Src: "/r/" + r.PathValue("token") + "/" + strconv.Itoa(part.Part)})
	}
	for _, g := range sh.Gaps {
		if g.ToMS != nil && *g.ToMS > g.FromMS {
			p.Gaps = append(p.Gaps, "Not recorded from "+clock(g.FromMS)+" to "+clock(*g.ToMS)+" into the meeting.")
		}
	}
	s.renderShare(w, http.StatusOK, p)
}

func (s *Server) handleShareMedia(w http.ResponseWriter, r *http.Request) {
	sh, err := s.store.ShareByToken(r.Context(), r.PathValue("token"), s.cfg.Now())
	if err != nil {
		s.shareMissing(w)
		return
	}
	n := atoi(r.PathValue("part"))
	for _, p := range sh.Parts {
		if p.Part == n {
			s.streamObject(w, r, p.ObjectKey)
			return
		}
	}
	s.shareMissing(w)
}

// ---- the time-aligned bundle (decision 4) ----

// bundleSegment is a transcript segment with its place in the recording.
type bundleSegment struct {
	store.Segment
	// Recording is where the segment's start is in the recording (nil when
	// no part covers it); ScreenShare says a screen was shared then.
	Recording   *timeline.Offset `json:"recording"`
	ScreenShare bool             `json:"screen_share"`
}

// frameRef is a frame the analysis can fetch: one per text segment spoken
// while a screen was shared and recorded, at the middle of that overlap.
type frameRef struct {
	MeetingMS int64  `json:"meeting_ms"`
	Part      int    `json:"part"`
	OffsetMS  int64  `json:"offset_ms"`
	SegmentID int64  `json:"segment_id"`
	Href      string `json:"href"`
}

// Bundle is GET /api/meetings/{id}/timeline: everything a muster agent or the
// post-call worker needs to read the meeting in one time base.
type Bundle struct {
	Meeting         store.Meeting       `json:"meeting"`
	MeetingStartMS  int64               `json:"meeting_start_unix_ms"`
	Parts           []store.Recording   `json:"parts"`
	Gaps            []store.Gap         `json:"recording_gaps"`
	ScreenShares    []store.ScreenShare `json:"screen_shares"`
	Segments        []bundleSegment     `json:"segments"`
	Frames          []frameRef          `json:"frames"`
	FramesAvailable bool                `json:"frames_available"`
	// Analysis is the current run's items, frame notes and summary (only
	// with the analysis configured, and once a run exists).
	Analysis *bundleAnalysis `json:"analysis,omitempty"`
}

func (s *Server) bundle(ctx context.Context, repo *store.OwnerRepo, id string) (Bundle, error) {
	m, segs, err := repo.Transcript(ctx, id)
	if err != nil {
		return Bundle{}, err
	}
	parts, gaps, _, _, err := s.partsOf(ctx, repo, id)
	if err != nil {
		return Bundle{}, err
	}
	shares, err := repo.ScreenShares(ctx, id)
	if err != nil {
		return Bundle{}, err
	}
	start := m.CreatedAt
	if m.StartedAt != nil {
		start = *m.StartedAt
	}
	end := s.sinceStart(start)
	if m.EndedAt != nil {
		end = m.EndedAt.Sub(start).Milliseconds()
	}
	tl := store.Parts(parts)
	b := Bundle{Meeting: m, MeetingStartMS: start.UnixMilli(), Parts: parts, Gaps: gaps, ScreenShares: shares,
		Segments: []bundleSegment{}, Frames: []frameRef{}, FramesAvailable: s.cfg.Frames.Available()}
	seen := map[int64]bool{}
	for _, g := range segs {
		bs := bundleSegment{Segment: g, Recording: timeline.Locate(tl, g.StartMS)}
		segEnd := int64(-1)
		if g.EndMS != nil {
			segEnd = *g.EndMS
		}
		for _, sh := range shares {
			shEnd := int64(-1)
			if sh.EndMS != nil {
				shEnd = *sh.EndMS
			}
			ov, ok := timeline.Overlap(timeline.Interval{FromMS: g.StartMS, ToMS: segEnd}, timeline.Interval{FromMS: sh.StartMS, ToMS: shEnd}, end)
			if !ok {
				continue
			}
			bs.ScreenShare = true
			if g.Kind != store.SegText {
				continue
			}
			at := (ov.FromMS + ov.ToMS) / 2
			if o := timeline.Locate(tl, at); o != nil && !seen[at] {
				seen[at] = true
				b.Frames = append(b.Frames, frameRef{MeetingMS: at, Part: o.Part, OffsetMS: o.OffsetMS, SegmentID: g.ID,
					Href: "/api/meetings/" + id + "/frame?at=" + strconv.FormatInt(at, 10)})
			}
		}
		b.Segments = append(b.Segments, bs)
	}
	return b, nil
}

func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request, repo *store.OwnerRepo) {
	id := r.PathValue("id")
	b, err := s.bundle(r.Context(), repo, id)
	if err != nil {
		s.meetingErr(w, err)
		return
	}
	if b.Analysis, err = s.analysisOf(r.Context(), id); err != nil {
		s.meetingErr(w, err)
		return
	}
	// An agent's token reads frames on its own route.
	if strings.HasPrefix(r.URL.Path, "/api/agent/") {
		for i := range b.Frames {
			b.Frames[i].Href = "/api/agent/meetings/" + id + "/frame?at=" + strconv.FormatInt(b.Frames[i].MeetingMS, 10)
		}
	}
	writeJSON(w, http.StatusOK, b)
}

// handleFrame is GET /api/meetings/{id}/frame?at=<meeting ms>: one JPEG of the
// recording at that meeting time (frames.Sampler), with its place in the
// recording in X-Meeting-Ms, X-Recording-Part and X-Recording-Offset-Ms.
func (s *Server) handleFrame(w http.ResponseWriter, r *http.Request, repo *store.OwnerRepo) {
	id := r.PathValue("id")
	at, err := strconv.ParseInt(r.URL.Query().Get("at"), 10, 64)
	if err != nil || at < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "at is a meeting time in ms"})
		return
	}
	parts, _, _, _, err := s.partsOf(r.Context(), repo, id)
	if err != nil {
		s.meetingErr(w, err)
		return
	}
	if !s.cfg.Frames.Available() || s.cfg.S3 == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "frame sampling is off: this plugin has no ffmpeg"})
		return
	}
	keys := map[int]string{}
	for _, p := range parts {
		keys[p.Part] = p.ObjectKey
	}
	select {
	case s.rec.frames <- struct{}{}:
		defer func() { <-s.rec.frames }()
	case <-r.Context().Done():
		return
	}
	got, _, err := s.cfg.Frames.Sample(r.Context(), store.Parts(parts), func(part int) frames.Object {
		return func(ctx context.Context, rng string) (*http.Response, error) {
			return s.cfg.S3.Get(ctx, keys[part], rng)
		}
	}, []int64{at})
	if err != nil {
		s.cfg.Log.Printf("frames: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "the frame could not be read"})
		return
	}
	if len(got) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "that moment is not in the recording"})
		return
	}
	h := w.Header()
	h.Set("Content-Type", "image/jpeg")
	h.Set("Cache-Control", "private, no-store")
	h.Set("X-Meeting-Ms", strconv.FormatInt(got[0].MeetingMS, 10))
	h.Set("X-Recording-Part", strconv.Itoa(got[0].Part))
	h.Set("X-Recording-Offset-Ms", strconv.FormatInt(got[0].OffsetMS, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(got[0].JPEG)
}
