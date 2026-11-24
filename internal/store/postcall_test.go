package store_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/store"
)

// Migration 7 (the post-call pass) on a schema-6 database: every row is
// kept, nothing has an address, no run and no draft exists, and the schema
// is this build's.
func TestMigration7KeepsEveryRowAndHoldsNoAddress(t *testing.T) {
	p := filepath.Join(t.TempDir(), "m6.db")
	db, err := sql.Open("sqlite", "file:"+p+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range store.MigrationsBeforePostCallForTest {
		if _, err := db.Exec(m); err != nil {
			t.Fatalf("migration %d: %v", i+1, err)
		}
	}
	for _, q := range []string{
		`PRAGMA user_version = 6`,
		`INSERT INTO owners (subject, display_name, updated_at) VALUES ('U3ViamVjdE1pZ3JhdGlvbj', 'Ada L.', 1)`,
		`INSERT INTO meetings (id, subject, title, room, join_code_sha256, state, record, ai, created_at, ended_at)
		 VALUES ('bWlncmF0aW9uN21l', 'U3ViamVjdE1pZ3JhdGlvbj', 'old', 'meeting-m7', 'hash-m7', 'ended', 1, 1, 1000, 7000)`,
		`INSERT INTO participants (id, meeting_id, kind, display_name, identity, ticket_sha256, state, knocked_at, admitted_at, left_at, card_state)
		 VALUES ('cGFydGljaXBhbnQ3', 'bWlncmF0aW9uN21l', 'guest', 'Grace H.', 'guest-m7', 'ticket-m7', 'ended', 1, 1, 2, 'sent')`,
		`INSERT INTO recording_shares (token_sha256, meeting_id, created_at) VALUES ('h7', 'bWlncmF0aW9uN21l', 5)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	s, err := store.Open(p, keyA)
	if err != nil {
		t.Fatalf("opening a schema-6 database: %v", err)
	}
	defer s.Close()
	db, _ = sql.Open("sqlite", "file:"+p+"?mode=ro")
	defer db.Close()
	var v, runs, drafts int
	var email, card sql.NullString
	var kind, title string
	db.QueryRow(`PRAGMA user_version`).Scan(&v)
	db.QueryRow(`SELECT email FROM owners`).Scan(&email)
	db.QueryRow(`SELECT card_email FROM participants`).Scan(&card)
	db.QueryRow(`SELECT kind FROM recording_shares`).Scan(&kind)
	db.QueryRow(`SELECT title FROM meetings`).Scan(&title)
	db.QueryRow(`SELECT (SELECT count(*) FROM ai_runs), (SELECT count(*) FROM mail_drafts)`).Scan(&runs, &drafts)
	if v != store.SchemaForTest || email.Valid || card.Valid || kind != "owner" || title != "old" || runs != 0 || drafts != 0 {
		t.Fatalf("after migration 7: schema %d, owner email %v, card email %v, share kind %q, title %q, %d runs, %d drafts", v, email, card, kind, title, runs, drafts)
	}
}

const (
	pidGrace = "Z3JhY2VwYXJ0aWNp"
	pidLinus = "bGludXNwYXJ0aWNp"
)

// ended is an ended meeting of subA's with two guests who sent the card,
// and a finished post run whose items are its argument.
func ended(t *testing.T, s *store.Store, r *store.OwnerRepo) (string, int64) {
	t.Helper()
	c, err := r.CreateMeeting(ctx, t0)
	if err != nil {
		t.Fatal(err)
	}
	id := c.Meeting.ID
	for _, q := range []struct {
		q    string
		args []any
	}{
		{`UPDATE meetings SET state = 'ended', ended_at = ? WHERE id = ?`, []any{t0.Add(time.Hour).UnixMilli(), id}},
		{`INSERT INTO participants (id, meeting_id, kind, display_name, identity, ticket_sha256, state, knocked_at, card_email) VALUES (?, ?, 'guest', 'Grace H.', 'g1', 't1', 'ended', 1, 'grace@example.test')`, []any{pidGrace, id}},
		{`INSERT INTO participants (id, meeting_id, kind, display_name, identity, ticket_sha256, state, knocked_at, card_email) VALUES (?, ?, 'guest', 'Linus T.', 'g2', 't2', 'ended', 1, 'linus@example.test')`, []any{pidLinus, id}},
	} {
		if err := s.ExecForTest(q.q, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	run := postRun(t, s, id, "model/a", []store.NewItem{{Source: store.SrcTranscript, Text: "Send the shortlist", Assignee: "Grace H.", Segments: []int64{1}}})
	return id, run
}

func postRun(t *testing.T, s *store.Store, meetingID, model string, items []store.NewItem) int64 {
	t.Helper()
	at := t0.Add(2 * time.Hour)
	if _, err := s.QueuePost(ctx, meetingID, model, `{"data_collection":"deny"}`, "", "", "owner", at); err != nil {
		t.Fatal(err)
	}
	run, ok, err := s.ClaimRun(ctx, at)
	if !ok || err != nil {
		t.Fatalf("claim = %v, %v", ok, err)
	}
	if _, err := s.FinishRun(ctx, run, store.Outcome{Items: items}, true, at); err != nil {
		t.Fatal(err)
	}
	return run.ID
}

func confirm(t *testing.T, s *store.Store, r *store.OwnerRepo) {
	t.Helper()
	tok, _, err := r.SetPendingEmail(ctx, "ada@example.test", t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmEmail(ctx, tok, t0); err != nil {
		t.Fatal(err)
	}
}

func TestAFinishedRunDraftsTheOwnerAndEveryCardGuest(t *testing.T) {
	s, _ := open(t)
	r := owner(t, s, subA, t0)
	id, run := ended(t, s, r)
	ds, err := r.Drafts(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, d := range ds {
		got = append(got, d.Recipient+":"+d.Name+":"+d.Email+":"+d.State)
		if d.RunID != run || len(d.Items) != 1 || d.Items[0].Text != "Send the shortlist" {
			t.Errorf("draft %s = %+v", d.Recipient, d)
		}
	}
	want := "owner:fixture name::draft " + pidGrace + ":Grace H.:grace@example.test:draft " + pidLinus + ":Linus T.:linus@example.test:draft"
	if strings.Join(got, " ") != want {
		t.Errorf("drafts = %s\nwant %s", strings.Join(got, " "), want)
	}
}

// 🔴 Nothing is queued without the owner's confirmed address.
func TestApproveRefusesAnUnconfirmedOwnerAddress(t *testing.T) {
	s, _ := open(t)
	r := owner(t, s, subA, t0)
	id, _ := ended(t, s, r)
	if _, _, err := r.SetPendingEmail(ctx, "ada@example.test", t0); err != nil {
		t.Fatal(err)
	}
	n, err := r.Approve(ctx, id, nil, t0, nil)
	if !errors.Is(err, store.ErrNoOwnerEmail) || n != 0 {
		t.Fatalf("Approve with the address pending = %d, %v; want ErrNoOwnerEmail (an unconfirmed owner address was used)", n, err)
	}
	if _, ok, _ := s.ClaimMail(ctx, t0.Add(time.Hour)); ok {
		t.Fatal("a draft was queued with the owner's address unconfirmed")
	}
}

func TestApproveQueuesDraftsAndNotTheRemoved(t *testing.T) {
	s, _ := open(t)
	r := owner(t, s, subA, t0)
	id, _ := ended(t, s, r)
	confirm(t, s, r)
	ds, _ := r.Drafts(ctx, id)
	yes := true
	if _, err := r.EditDraft(ctx, id, ds[2].ID, nil, &yes, nil, t0); err != nil {
		t.Fatal(err)
	}
	n, err := r.Approve(ctx, id, nil, t0, nil)
	if err != nil || n != 2 {
		t.Fatalf("Approve = %d, %v; want 2", n, err)
	}
	st := s.DraftStatesForTest(id)
	if st["owner"][0] != "queued" || st[pidGrace][0] != "queued" || st[pidLinus][0] != "removed" {
		t.Errorf("states = %v", st)
	}
	if _, err := r.EditDraft(ctx, id, ds[1].ID, &[]store.DraftItem{}, nil, nil, t0); !errors.Is(err, store.ErrDraftLocked) {
		t.Errorf("editing a queued draft = %v, want ErrDraftLocked", err)
	}
}

// 🔴 A reprocess makes new drafts only for recipients not yet sent to; a
// sent draft is byte for byte unchanged, and a revert to the older run
// redrafts from it.
func TestReprocessLeavesSentDraftsAndRevertRedrafts(t *testing.T) {
	s, _ := open(t)
	r := owner(t, s, subA, t0)
	id, run1 := ended(t, s, r)
	confirm(t, s, r)
	ds, _ := r.Drafts(ctx, id)
	if _, err := r.Approve(ctx, id, []string{ds[1].ID}, t0, nil); err != nil { // Grace's only
		t.Fatal(err)
	}
	m, ok, err := s.ClaimMail(ctx, t0.Add(time.Hour))
	if !ok || err != nil || m.Recipient != pidGrace {
		t.Fatalf("claim = %+v %v %v", m, ok, err)
	}
	if err := s.MailSent(ctx, m.ID, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	before, _ := r.Drafts(ctx, id)
	run2 := postRun(t, s, id, "model/b", []store.NewItem{{Source: store.SrcTranscript, Text: "Book the venue", Assignee: "Linus T.", Segments: []int64{2}}})
	after, _ := r.Drafts(ctx, id)
	byRcpt := func(ds []store.Draft) map[string]store.Draft {
		out := map[string]store.Draft{}
		for _, d := range ds {
			out[d.Recipient] = d
		}
		return out
	}
	b, a := byRcpt(before), byRcpt(after)
	if a[pidGrace].ID != b[pidGrace].ID || a[pidGrace].State != "sent" || a[pidGrace].Items[0].Text != "Send the shortlist" || a[pidGrace].RunID != run1 {
		t.Errorf("a sent draft was changed by reprocess: before %+v after %+v", b[pidGrace], a[pidGrace])
	}
	if a[pidGrace].Email != "" {
		t.Errorf("a sent draft kept the guest's address")
	}
	for _, k := range []string{"owner", pidLinus} {
		if a[k].RunID != run2 || a[k].State != "draft" || a[k].Items[0].Text != "Book the venue" {
			t.Errorf("draft %s after reprocess = %+v, want a new draft of run %d", k, a[k], run2)
		}
	}
	cur, err := s.CurrentRun(ctx, id)
	if err != nil || cur.ID != run2 {
		t.Errorf("current = %d %v, want the newest run %d", cur.ID, err, run2)
	}
	res, err := r.SetCurrent(ctx, id, run1, true, t0.Add(3*time.Hour))
	if err != nil || res.Made != 2 || res.AlreadySent != 1 {
		t.Fatalf("revert = %+v, %v; want 2 made, 1 already sent", res, err)
	}
	reverted := byRcpt(func() []store.Draft { d, _ := r.Drafts(ctx, id); return d }())
	if reverted[pidLinus].RunID != run1 || reverted[pidLinus].Items[0].Text != "Send the shortlist" || reverted[pidGrace].ID != b[pidGrace].ID {
		t.Errorf("after revert: %+v", reverted)
	}
	if cur, _ := s.CurrentRun(ctx, id); cur.ID != run1 {
		t.Errorf("current after revert = %d, want %d", cur.ID, run1)
	}
}

// 🔴 One send per recipient per meeting, by the schema itself.
func TestASecondSendToOneRecipientIsRefusedByTheSchema(t *testing.T) {
	s, _ := open(t)
	r := owner(t, s, subA, t0)
	ended(t, s, r)
	err := s.ExecForTest(`INSERT INTO mail_drafts (id, meeting_id, run_id, recipient, name, items, state, created_at, updated_at)
		SELECT 'AAAAAAAAAAAAAAAAAAAAAA', meeting_id, run_id, recipient, name, items, 'sent', 1, 1 FROM mail_drafts WHERE recipient = ?`, pidGrace)
	if err != nil {
		t.Fatal(err)
	}
	err = s.ExecForTest(`UPDATE mail_drafts SET state = 'queued' WHERE recipient = ? AND state = 'draft'`, pidGrace)
	if err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("queueing a second email to a recipient = %v, want a UNIQUE refusal", err)
	}
}

// 🔴 Another owner sees none of it.
func TestRunsAndDraftsAreTheOwnersAlone(t *testing.T) {
	s, _ := open(t)
	r := owner(t, s, subA, t0)
	id, run := ended(t, s, r)
	other := owner(t, s, subB, t0)
	if _, err := other.Drafts(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("another owner's Drafts = %v", err)
	}
	if _, err := other.Runs(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("another owner's Runs = %v", err)
	}
	if _, err := other.SetCurrent(ctx, id, run, true, t0); !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrNoRun) {
		t.Errorf("another owner's SetCurrent = %v", err)
	}
	ds, _ := r.Drafts(ctx, id)
	if _, err := other.EditDraft(ctx, id, ds[0].ID, &[]store.DraftItem{}, nil, nil, t0); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("another owner's EditDraft = %v", err)
	}
	confirm(t, s, other)
	if n, err := other.Approve(ctx, id, nil, t0, nil); n != 0 || !errors.Is(err, store.ErrNotFound) {
		t.Errorf("another owner's Approve = %d, %v", n, err)
	}
}

// 🔴 The confirmation door: a malformed, unknown, expired or spent token is
// the same ErrNotFound.
func TestTheConfirmationTokenDoor(t *testing.T) {
	s, _ := open(t)
	r := owner(t, s, subA, t0)
	tok, exp, err := r.SetPendingEmail(ctx, "ada@example.test", t0)
	if err != nil || !exp.Equal(t0.Add(store.EmailTokenLifetime)) {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		tok string
		at  time.Time
	}{
		"malformed": {"x", t0}, "unknown": {strings.Repeat("A", 43), t0}, "expired": {tok, exp},
	} {
		if _, err := s.PendingByToken(ctx, c.tok, c.at); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s token = %v", name, err)
		}
		if err := s.ConfirmEmail(ctx, c.tok, c.at); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s token confirmed: %v", name, err)
		}
	}
	if e, _ := r.Email(ctx, t0); e.Confirmed != "" || e.Pending != "ada@example.test" {
		t.Errorf("before confirming = %+v", e)
	}
	if err := s.ConfirmEmail(ctx, tok, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmEmail(ctx, tok, t0.Add(time.Minute)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a spent token = %v", err)
	}
	if e, _ := r.Email(ctx, t0); e.Confirmed != "ada@example.test" || e.Pending != "" {
		t.Errorf("after confirming = %+v", e)
	}
}

func TestGuestAddressesArePurgedAfterTheKeep(t *testing.T) {
	s, p := open(t)
	r := owner(t, s, subA, t0)
	id, _ := ended(t, s, r)
	at := t0.Add(time.Hour + store.MailKeepDays*24*time.Hour)
	if n, err := s.PurgeMail(ctx, at.Add(-time.Second)); n != 0 || err != nil {
		t.Fatalf("purge before the keep = %d %v", n, err)
	}
	if n, err := s.PurgeMail(ctx, at); n != 2 || err != nil {
		t.Fatalf("purge = %d %v, want 2", n, err)
	}
	db, _ := sql.Open("sqlite", "file:"+p+"?mode=ro")
	defer db.Close()
	var left int
	db.QueryRow(`SELECT (SELECT count(*) FROM participants WHERE card_email IS NOT NULL) + (SELECT count(*) FROM mail_drafts WHERE email IS NOT NULL)`).Scan(&left)
	if left != 0 {
		t.Errorf("%d addresses left after the purge", left)
	}
	confirm(t, s, r)
	if n, _ := r.Approve(ctx, id, nil, t0, nil); n != 1 { // the owner's only
		t.Errorf("approve after purge queued %d, want the owner's 1", n)
	}
}

// 🔴 A reprocess keeps "Don't send" and the owner's edits: a removed guest
// stays removed, an edit carries to the new run's item with the same text,
// and when an edited item is gone the draft takes the new items and says
// the edits were not carried.
func TestReprocessCarriesRemovedAndEdits(t *testing.T) {
	s, _ := open(t)
	r := owner(t, s, subA, t0)
	id, _ := ended(t, s, r)
	ds, _ := r.Drafts(ctx, id)
	yes := true
	edit := []store.DraftItem{{Text: "Send the shortlist", Assignee: "Linus T.", Due: "Friday"}}
	if _, err := r.EditDraft(ctx, id, ds[1].ID, &edit, nil, nil, t0); err != nil { // Grace's
		t.Fatal(err)
	}
	if _, err := r.EditDraft(ctx, id, ds[2].ID, nil, &yes, nil, t0); err != nil { // Linus: don't send
		t.Fatal(err)
	}
	byRcpt := func() map[string]store.Draft {
		ds, _ := r.Drafts(ctx, id)
		out := map[string]store.Draft{}
		for _, d := range ds {
			out[d.Recipient] = d
		}
		return out
	}
	postRun(t, s, id, "model/b", []store.NewItem{
		{Source: store.SrcTranscript, Text: "Send the shortlist", Assignee: "Grace H.", Segments: []int64{1}},
		{Source: store.SrcTranscript, Text: "Book the venue", Assignee: "Ada L.", Segments: []int64{2}}})
	d := byRcpt()
	if d[pidLinus].State != store.DraftRemoved {
		t.Errorf("a removed recipient was put back by reprocess: %+v", d[pidLinus])
	}
	if len(d[pidGrace].Items) != 2 || d[pidGrace].Items[0] != edit[0] || d[pidGrace].Items[1].Text != "Book the venue" || d[pidGrace].ItemsChanged {
		t.Errorf("Grace's edit was not carried to the same item: %+v", d[pidGrace])
	}
	if d["owner"].Items[0].Assignee != "Grace H." || d["owner"].ItemsChanged {
		t.Errorf("the owner's unedited draft: %+v", d["owner"])
	}
	postRun(t, s, id, "model/c", []store.NewItem{{Source: store.SrcTranscript, Text: "Confirm the date", Assignee: "Ada L.", Segments: []int64{1}}})
	d = byRcpt()
	if !d[pidGrace].ItemsChanged || len(d[pidGrace].Items) != 1 || d[pidGrace].Items[0].Text != "Confirm the date" {
		t.Errorf("an edit whose item is gone: %+v, want the new items flagged", d[pidGrace])
	}
	if d[pidLinus].State != store.DraftRemoved {
		t.Errorf("removed lost on the second reprocess: %+v", d[pidLinus])
	}
}
