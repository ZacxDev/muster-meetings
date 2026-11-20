package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/store"
)

// t0 is an arbitrary fixed instant, not a round number of anything the code
// names.
var t0 = time.Date(2031, 3, 14, 15, 9, 26, 535_000_000, time.UTC)

func open(t *testing.T) (*store.Store, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sub", "meetings.db")
	s, err := store.Open(p, keyA)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, p
}

var ctx = context.Background()

// Two link keys, neither all zeros nor any pattern the code names.
var (
	keyA = func() []byte { h := sha256.Sum256([]byte("fixture link key A")); return h[:] }()
	keyB = func() []byte { h := sha256.Sum256([]byte("fixture link key B")); return h[:] }()
)

// ---- the link: entropy and format ----

var joinCodeShape = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)

// "Join code: 22 characters, base64url, distinct across 10,000 draws, stored
// only hashed. Red if: a shorter random source is used."
func TestJoinCodeIs128RandomBitsIn22Base64urlCharacters(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 10_000; i++ {
		c := store.NewJoinCode()
		if !joinCodeShape.MatchString(c) {
			t.Fatalf("join code %q is not 22 base64url characters", c)
		}
		raw, err := base64.RawURLEncoding.DecodeString(c)
		if err != nil || len(raw) != 16 {
			t.Fatalf("join code %q decodes to %d bytes (%v), want 16 (128 bits)", c, len(raw), err)
		}
		if seen[c] {
			t.Fatalf("join code %q drawn twice in %d draws", c, i+1)
		}
		seen[c] = true
	}
}

func TestSessionTokenAndMeetingIDShapes(t *testing.T) {
	tok := store.NewSessionToken()
	raw, err := base64.RawURLEncoding.DecodeString(tok[len("mmo_"):])
	if !regexp.MustCompile(`^mmo_[A-Za-z0-9_-]{43}$`).MatchString(tok) || err != nil || len(raw) != 32 {
		t.Fatalf("session token %q: want mmo_ and 32 random bytes", tok)
	}
	if !store.ValidSessionToken(tok) || store.ValidSessionToken(tok[:len(tok)-1]) || store.ValidSessionToken("mmx_"+tok[4:]) {
		t.Fatalf("ValidSessionToken accepts the wrong shapes")
	}
	id := store.NewMeetingID()
	// A meeting id rides muster's ref (^[A-Za-z0-9_-]{1,64}$) in a knock.
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{16}$`).MatchString(id) || !store.ValidMeetingID(id) {
		t.Fatalf("meeting id %q", id)
	}
	if store.NewMeetingID() == id || store.NewSessionToken() == tok {
		t.Fatal("two draws were equal")
	}
}

// The code exists only in the answer to New meeting: neither the database
// file nor its WAL holds it, while both hold its SHA-256 (the positive
// control: the scan can find what is there).
func TestJoinCodeIsStoredOnlyHashed(t *testing.T) {
	s, p := open(t)
	c, err := s.ForOwner(7).CreateMeeting(ctx, store.NewMeeting{Title: "budget sync"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	var all []byte
	for _, f := range []string{p, p + "-wal"} {
		b, err := os.ReadFile(f)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		all = append(all, b...)
	}
	if bytes.Contains(all, []byte(c.JoinCode)) {
		t.Fatalf("the join code itself is in the database")
	}
	if !bytes.Contains(all, []byte(store.Hash(c.JoinCode))) {
		t.Fatal("positive control: the code's hash is not in the database files, so the scan cannot see rows")
	}
	if !bytes.Contains(all, []byte("budget sync")) {
		t.Fatal("positive control: the title is not in the database files")
	}
	if sealed := s.SealedForTest(c.Meeting.ID); len(sealed) == 0 || !bytes.Contains(all, sealed) {
		t.Fatal("positive control: the sealed link is not in the database files, so the scan never saw it")
	}
}

func TestSessionTokenIsStoredOnlyHashed(t *testing.T) {
	s, p := open(t)
	tok, _, err := s.CreateSession(ctx, 7, 12, t0)
	if err != nil {
		t.Fatal(err)
	}
	var all []byte
	for _, f := range []string{p, p + "-wal"} {
		b, _ := os.ReadFile(f)
		all = append(all, b...)
	}
	if bytes.Contains(all, []byte(tok[4:])) {
		t.Fatal("the session token itself is in the database")
	}
	if !bytes.Contains(all, []byte(store.Hash(tok))) {
		t.Fatal("positive control: the token's hash is not in the database files")
	}
}

// ---- sessions ----

func TestSessionLivesEightHoursBoundToItsOwner(t *testing.T) {
	s, _ := open(t)
	tok, exp, err := s.CreateSession(ctx, 7, 12, t0)
	if err != nil {
		t.Fatal(err)
	}
	if want := t0.Add(8 * time.Hour); !exp.Equal(want) {
		t.Fatalf("expires %s, want %s", exp, want)
	}
	got, err := s.LookupSession(ctx, tok, t0.Add(8*time.Hour-time.Millisecond))
	if err != nil || got.OwnerID != 7 || got.PluginID != 12 {
		t.Fatalf("one ms before expiry = %+v, %v; want owner 7 plugin 12", got, err)
	}
	if _, err := s.LookupSession(ctx, tok, t0.Add(8*time.Hour)); err != store.ErrNotFound {
		t.Fatalf("at expiry = %v, want ErrNotFound", err)
	}
	if _, err := s.LookupSession(ctx, store.NewSessionToken(), t0); err != store.ErrNotFound {
		t.Fatalf("an unknown token = %v", err)
	}
	if _, err := s.LookupSession(ctx, store.Hash(tok), t0); err != store.ErrNotFound {
		t.Fatalf("the stored hash, presented as a token = %v, want ErrNotFound", err)
	}
}

// ---- ownership, at the repository ----

// "Ownership: every repository query for owner A returns nothing of owner
// B's. Red if: the repository's owner filter is removed."
func TestOwnerRepoNeverReturnsAnotherOwnersMeetings(t *testing.T) {
	s, _ := open(t)
	a, b := s.ForOwner(7), s.ForOwner(41)
	ma1, _ := a.CreateMeeting(ctx, store.NewMeeting{Title: "a one"}, t0)
	mb, _ := b.CreateMeeting(ctx, store.NewMeeting{Title: "b one"}, t0.Add(time.Minute))
	ma2, _ := a.CreateMeeting(ctx, store.NewMeeting{Title: "a two"}, t0.Add(2*time.Minute))
	if err := s.AddParticipantForTest(ctx, mb.Meeting.ID, "Grace"); err != nil {
		t.Fatal(err)
	}

	list, next, err := a.ListMeetings(ctx, nil, 50)
	if err != nil || next != nil || len(list) != 2 || list[0].ID != ma2.Meeting.ID || list[1].ID != ma1.Meeting.ID {
		t.Fatalf("A's list = %+v, next %v, %v; want A's two, newest first", list, next, err)
	}
	list, _, _ = b.ListMeetings(ctx, nil, 50)
	if len(list) != 1 || list[0].ID != mb.Meeting.ID {
		t.Fatalf("B's list = %+v, want B's one", list)
	}
	if _, _, err := a.Meeting(ctx, mb.Meeting.ID); err != store.ErrNotFound {
		t.Fatalf("A reading B's meeting = %v, want ErrNotFound", err)
	}
	// Positive control: B reads its own, with its participant.
	m, ps, err := b.Meeting(ctx, mb.Meeting.ID)
	if err != nil || m.Title != "b one" || len(ps) != 1 || ps[0].DisplayName != "Grace" {
		t.Fatalf("B reading its own = %+v %+v %v", m, ps, err)
	}
	// A cursor pointing into B's rows still pages only A's.
	list, _, _ = a.ListMeetings(ctx, &store.Cursor{CreatedAt: t0.Add(time.Hour).UnixMilli(), ID: mb.Meeting.ID}, 50)
	if len(list) != 2 {
		t.Fatalf("A's list after a cursor = %d rows, want 2", len(list))
	}
}

func TestListPagesNewestFirst(t *testing.T) {
	s, _ := open(t)
	r := s.ForOwner(7)
	var ids []string
	for i := 0; i < 5; i++ {
		c, err := r.CreateMeeting(ctx, store.NewMeeting{}, t0.Add(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, c.Meeting.ID)
	}
	p1, next, err := r.ListMeetings(ctx, nil, 2)
	if err != nil || len(p1) != 2 || p1[0].ID != ids[4] || p1[1].ID != ids[3] || next == nil {
		t.Fatalf("page 1 = %v next %v %v", p1, next, err)
	}
	c, ok := store.ParseCursor(next.String())
	if !ok || c != *next {
		t.Fatalf("cursor round trip %q -> %+v", next.String(), c)
	}
	p2, next, _ := r.ListMeetings(ctx, &c, 2)
	p3, last, _ := r.ListMeetings(ctx, next, 2)
	if len(p2) != 2 || p2[0].ID != ids[2] || p2[1].ID != ids[1] || len(p3) != 1 || p3[0].ID != ids[0] || last != nil {
		t.Fatalf("pages 2 and 3 = %v %v (last %v)", p2, p3, last)
	}
	for _, bad := range []string{"", "x", "12", "-1." + ids[0], "12.short", "12." + ids[0] + "x"} {
		if _, ok := store.ParseCursor(bad); ok {
			t.Errorf("ParseCursor(%q) accepted it", bad)
		}
	}
}

// ---- defaults ----

func TestANewMeetingIsOpenWithRecordAndAIOnAndLinkFor24Hours(t *testing.T) {
	s, _ := open(t)
	c, err := s.ForOwner(7).CreateMeeting(ctx, store.NewMeeting{}, t0)
	if err != nil {
		t.Fatal(err)
	}
	m := c.Meeting
	if m.State != "open" || !m.Record || !m.AI || !m.CreatedAt.Equal(t0.Truncate(time.Millisecond)) ||
		!m.LinkExpiresAt.Equal(m.CreatedAt.Add(24*time.Hour)) || m.StartedAt != nil || m.EndedAt != nil {
		t.Fatalf("new meeting = %+v", m)
	}
	off := false
	c, _ = s.ForOwner(7).CreateMeeting(ctx, store.NewMeeting{Record: &off}, t0)
	if c.Meeting.Record || !c.Meeting.AI {
		t.Fatalf("record off = %+v, want record off and AI still on", c.Meeting)
	}
}

// ---- revocation and its scope ----

func TestRevokePluginDeletesItsSessionsAndEndsTheOwnersOpenMeetings(t *testing.T) {
	s, _ := open(t)
	tA, _, _ := s.CreateSession(ctx, 7, 12, t0)
	tB, _, _ := s.CreateSession(ctx, 41, 19, t0)
	ma, _ := s.ForOwner(7).CreateMeeting(ctx, store.NewMeeting{}, t0)
	mb, _ := s.ForOwner(41).CreateMeeting(ctx, store.NewMeeting{}, t0)
	res, err := s.RevokePlugin(ctx, 12, 7, true, t0.Add(time.Minute))
	if err != nil || res.Sessions != 1 || res.Meetings != 1 {
		t.Fatalf("RevokePlugin = %+v, %v", res, err)
	}
	if _, err := s.LookupSession(ctx, tA, t0.Add(time.Minute)); err != store.ErrNotFound {
		t.Fatalf("A's session after revoke = %v", err)
	}
	if m, _, _ := s.ForOwner(7).Meeting(ctx, ma.Meeting.ID); m.State != "ended" || m.EndedAt == nil {
		t.Fatalf("A's meeting after revoke = %+v", m)
	}
	// B, served by another token, is untouched.
	if _, err := s.LookupSession(ctx, tB, t0.Add(time.Minute)); err != nil {
		t.Fatalf("B's session after A's revoke = %v", err)
	}
	if m, _, _ := s.ForOwner(41).Meeting(ctx, mb.Meeting.ID); m.State != "open" {
		t.Fatalf("B's meeting after A's revoke = %+v", m)
	}
}

func TestSweepEndsAMeetingAt24HoursAndNotBefore(t *testing.T) {
	s, _ := open(t)
	c, _ := s.ForOwner(7).CreateMeeting(ctx, store.NewMeeting{}, t0)
	if n, err := s.Sweep(ctx, t0.Add(24*time.Hour-time.Millisecond)); err != nil || n != 0 {
		t.Fatalf("sweep one ms short = %d, %v", n, err)
	}
	if n, err := s.Sweep(ctx, t0.Add(24*time.Hour)); err != nil || n != 1 {
		t.Fatalf("sweep at 24 h = %d, %v", n, err)
	}
	if m, _, _ := s.ForOwner(7).Meeting(ctx, c.Meeting.ID); m.State != "ended" {
		t.Fatalf("after sweep = %+v", m)
	}
}

func TestReconcileLiveDropsWhatNoLiveTokenServes(t *testing.T) {
	s, _ := open(t)
	dead, _, _ := s.CreateSession(ctx, 7, 12, t0)
	live, _, _ := s.CreateSession(ctx, 41, 19, t0)
	ma, _ := s.ForOwner(7).CreateMeeting(ctx, store.NewMeeting{}, t0)
	mb, _ := s.ForOwner(41).CreateMeeting(ctx, store.NewMeeting{}, t0)
	res, err := s.ReconcileLive(ctx, []int64{19}, []int64{41}, t0)
	if err != nil || res.Sessions != 1 || res.Meetings != 1 {
		t.Fatalf("ReconcileLive = %+v, %v", res, err)
	}
	if _, err := s.LookupSession(ctx, dead, t0); err != store.ErrNotFound {
		t.Fatal("a session of a dead token survived")
	}
	if _, err := s.LookupSession(ctx, live, t0); err != nil {
		t.Fatal("a session of a live token was dropped")
	}
	if m, _, _ := s.ForOwner(7).Meeting(ctx, ma.Meeting.ID); m.State != "ended" {
		t.Fatal("an unserved owner's meeting stayed open")
	}
	if m, _, _ := s.ForOwner(41).Meeting(ctx, mb.Meeting.ID); m.State != "open" {
		t.Fatal("a served owner's meeting was ended")
	}
}

func TestReopenKeepsDataAndRefusesANewerSchema(t *testing.T) {
	s, p := open(t)
	c, _ := s.ForOwner(7).CreateMeeting(ctx, store.NewMeeting{Title: "kept"}, t0)
	s.Close()
	s2, err := store.Open(p, keyA)
	if err != nil {
		t.Fatal(err)
	}
	if m, _, err := s2.ForOwner(7).Meeting(ctx, c.Meeting.ID); err != nil || m.Title != "kept" {
		t.Fatalf("after reopen = %+v, %v", m, err)
	}
	if err := s2.SetUserVersionForTest(99); err != nil {
		t.Fatal(err)
	}
	s2.Close()
	if _, err := store.Open(p, keyA); err == nil {
		t.Fatal("a database from a newer build was opened")
	}
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("database mode = %v, %v; want 0600", fi.Mode().Perm(), err)
	}
}

// ---- the link, sealed so its owner can copy it again ----

func TestOnlyTheOwnerReadsTheLink(t *testing.T) {
	s, _ := open(t)
	c, _ := s.ForOwner(7).CreateMeeting(ctx, store.NewMeeting{}, t0)
	if _, err := s.ForOwner(41).Link(ctx, c.Meeting.ID, t0); err != store.ErrNotFound {
		t.Fatalf("another owner reading the link = %v, want ErrNotFound", err)
	}
	if got, err := s.ForOwner(7).Link(ctx, c.Meeting.ID, t0); err != nil || got != c.JoinCode {
		t.Fatalf("positive control: the owner's link = %q, %v; want the code made with it", got, err)
	}
}

func TestAnEndedMeetingHasNoLinkAndItsSealedCodeIsNull(t *testing.T) {
	s, _ := open(t)
	c, _ := s.ForOwner(7).CreateMeeting(ctx, store.NewMeeting{}, t0)
	if _, err := s.RevokePlugin(ctx, 12, 7, true, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if sealed := s.SealedForTest(c.Meeting.ID); sealed != nil {
		t.Fatalf("ending the meeting left its sealed code (%d bytes)", len(sealed))
	}
	if _, err := s.ForOwner(7).Link(ctx, c.Meeting.ID, t0.Add(time.Minute)); err != store.ErrNoLink {
		t.Fatalf("an ended meeting's link = %v, want ErrNoLink", err)
	}
}

// An ended meeting whose sealed code is somehow still there (an end path
// that forgot it): the lookup refuses it AND deletes it.
func TestALookupOfAnEndedMeetingDeletesALeftoverSealedCode(t *testing.T) {
	s, _ := open(t)
	c, _ := s.ForOwner(7).CreateMeeting(ctx, store.NewMeeting{}, t0)
	s.EndKeepingLinkForTest(c.Meeting.ID)
	if s.SealedForTest(c.Meeting.ID) == nil {
		t.Fatal("positive control: the fixture lost its sealed code before the lookup")
	}
	if _, err := s.ForOwner(7).Link(ctx, c.Meeting.ID, t0); err != store.ErrNoLink {
		t.Fatalf("the link of an ended meeting = %v, want ErrNoLink", err)
	}
	if s.SealedForTest(c.Meeting.ID) != nil {
		t.Fatal("the lookup refused the link but left the sealed code in the row")
	}
}

func TestALinkPast24HoursIsGoneAndItsSealedCodeNulledByTheLookup(t *testing.T) {
	s, _ := open(t)
	c, _ := s.ForOwner(7).CreateMeeting(ctx, store.NewMeeting{}, t0)
	r := s.ForOwner(7)
	if got, err := r.Link(ctx, c.Meeting.ID, t0.Add(24*time.Hour-time.Millisecond)); err != nil || got != c.JoinCode {
		t.Fatalf("one ms before 24 h = %q, %v; want the link", got, err)
	}
	if _, err := r.Link(ctx, c.Meeting.ID, t0.Add(24*time.Hour)); err != store.ErrNoLink {
		t.Fatalf("at 24 h = %v, want ErrNoLink", err)
	}
	if s.SealedForTest(c.Meeting.ID) != nil {
		t.Fatal("the lookup at 24 h left the sealed code in the row")
	}
}

func TestTheSweepNullsTheSealedCodeAt24Hours(t *testing.T) {
	s, _ := open(t)
	c, _ := s.ForOwner(7).CreateMeeting(ctx, store.NewMeeting{}, t0)
	if _, err := s.Sweep(ctx, t0.Add(24*time.Hour-time.Millisecond)); err != nil || s.SealedForTest(c.Meeting.ID) == nil {
		t.Fatalf("a sweep one ms short of 24 h removed the sealed code (%v)", err)
	}
	if _, err := s.Sweep(ctx, t0.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if s.SealedForTest(c.Meeting.ID) != nil {
		t.Fatal("the sweep at 24 h left the sealed code in the row")
	}
}

// The meeting id is the associated data: a sealed code moved onto another
// meeting's row does not open there.
func TestASealedCodeMovedOntoAnotherMeetingDoesNotOpen(t *testing.T) {
	s, _ := open(t)
	r := s.ForOwner(7)
	a, _ := r.CreateMeeting(ctx, store.NewMeeting{Title: "a"}, t0)
	b, _ := r.CreateMeeting(ctx, store.NewMeeting{Title: "b"}, t0)
	s.SetSealedForTest(b.Meeting.ID, s.SealedForTest(a.Meeting.ID))
	if got, err := r.Link(ctx, b.Meeting.ID, t0); err != store.ErrSealed {
		t.Fatalf("a's sealed code on b's row = %q, %v; want ErrSealed", got, err)
	}
	if got, err := r.Link(ctx, a.Meeting.ID, t0); err != nil || got != a.JoinCode {
		t.Fatalf("positive control: a's own = %q, %v", got, err)
	}
}

func TestAnotherKeyDoesNotOpenTheLink(t *testing.T) {
	s, p := open(t)
	c, _ := s.ForOwner(7).CreateMeeting(ctx, store.NewMeeting{}, t0)
	s.Close()
	other, err := store.Open(p, keyB)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := other.ForOwner(7).Link(ctx, c.Meeting.ID, t0); err != store.ErrSealed {
		t.Fatalf("under another key = %q, %v; want ErrSealed", got, err)
	}
	other.Close()
	same, err := store.Open(p, keyA)
	if err != nil {
		t.Fatal(err)
	}
	defer same.Close()
	if got, err := same.ForOwner(7).Link(ctx, c.Meeting.ID, t0); err != nil || got != c.JoinCode {
		t.Fatalf("positive control: under the same key = %q, %v", got, err)
	}
}

func TestOpenAndParseLinkKeyRefuseAnythingButThirtyTwoBytes(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []int{0, 16, 31, 33} {
		if _, err := store.Open(filepath.Join(dir, "x.db"), make([]byte, n)); err == nil {
			t.Errorf("Open with a %d-byte key succeeded", n)
		}
		if _, err := store.ParseLinkKey(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, n))); err == nil {
			t.Errorf("ParseLinkKey accepted %d bytes", n)
		}
	}
	for _, s := range []string{base64.StdEncoding.EncodeToString(keyA), base64.RawURLEncoding.EncodeToString(keyA), " " + base64.StdEncoding.EncodeToString(keyA) + "\n"} {
		k, err := store.ParseLinkKey(s)
		if err != nil || !bytes.Equal(k, keyA) {
			t.Errorf("ParseLinkKey(%q) = %v", s, err)
		}
	}
	if _, err := store.ParseLinkKey("not base64 at all!"); err == nil || strings.Contains(err.Error(), "not base64") {
		t.Errorf("a malformed key: %v", err)
	}
}
