package store_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/store"
)

const day = 24 * time.Hour

func i64(v int64) *int64 { return &v }

// Migration 6 (the recording) on a schema-5 database: every row is kept, the
// owner's recording retention is forever (0), no meeting gets a purge time
// (ended or open: kept until deleted), and the schema is this build's. (One-way: TestReopenKeepsDataAndRefusesANewerSchema pins that an
// older build refuses it.)
func TestMigration6KeepsEveryRowAndStampsEndedMeetings(t *testing.T) {
	p := filepath.Join(t.TempDir(), "m5.db")
	db, err := sql.Open("sqlite", "file:"+p+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range store.MigrationsBeforeRecordingForTest {
		if _, err := db.Exec(m); err != nil {
			t.Fatalf("migration %d: %v", i+1, err)
		}
	}
	for _, q := range []string{
		`PRAGMA user_version = 5`,
		`INSERT INTO owners (subject, display_name, updated_at) VALUES ('U3ViamVjdE1pZ3JhdGlvbj', 'Ada L.', 1)`,
		`INSERT INTO meetings (id, subject, title, room, join_code_sha256, state, record, ai, created_at, ended_at)
		 VALUES ('bWlncmF0aW9uNm1l', 'U3ViamVjdE1pZ3JhdGlvbj', 'old', 'meeting-m6', 'hash-m6', 'ended', 1, 1, 1000, 7000)`,
		`INSERT INTO meetings (id, subject, title, room, join_code_sha256, state, record, ai, created_at)
		 VALUES ('b3Blbm1lZXRpbmc2', 'U3ViamVjdE1pZ3JhdGlvbj', '', 'meeting-o6', 'hash-o6', 'open', 1, 1, 2000)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	s, err := store.Open(p, keyA)
	if err != nil {
		t.Fatalf("opening a schema-5 database: %v", err)
	}
	defer s.Close()
	db, _ = sql.Open("sqlite", "file:"+p+"?mode=ro")
	defer db.Close()
	var v, days int
	db.QueryRow(`PRAGMA user_version`).Scan(&v)
	db.QueryRow(`SELECT retention_days_recording FROM owners`).Scan(&days)
	var title string
	var purgeOld, purgeOpen sql.NullInt64
	db.QueryRow(`SELECT title, purge_recording_at FROM meetings WHERE id = 'bWlncmF0aW9uNm1l'`).Scan(&title, &purgeOld)
	db.QueryRow(`SELECT purge_recording_at FROM meetings WHERE id = 'b3Blbm1lZXRpbmc2'`).Scan(&purgeOpen)
	if v != store.SchemaForTest || days != 0 || title != "old" || purgeOld.Valid || purgeOpen.Valid {
		t.Fatalf("after migration 6: schema %d, retention %d, title %q, purge(ended) %v, purge(open) %v", v, days, title, purgeOld, purgeOpen)
	}
}

// Every end stamps the purge time from the owner's retention at the end:
// End meeting (with 365 days set) and the 24 h cap (with 90); with the
// default, forever, it stays NULL.
func TestEveryEndStampsTheRecordingPurgeTime(t *testing.T) {
	s, _ := open(t)
	r := owner(t, s, subA, t0)
	if err := r.SetSettings(ctx, store.Settings{RecordingRetentionDays: 365}, t0); err != nil {
		t.Fatal(err)
	}
	c := mk(t, s, subA, t0)
	endAt := t0.Add(47 * time.Minute)
	if _, err := r.End(ctx, c.Meeting.ID, endAt); err != nil {
		t.Fatal(err)
	}
	got, err := r.PurgeRecordingAt(ctx, c.Meeting.ID)
	if err != nil || got == nil || !got.Equal(endAt.Add(365*day)) {
		t.Fatalf("End: purge at %v (%v), want %v", got, err, endAt.Add(365*day))
	}

	b := owner(t, s, subB, t0)
	if err := b.SetSettings(ctx, store.Settings{RecordingRetentionDays: 90}, t0); err != nil {
		t.Fatal(err)
	}
	cb := mk(t, s, subB, t0)
	sweepAt := t0.Add(store.LinkLifetime + time.Minute)
	if n, err := s.Sweep(ctx, sweepAt); err != nil || n != 1 {
		t.Fatalf("sweep ended %d (%v)", n, err)
	}
	got, err = b.PurgeRecordingAt(ctx, cb.Meeting.ID)
	if err != nil || got == nil || !got.Equal(sweepAt.Add(90*day)) {
		t.Fatalf("24 h cap: purge at %v (%v), want %v", got, err, sweepAt.Add(90*day))
	}
	// The default: forever, no purge time.
	f := owner(t, s, "Ff0Gg1Hh2Ii3Jj4Kk5Ll6m", t0)
	cf := mk(t, s, "Ff0Gg1Hh2Ii3Jj4Kk5Ll6m", t0)
	if _, err := f.End(ctx, cf.Meeting.ID, endAt); err != nil {
		t.Fatal(err)
	}
	if got, err := f.PurgeRecordingAt(ctx, cf.Meeting.ID); err != nil || got != nil {
		t.Fatalf("forever: purge at %v (%v), want none", got, err)
	}
	// 🔴 A later finite setting does not reach a meeting kept forever when
	// some other meeting ends.
	if err := f.SetSettings(ctx, store.Settings{RecordingRetentionDays: 30}, t0); err != nil {
		t.Fatal(err)
	}
	cf2 := mk(t, s, "Ff0Gg1Hh2Ii3Jj4Kk5Ll6m", t0)
	if _, err := f.End(ctx, cf2.Meeting.ID, endAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.PurgeRecordingAt(ctx, cf.Meeting.ID); got != nil {
		t.Fatalf("a meeting kept forever was given %v by a later end", got)
	}
	if got, _ := f.PurgeRecordingAt(ctx, cf2.Meeting.ID); got == nil || !got.Equal(endAt.Add(time.Hour).Add(30*day)) {
		t.Fatalf("the next meeting under 30 days: purge at %v", got)
	}
	// A later change of the setting leaves an ended meeting's time alone.
	if err := r.SetSettings(ctx, store.Settings{RecordingRetentionDays: 30}, t0); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.PurgeRecordingAt(ctx, c.Meeting.ID); !got.Equal(endAt.Add(365 * day)) {
		t.Fatalf("the setting shortened an ended meeting: %v", got)
	}
}

func TestRetentionIsForeverByDefaultAnd30Or90Or365(t *testing.T) {
	s, _ := open(t)
	r := owner(t, s, subA, t0)
	if v, err := r.Settings(ctx); err != nil || v.RecordingRetentionDays != 0 {
		t.Fatalf("default = %+v %v, want 0 (forever)", v, err)
	}
	for _, d := range []int{30, 90, 365, 0} {
		if err := r.SetSettings(ctx, store.Settings{RecordingRetentionDays: d}, t0); err != nil {
			t.Errorf("%d days refused: %v", d, err)
		}
	}
	for _, d := range []int{7, 1, 366, -1} {
		if err := r.SetSettings(ctx, store.Settings{RecordingRetentionDays: d}, t0); !errors.Is(err, store.ErrRetention) {
			t.Errorf("%d days: %v", d, err)
		}
	}
}

// The parts and gaps of one meeting through a failure and a restart, every
// value literal: asked at meeting time 9 000; the egress's file began at
// 12 400; its file ended at 70 000; the second part was asked at 71 000 (the
// retry's backoff later) and its file began at 74 250. The failure's gap
// starts where the FILE ended, not at the retry.
func TestPartsAndGapsThroughAFailureAndARestart(t *testing.T) {
	s, _ := open(t)
	r := owner(t, s, subA, t0)
	c := mk(t, s, subA, t0)
	id := c.Meeting.ID

	p1, err := s.BeginRecording(ctx, id, 9_000, t0)
	if err != nil || p1.Part != 1 || p1.State != store.RecStarting || p1.ObjectKey != "meetings/"+subA+"/"+id+"/recording-1.mp4" {
		t.Fatalf("part 1 = %+v %v", p1, err)
	}
	if _, err := s.BeginRecording(ctx, id, 9_100, t0); !errors.Is(err, store.ErrRecordingRunning) {
		t.Fatalf("a second start while part 1 runs: %v", err)
	}
	if err := s.SetRecordingEgress(ctx, p1.ID, "EG_part1"); err != nil {
		t.Fatal(err)
	}
	if closed, err := s.RecordingBegan(ctx, p1.ID, 12_400); err != nil || !closed {
		t.Fatalf("began = %v %v", closed, err)
	}
	if st, _ := s.RecordingStatus(ctx, id); st != "" {
		t.Fatalf("status while recording = %q", st)
	}
	if _, err := s.EndRecording(ctx, p1.ID, store.RecordingEnd{State: store.RecFailed, StartMS: i64(12_400), DurationMS: i64(57_600), Bytes: i64(21_000_000),
		Error: "EGRESS_FAILED", GapAtMS: i64(70_000)}, t0); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.RecordingStatus(ctx, id); st != store.RecStatusFailed {
		t.Fatalf("status after the failure = %q, want failed", st)
	}
	p2, err := s.BeginRecording(ctx, id, 71_000, t0)
	if err != nil || p2.Part != 2 {
		t.Fatalf("part 2 = %+v %v", p2, err)
	}
	if _, err := s.RecordingBegan(ctx, p2.ID, 74_250); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.RecordingStatus(ctx, id); st != "" {
		t.Fatalf("status once part 2 began = %q", st)
	}
	recs, gaps, err := r.Recordings(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || *recs[0].StartMS != 12_400 || *recs[0].DurationMS != 57_600 || recs[0].State != store.RecFailed || !recs[0].Playable() ||
		*recs[1].StartMS != 74_250 || recs[1].DurationMS != nil || recs[1].State != store.RecActive || recs[1].Playable() {
		t.Fatalf("parts = %+v", recs)
	}
	want := []store.Gap{{FromMS: 9_000, ToMS: i64(12_400), Why: store.GapStarting}, {FromMS: 70_000, ToMS: i64(74_250), Why: store.GapFailed}}
	if len(gaps) != 2 || gaps[0].FromMS != want[0].FromMS || *gaps[0].ToMS != *want[0].ToMS || gaps[0].Why != want[0].Why ||
		gaps[1].FromMS != want[1].FromMS || *gaps[1].ToMS != *want[1].ToMS || gaps[1].Why != want[1].Why {
		t.Fatalf("gaps = %+v", gaps)
	}
	// A repeated end report changes nothing, and says so.
	if moved, err := s.EndRecording(ctx, p1.ID, store.RecordingEnd{State: store.RecComplete, DurationMS: i64(1)}, t0); err != nil || moved {
		t.Fatalf("a repeated end: moved %v, %v", moved, err)
	}
	if recs, _, _ := r.Recordings(ctx, id); *recs[0].DurationMS != 57_600 || recs[0].State != store.RecFailed {
		t.Fatalf("a second end rewrote part 1: %+v", recs[0])
	}
}

func TestNoRecordingWithRecordOffOrOnAnEndedMeeting(t *testing.T) {
	s, _ := open(t)
	r := owner(t, s, subA, t0)
	c := mk(t, s, subA, t0)
	if _, err := r.SetSwitch(ctx, c.Meeting.ID, store.SwitchRecord, false, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginRecording(ctx, c.Meeting.ID, 0, t0); !errors.Is(err, store.ErrNotRecorded) {
		t.Fatalf("record off: %v", err)
	}
	c2 := mk(t, s, subA, t0)
	r.End(ctx, c2.Meeting.ID, t0)
	if _, err := s.BeginRecording(ctx, c2.Meeting.ID, 0, t0); !errors.Is(err, store.ErrNotRecorded) {
		t.Fatalf("ended: %v", err)
	}
}

// A gap open when the meeting ends closes at the meeting's end, by every end
// path (endParticipants), and so does a screen share.
func TestTheEndClosesAnOpenGapAndScreenShare(t *testing.T) {
	s, _ := open(t)
	r := owner(t, s, subA, t0)
	c := mk(t, s, subA, t0)
	if err := r.MarkStarted(ctx, c.Meeting.ID, t0); err != nil {
		t.Fatal(err)
	}
	p, _ := s.BeginRecording(ctx, c.Meeting.ID, 5_000, t0)
	s.RecordingStartRefused(ctx, p.ID, "unavailable", t0)
	if _, err := s.StartScreenShare(ctx, store.RoomName(c.Meeting.ID), "TR_screen1", "owner", "Ada L.", 20_000, t0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EndRoom(ctx, store.RoomName(c.Meeting.ID), "test", t0.Add(90*time.Second)); err != nil {
		t.Fatal(err)
	}
	_, gaps, _ := r.Recordings(ctx, c.Meeting.ID)
	if len(gaps) != 1 || gaps[0].ToMS == nil || *gaps[0].ToMS != 90_000 {
		t.Fatalf("gaps after the end = %+v", gaps)
	}
	ss, _ := r.ScreenShares(ctx, c.Meeting.ID)
	if len(ss) != 1 || ss[0].EndMS == nil || *ss[0].EndMS != 90_000 || ss[0].StartMS != 20_000 {
		t.Fatalf("screen shares after the end = %+v", ss)
	}
}

func TestRecordingsAreTheOwnersAlone(t *testing.T) {
	s, _ := open(t)
	c := mk(t, s, subA, t0)
	s.BeginRecording(ctx, c.Meeting.ID, 0, t0)
	b := owner(t, s, subB, t0)
	if _, _, err := b.Recordings(ctx, c.Meeting.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("another owner read the parts: %v", err)
	}
	if _, _, err := b.Share(ctx, c.Meeting.ID, t0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("another owner shared: %v", err)
	}
	if ss, err := b.ScreenShares(ctx, c.Meeting.ID); err != nil || len(ss) != 0 {
		t.Fatalf("another owner's screen shares: %v %v", ss, err)
	}
}

// finished is a meeting of subA, kept 30 days, that ended at endAt with one
// playable part.
func finished(t *testing.T, s *store.Store, endAt time.Time) (*store.OwnerRepo, string) {
	t.Helper()
	r := owner(t, s, subA, t0)
	if err := r.SetSettings(ctx, store.Settings{RecordingRetentionDays: 30}, t0); err != nil {
		t.Fatal(err)
	}
	c := mk(t, s, subA, t0)
	p, err := s.BeginRecording(ctx, c.Meeting.ID, 0, t0)
	if err != nil {
		t.Fatal(err)
	}
	s.RecordingBegan(ctx, p.ID, 3_000)
	s.EndRecording(ctx, p.ID, store.RecordingEnd{State: store.RecComplete, DurationMS: i64(60_000), Bytes: i64(9_000_000)}, endAt)
	if _, err := r.End(ctx, c.Meeting.ID, endAt); err != nil {
		t.Fatal(err)
	}
	return r, c.Meeting.ID
}

func TestAShareLinkOpensUntilThePurgeAndNotOnceRevokedOrReplaced(t *testing.T) {
	s, _ := open(t)
	endAt := t0.Add(time.Hour)
	r, id := finished(t, s, endAt)
	tok, exp, err := r.Share(ctx, id, endAt)
	if err != nil || exp == nil || !exp.Equal(endAt.Add(30*day)) || !store.ValidShareToken(tok) {
		t.Fatalf("share = %q %v %v", tok, exp, err)
	}
	sh, err := s.ShareByToken(ctx, tok, endAt.Add(29*day))
	if err != nil || sh.MeetingID != id || len(sh.Parts) != 1 {
		t.Fatalf("open at day 29: %+v %v", sh, err)
	}
	if _, err := s.ShareByToken(ctx, tok, endAt.Add(30*day)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("open at the purge time: %v", err)
	}
	// A new link revokes the old one.
	tok2, _, err := r.Share(ctx, id, endAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ShareByToken(ctx, tok, endAt); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the replaced link still opens: %v", err)
	}
	if _, err := s.ShareByToken(ctx, tok2, endAt); err != nil {
		t.Fatalf("the new link: %v", err)
	}
	if ok, err := r.RevokeShare(ctx, id, endAt); !ok || err != nil {
		t.Fatalf("revoke = %v %v", ok, err)
	}
	if _, err := s.ShareByToken(ctx, tok2, endAt); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a revoked link opens: %v", err)
	}
	for _, bad := range []string{"", "x", tok + "A", tok[:42] + "!"} {
		if _, err := s.ShareByToken(ctx, bad, endAt); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("token %q: %v", bad, err)
		}
	}
}

func TestTheTokenIsStoredOnlyHashed(t *testing.T) {
	s, p := open(t)
	endAt := t0.Add(time.Hour)
	r, id := finished(t, s, endAt)
	tok, _, err := r.Share(ctx, id, endAt)
	if err != nil {
		t.Fatal(err)
	}
	if b := dbBytes(t, p); containsBytes(b, tok) {
		t.Fatal("the share token is in the database file")
	}
}

func TestNoShareWhileTheMeetingRunsOrWithNothingPlayable(t *testing.T) {
	s, _ := open(t)
	r := owner(t, s, subA, t0)
	c := mk(t, s, subA, t0)
	if _, _, err := r.Share(ctx, c.Meeting.ID, t0); !errors.Is(err, store.ErrNotShareable) {
		t.Fatalf("live meeting: %v", err)
	}
	r.End(ctx, c.Meeting.ID, t0)
	if _, _, err := r.Share(ctx, c.Meeting.ID, t0); !errors.Is(err, store.ErrNotShareable) {
		t.Fatalf("ended with no recording: %v", err)
	}
}

func TestDueRecordingsArePastTheirPurgeTime(t *testing.T) {
	s, _ := open(t)
	endAt := t0.Add(time.Hour)
	_, id := finished(t, s, endAt)
	if d, err := s.DueRecordings(ctx, endAt.Add(30*day-time.Millisecond), 10); err != nil || len(d) != 0 {
		t.Fatalf("due before the purge time: %+v %v", d, err)
	}
	d, err := s.DueRecordings(ctx, endAt.Add(30*day), 10)
	if err != nil || len(d) != 1 || d[0].MeetingID != id || d[0].ObjectKey != "meetings/"+subA+"/"+id+"/recording-1.mp4" {
		t.Fatalf("due at the purge time: %+v %v", d, err)
	}
}

func containsBytes(b []byte, s string) bool {
	for i := 0; i+len(s) <= len(b); i++ {
		if string(b[i:i+len(s)]) == s {
			return true
		}
	}
	return false
}

// A recording kept forever: its share link has no expiry and opens years
// later, until it is revoked.
func TestAForeverRecordingsLinkOpensUntilRevoked(t *testing.T) {
	s, _ := open(t)
	r := owner(t, s, subA, t0)
	c := mk(t, s, subA, t0)
	p, _ := s.BeginRecording(ctx, c.Meeting.ID, 0, t0)
	s.RecordingBegan(ctx, p.ID, 1_000)
	s.EndRecording(ctx, p.ID, store.RecordingEnd{State: store.RecComplete, DurationMS: i64(5_000), Bytes: i64(100)}, t0)
	r.End(ctx, c.Meeting.ID, t0)
	tok, exp, err := r.Share(ctx, c.Meeting.ID, t0)
	if err != nil || exp != nil {
		t.Fatalf("share = %v %v, want no expiry", exp, err)
	}
	if _, err := s.ShareByToken(ctx, tok, t0.Add(5*365*day)); err != nil {
		t.Fatalf("five years on: %v", err)
	}
	if d, _ := s.DueRecordings(ctx, t0.Add(5*365*day), 10); len(d) != 0 {
		t.Fatalf("a forever recording is due: %v", d)
	}
	r.RevokeShare(ctx, c.Meeting.ID, t0)
	if _, err := s.ShareByToken(ctx, tok, t0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoked: %v", err)
	}
}

// 🔴 Delete now: one transaction makes the purge time now and revokes every
// share link; the parts are then due; a live meeting is refused, and another
// owner gets the same not found as no meeting.
func TestDeleteNowRevokesEveryLinkAndMakesThePartsDue(t *testing.T) {
	s, _ := open(t)
	endAt := t0.Add(time.Hour)
	r, id := finished(t, s, endAt)
	tok, _, _ := r.Share(ctx, id, endAt)
	if s.LiveSharesForTest(id) != 1 {
		t.Fatal("no live link to start with")
	}
	b := owner(t, s, subB, t0)
	if _, err := b.DeleteRecording(ctx, id, endAt); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("another owner deleted it: %v", err)
	}
	if s.LiveSharesForTest(id) != 1 {
		t.Fatal("another owner's delete revoked the link")
	}
	at := endAt.Add(time.Minute)
	if n, err := r.DeleteRecording(ctx, id, at); err != nil || n != 1 {
		t.Fatalf("delete = %d %v", n, err)
	}
	if n := s.LiveSharesForTest(id); n != 0 {
		t.Fatalf("a share link left unrevoked after Delete now (%d)", n)
	}
	if got, _ := r.PurgeRecordingAt(ctx, id); got == nil || !got.Equal(at) {
		t.Fatalf("purge after Delete now = %v, want %v", got, at)
	}
	if d, _ := s.DueRecordingsOf(ctx, id, at); len(d) != 1 {
		t.Fatalf("due after Delete now: %v", d)
	}
	if _, err := s.ShareByToken(ctx, tok, at); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the link after Delete now: %v", err)
	}
	if _, err := s.PlayablePart(ctx, id, 1, at); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the part after Delete now: %v", err)
	}
	live := mk(t, s, subA, t0)
	s.BeginRecording(ctx, live.Meeting.ID, 0, t0)
	if _, err := r.DeleteRecording(ctx, live.Meeting.ID, at); !errors.Is(err, store.ErrRecordingLive) {
		t.Fatalf("a live meeting: %v", err)
	}
}
