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

var ctx = context.Background()

// Two link keys, neither all zeros nor any pattern the code names.
var (
	keyA = func() []byte { h := sha256.Sum256([]byte("fixture link key A")); return h[:] }()
	keyB = func() []byte { h := sha256.Sum256([]byte("fixture link key B")); return h[:] }()
)

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

// owner returns a repository for ownerID built from a live session (minted
// through plugin ownerID+100), as the server builds one.
func owner(t *testing.T, s *store.Store, ownerID int64, at time.Time) *store.OwnerRepo {
	t.Helper()
	tok, _, err := s.CreateSession(ctx, ownerID, ownerID+100, at)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := s.LookupSession(ctx, tok, at)
	if err != nil {
		t.Fatal(err)
	}
	return s.ForSession(sess)
}

func mk(t *testing.T, s *store.Store, ownerID int64, at time.Time) store.Created {
	t.Helper()
	c, err := owner(t, s, ownerID, at).CreateMeeting(ctx, at)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func dbBytes(t *testing.T, p string) []byte {
	t.Helper()
	var all []byte
	for _, f := range []string{p, p + "-wal"} {
		b, err := os.ReadFile(f)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		all = append(all, b...)
	}
	return all
}

// ---- the link: entropy, format, and what it is derived from ----

var joinCodeShape = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)

// "Join code: 22 characters, base64url, distinct across 10,000 draws."
// Each draw is a new meeting id under one key.
func TestJoinCodeIs128BitsIn22Base64urlCharacters(t *testing.T) {
	s, _ := open(t)
	seen := map[string]bool{}
	for i := 0; i < 10_000; i++ {
		c := s.JoinCode(store.NewMeetingID())
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

// The code is HMAC-SHA256(key, "muster-meetings link v1\0" + id), cut to
// 128 bits. The literal was computed outside this code (Python's hmac), so
// the domain string, the key and the id are all pinned; then each input is
// varied alone and the code must move.
func TestJoinCodeIsTheKeyedHMACOfTheMeetingID(t *testing.T) {
	s, p := open(t)
	const id = "AbCdEfGhIjKlMnOp"
	if got, want := s.JoinCode(id), "OT2SbON65PHyZHm3YY8xoA"; got != want {
		t.Fatalf("JoinCode(%q) = %q, want %q", id, got, want)
	}
	if s.JoinCode(id) != s.JoinCode(id) {
		t.Fatal("the same key and id gave two codes")
	}
	if s.JoinCode("AbCdEfGhIjKlMnOq") == s.JoinCode(id) {
		t.Fatal("another meeting id gave the same code")
	}
	s.Close()
	other, err := store.Open(p, keyB)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if other.JoinCode(id) == "OT2SbON65PHyZHm3YY8xoA" {
		t.Fatal("another key gave the same code")
	}
}

// Another key, so another link: rotating MEETINGS_LINK_KEY changes the link
// an open meeting answers its owner.
func TestAnotherKeyGivesTheOwnerAnotherLink(t *testing.T) {
	s, p := open(t)
	c := mk(t, s, 7, t0)
	s.Close()
	other, err := store.Open(p, keyB)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	got, err := other.ForOwner(7).Link(ctx, c.Meeting.ID, t0)
	if err != nil || got == c.JoinCode || !joinCodeShape.MatchString(got) {
		t.Fatalf("under another key = %q, %v; want another well-formed code than %q", got, err, c.JoinCode)
	}
}

// The code is never written: neither the database file nor its WAL holds it,
// while both hold its SHA-256 and the meeting id (the positive controls: the
// scan sees the row).
func TestJoinCodeIsNeverStoredInClear(t *testing.T) {
	s, p := open(t)
	c := mk(t, s, 7, t0)
	all := dbBytes(t, p)
	if bytes.Contains(all, []byte(c.JoinCode)) {
		t.Fatalf("the join code itself is in the database")
	}
	if !bytes.Contains(all, []byte(store.Hash(c.JoinCode))) || !bytes.Contains(all, []byte(c.Meeting.ID)) {
		t.Fatal("positive control: the row's hash or id is not in the database files, so the scan cannot see rows")
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

func TestSessionTokenIsStoredOnlyHashed(t *testing.T) {
	s, p := open(t)
	tok, _, err := s.CreateSession(ctx, 7, 12, t0)
	if err != nil {
		t.Fatal(err)
	}
	all := dbBytes(t, p)
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
	if err != nil || got.OwnerID != 7 || got.PluginID != 12 || got.TokenSHA256 != store.Hash(tok) {
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

// ---- making a meeting needs a live session (the revocation race) ----

func TestCreatingAMeetingNeedsTheSessionStillLive(t *testing.T) {
	s, _ := open(t)
	if _, err := s.ForOwner(7).CreateMeeting(ctx, t0); err != store.ErrNoSession {
		t.Fatalf("a repository with no session made a meeting: %v", err)
	}
	r := owner(t, s, 7, t0)
	if _, err := s.RevokePlugin(ctx, 107, 7, true, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateMeeting(ctx, t0); err != store.ErrNoSession {
		t.Fatalf("after the session was revoked = %v, want ErrNoSession", err)
	}
	if list, _, _ := s.ForOwner(7).ListMeetings(ctx, nil, 10); len(list) != 0 {
		t.Fatalf("a revoked owner has %d meetings", len(list))
	}
	r = owner(t, s, 7, t0)
	if _, err := r.CreateMeeting(ctx, t0.Add(8*time.Hour)); err != store.ErrNoSession {
		t.Fatalf("with the session expired = %v, want ErrNoSession", err)
	}
	if _, err := r.CreateMeeting(ctx, t0.Add(8*time.Hour-time.Millisecond)); err != nil {
		t.Fatalf("positive control: a live session = %v", err)
	}
}

// ---- ownership, at the repository ----

// "Ownership: every repository query for owner A returns nothing of owner
// B's. Red if: the repository's owner filter is removed."
func TestOwnerRepoNeverReturnsAnotherOwnersMeetings(t *testing.T) {
	s, _ := open(t)
	a, b := s.ForOwner(7), s.ForOwner(41)
	ma1 := mk(t, s, 7, t0)
	mb := mk(t, s, 41, t0.Add(time.Minute))
	ma2 := mk(t, s, 7, t0.Add(2*time.Minute))

	list, next, err := a.ListMeetings(ctx, nil, 50)
	if err != nil || next != nil || len(list) != 2 || list[0].ID != ma2.Meeting.ID || list[1].ID != ma1.Meeting.ID {
		t.Fatalf("A's list = %+v, next %v, %v; want A's two, newest first", list, next, err)
	}
	list, _, _ = b.ListMeetings(ctx, nil, 50)
	if len(list) != 1 || list[0].ID != mb.Meeting.ID {
		t.Fatalf("B's list = %+v, want B's one", list)
	}
	if _, err := a.Meeting(ctx, mb.Meeting.ID); err != store.ErrNotFound {
		t.Fatalf("A reading B's meeting = %v, want ErrNotFound", err)
	}
	if m, err := b.Meeting(ctx, mb.Meeting.ID); err != nil || m.ID != mb.Meeting.ID {
		t.Fatalf("positive control: B reading its own = %+v %v", m, err)
	}
	// A cursor pointing into B's rows still pages only A's.
	list, _, _ = a.ListMeetings(ctx, &store.Cursor{CreatedAt: t0.Add(time.Hour).UnixMilli(), ID: mb.Meeting.ID}, 50)
	if len(list) != 2 {
		t.Fatalf("A's list after a cursor = %d rows, want 2", len(list))
	}
}

func TestListPagesNewestFirst(t *testing.T) {
	s, _ := open(t)
	r := owner(t, s, 7, t0)
	var ids []string
	for i := 0; i < 5; i++ {
		c, err := r.CreateMeeting(ctx, t0.Add(time.Duration(i)*time.Second))
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

func TestANewMeetingIsOpenWithRecordAndAIOnAndLinkFor24Hours(t *testing.T) {
	s, _ := open(t)
	c := mk(t, s, 7, t0)
	m := c.Meeting
	if m.State != "open" || !m.Record || !m.AI || !m.CreatedAt.Equal(t0.Truncate(time.Millisecond)) ||
		!m.LinkExpiresAt.Equal(m.CreatedAt.Add(24*time.Hour)) || m.StartedAt != nil || m.EndedAt != nil || m.Title != "" {
		t.Fatalf("new meeting = %+v", m)
	}
	if c.JoinCode != s.JoinCode(m.ID) {
		t.Fatal("the created code is not the meeting's derived code")
	}
}

// ---- the owner's link: the owner's only, open only ----

func TestOnlyTheOwnerReadsTheLink(t *testing.T) {
	s, _ := open(t)
	c := mk(t, s, 7, t0)
	if got, err := s.ForOwner(41).Link(ctx, c.Meeting.ID, t0); err != store.ErrNotFound || got != "" {
		t.Fatalf("another owner reading the link = %q, %v; want ErrNotFound", got, err)
	}
	if got, err := s.ForOwner(7).Link(ctx, c.Meeting.ID, t0); err != nil || got != c.JoinCode {
		t.Fatalf("positive control: the owner's link = %q, %v; want the code made with it", got, err)
	}
}

func TestAnEndedMeetingHasNoLink(t *testing.T) {
	s, _ := open(t)
	c := mk(t, s, 7, t0)
	if _, err := s.RevokePlugin(ctx, 107, 7, true, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ForOwner(7).Link(ctx, c.Meeting.ID, t0.Add(time.Minute)); err != store.ErrNoLink || got != "" {
		t.Fatalf("an ended meeting's link = %q, %v; want ErrNoLink", got, err)
	}
}

// At 24 h the link is gone even before the sweep ends the meeting.
func TestTheLinkIsGoneAt24Hours(t *testing.T) {
	s, _ := open(t)
	c := mk(t, s, 7, t0)
	r := s.ForOwner(7)
	if got, err := r.Link(ctx, c.Meeting.ID, t0.Add(24*time.Hour-time.Millisecond)); err != nil || got != c.JoinCode {
		t.Fatalf("one ms before 24 h = %q, %v; want the link", got, err)
	}
	if got, err := r.Link(ctx, c.Meeting.ID, t0.Add(24*time.Hour)); err != store.ErrNoLink || got != "" {
		t.Fatalf("at 24 h = %q, %v; want ErrNoLink", got, err)
	}
}

// ---- revocation and its scope ----

func TestRevokePluginDeletesItsSessionsAndEndsTheOwnersOpenMeetings(t *testing.T) {
	s, _ := open(t)
	tA, _, _ := s.CreateSession(ctx, 7, 12, t0)
	tB, _, _ := s.CreateSession(ctx, 41, 19, t0)
	ma := mk(t, s, 7, t0)
	mb := mk(t, s, 41, t0)
	res, err := s.RevokePlugin(ctx, 12, 7, true, t0.Add(time.Minute))
	if err != nil || res.Sessions != 1 || res.Meetings != 1 {
		t.Fatalf("RevokePlugin = %+v, %v", res, err)
	}
	if _, err := s.LookupSession(ctx, tA, t0.Add(time.Minute)); err != store.ErrNotFound {
		t.Fatalf("A's session after revoke = %v", err)
	}
	if m, _ := s.ForOwner(7).Meeting(ctx, ma.Meeting.ID); m.State != "ended" || m.EndedAt == nil {
		t.Fatalf("A's meeting after revoke = %+v", m)
	}
	// B, served by another token, is untouched.
	if _, err := s.LookupSession(ctx, tB, t0.Add(time.Minute)); err != nil {
		t.Fatalf("B's session after A's revoke = %v", err)
	}
	if m, _ := s.ForOwner(41).Meeting(ctx, mb.Meeting.ID); m.State != "open" {
		t.Fatalf("B's meeting after A's revoke = %+v", m)
	}
}

func TestSweepEndsAMeetingAt24HoursAndNotBefore(t *testing.T) {
	s, _ := open(t)
	c := mk(t, s, 7, t0)
	if n, err := s.Sweep(ctx, t0.Add(24*time.Hour-time.Millisecond)); err != nil || n != 0 {
		t.Fatalf("sweep one ms short = %d, %v", n, err)
	}
	if n, err := s.Sweep(ctx, t0.Add(24*time.Hour)); err != nil || n != 1 {
		t.Fatalf("sweep at 24 h = %d, %v", n, err)
	}
	if m, _ := s.ForOwner(7).Meeting(ctx, c.Meeting.ID); m.State != "ended" {
		t.Fatalf("after sweep = %+v", m)
	}
}

func TestReconcileLiveDropsWhatNoLiveTokenServes(t *testing.T) {
	s, _ := open(t)
	dead, _, _ := s.CreateSession(ctx, 7, 12, t0)
	live, _, _ := s.CreateSession(ctx, 41, 19, t0)
	ma := mk(t, s, 7, t0)
	mb := mk(t, s, 41, t0)
	res, err := s.ReconcileLive(ctx, []int64{19, 141}, []int64{41}, t0)
	if err != nil || res.Sessions != 2 || res.Meetings != 1 {
		t.Fatalf("ReconcileLive = %+v, %v", res, err)
	}
	if _, err := s.LookupSession(ctx, dead, t0); err != store.ErrNotFound {
		t.Fatal("a session of a dead token survived")
	}
	if _, err := s.LookupSession(ctx, live, t0); err != nil {
		t.Fatal("a session of a live token was dropped")
	}
	if m, _ := s.ForOwner(7).Meeting(ctx, ma.Meeting.ID); m.State != "ended" {
		t.Fatal("an unserved owner's meeting stayed open")
	}
	if m, _ := s.ForOwner(41).Meeting(ctx, mb.Meeting.ID); m.State != "open" {
		t.Fatal("a served owner's meeting was ended")
	}
}

func TestReopenKeepsDataAndRefusesANewerSchema(t *testing.T) {
	s, p := open(t)
	c := mk(t, s, 7, t0)
	s.Close()
	s2, err := store.Open(p, keyA)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := s2.ForOwner(7).Meeting(ctx, c.Meeting.ID); err != nil || m.ID != c.Meeting.ID {
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
