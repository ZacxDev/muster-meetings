package store_test

// The owner's MCP tokens (migration 8) and the writes an MCP repository may
// make: the token is kept only as its hash, it opens only while unrevoked,
// muster revoking the owner (or the app) revokes it, and a write through
// it re-checks it in its own transaction.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/store"
)

func mintMCP(t *testing.T, r *store.OwnerRepo, at time.Time) (string, store.MCPToken) {
	t.Helper()
	tok, mt, err := r.MintMCPToken(ctx, at)
	if err != nil {
		t.Fatal(err)
	}
	return tok, mt
}

// 🔴 Stored only as its hash; shown once; opens until revoked, and a
// revoked, malformed or other-prefixed token is ErrNotFound alike.
func TestAnMCPTokenIsKeptAsItsHashAndOpensUntilRevoked(t *testing.T) {
	s, p := open(t)
	r := owner(t, s, subA, t0)
	tok, mt := mintMCP(t, r, t0)
	if !strings.HasPrefix(tok, "mmc_") || len(tok) != 47 {
		t.Fatalf("token %q: want mmc_ and 43 characters", tok)
	}
	s.Close()
	if raw := dbBytes(t, p); strings.Contains(string(raw), tok) || strings.Contains(string(raw), tok[4:]) {
		t.Fatal("the database holds the token itself")
	}
	s, err := store.Open(p, keyA)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	got, err := s.LookupMCPToken(ctx, tok, t0.Add(time.Minute))
	if err != nil || got.ID != mt.ID || got.Subject != subA || got.LastUsedAt == nil || !got.LastUsedAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("lookup = %+v, %v", got, err)
	}
	for _, bad := range []string{"", tok[:len(tok)-1], "mmo_" + tok[4:], "mmr_" + tok[4:], tok + "x"} {
		if _, err := s.LookupMCPToken(ctx, bad, t0); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("lookup %q = %v, want ErrNotFound", bad, err)
		}
	}
	r = owner(t, s, subA, t0)
	if err := r.RevokeMCPToken(ctx, mt.ID, t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LookupMCPToken(ctx, tok, t0.Add(3*time.Minute)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a revoked token = %v, want ErrNotFound", err)
	}
	if ts, _ := r.MCPTokens(ctx); len(ts) != 0 {
		t.Errorf("tokens after revoke = %+v", ts)
	}
}

// 🔴 One owner never lists, nor revokes, another's tokens; only a live
// session mints or revokes (an MCP repository cannot).
func TestMCPTokensAreTheOwnersAlone(t *testing.T) {
	s, _ := open(t)
	a, b := owner(t, s, subA, t0), owner(t, s, subB, t0)
	tokA, ma := mintMCP(t, a, t0)
	if ts, err := b.MCPTokens(ctx); err != nil || len(ts) != 0 {
		t.Fatalf("B's list = %+v, %v: holds A's token", ts, err)
	}
	if err := b.RevokeMCPToken(ctx, ma.ID, t0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("B revoking A's token = %v, want ErrNotFound", err)
	}
	if _, err := s.LookupMCPToken(ctx, tokA, t0); err != nil {
		t.Fatalf("A's token after B tried to revoke it = %v", err)
	}
	if ts, _ := a.MCPTokens(ctx); len(ts) != 1 || ts[0].ID != ma.ID {
		t.Fatalf("A's list = %+v", ts)
	}
	got, _ := s.LookupMCPToken(ctx, tokA, t0)
	m := s.ForMCP(got)
	if _, _, err := m.MintMCPToken(ctx, t0); !errors.Is(err, store.ErrNoSession) {
		t.Errorf("an MCP repository minting = %v, want ErrNoSession", err)
	}
	if err := m.RevokeMCPToken(ctx, ma.ID, t0); !errors.Is(err, store.ErrNoSession) {
		t.Errorf("an MCP repository revoking = %v, want ErrNoSession", err)
	}
	if _, err := s.ForSubject(subA).MCPTokens(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < store.MaxMCPTokens; i++ {
		mintMCP(t, a, t0)
	}
	if _, _, err := a.MintMCPToken(ctx, t0); !errors.Is(err, store.ErrTooManyTokens) {
		t.Errorf("mint past the cap = %v", err)
	}
}

// 🔴 muster revoking a person revokes their MCP tokens (made by then), and
// no one else's; an MCP token alone makes its owner a held subject (so the
// check reaches them); the app's revocation revokes every token.
func TestMusterRevocationRevokesMCPTokens(t *testing.T) {
	s, _ := open(t)
	a, b := owner(t, s, subA, t0), owner(t, s, subB, t0)
	tokA, _ := mintMCP(t, a, t0)
	tokB, _ := mintMCP(t, b, t0)
	// Every session expired: only the tokens hold A and B.
	at := t0.Add(9 * time.Hour)
	held, err := s.HeldSubjects(ctx, at)
	if err != nil || strings.Join(held, ",") != subA+","+subB {
		t.Fatalf("HeldSubjects = %v, %v; want the two token holders", held, err)
	}
	// A token A makes after the check was sent is newer than its answer.
	_, late := mintMCP(t, owner(t, s, subA, at), at.Add(time.Second))
	if _, err := s.EndSubjects(ctx, []string{subA}, at, at.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LookupMCPToken(ctx, tokA, at); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("A's token after muster revoked A = %v", err)
	}
	if _, err := s.LookupMCPToken(ctx, tokB, at); err != nil {
		t.Errorf("B's token after muster revoked A = %v (only A's may go)", err)
	}
	if ts, _ := s.ForSubject(subA).MCPTokens(ctx); len(ts) != 1 || ts[0].ID != late.ID {
		t.Errorf("A's tokens = %+v: want the one made after the check only", ts)
	}
	if _, err := s.EndAll(ctx, "fixture", at); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LookupMCPToken(ctx, tokB, at); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("B's token after the app was revoked = %v", err)
	}
	if ts, _ := s.ForSubject(subA).MCPTokens(ctx); len(ts) != 0 {
		t.Errorf("A's tokens after the app was revoked = %+v", ts)
	}
}

// 🔴 A write through an MCP repository (Send, Share, the owner's items)
// re-checks its token in the write's own transaction: once revoked, each
// is ErrNoSession and changes nothing. A ForSubject repository writes none.
func TestAnMCPRepositoryWritesOnlyWhileItsTokenStands(t *testing.T) {
	s, _ := open(t)
	r := owner(t, s, subA, t0)
	id, _ := ended(t, s, r)
	confirm(t, s, r)
	tok, mt := mintMCP(t, r, t0)
	got, err := s.LookupMCPToken(ctx, tok, t0)
	if err != nil {
		t.Fatal(err)
	}
	m := s.ForMCP(got)
	if err := r.RevokeMCPToken(ctx, mt.ID, t0); err != nil {
		t.Fatal(err)
	}
	if n, err := m.Approve(ctx, id, nil, t0, nil); n != 0 || !errors.Is(err, store.ErrNoSession) {
		t.Errorf("Send with a revoked token = %d, %v; want ErrNoSession", n, err)
	}
	if _, ok, _ := s.ClaimMail(ctx, t0.Add(time.Hour)); ok {
		t.Fatal("a draft was queued through a revoked token")
	}
	if _, _, err := m.AddOwnerRun(ctx, id, "mcp", nil, true, t0); !errors.Is(err, store.ErrNoSession) {
		t.Errorf("items with a revoked token = %v; want ErrNoSession", err)
	}
	if _, _, err := m.Share(ctx, id, t0); !errors.Is(err, store.ErrNoSession) {
		t.Errorf("share with a revoked token = %v; want ErrNoSession", err)
	}
	if _, _, err := s.ForSubject(subA).AddOwnerRun(ctx, id, "mcp", nil, true, t0); !errors.Is(err, store.ErrNoSession) {
		t.Errorf("items through a read-only repository = %v; want ErrNoSession", err)
	}
	// A live token sends, and the audit says it came through MCP.
	tok2, mt2 := mintMCP(t, r, t0)
	got2, _ := s.LookupMCPToken(ctx, tok2, t0)
	if n, err := s.ForMCP(got2).Approve(ctx, id, nil, t0, nil); err != nil || n != 3 {
		t.Fatalf("Send with a live token = %d, %v", n, err)
	}
	var detail string
	if err := s.QueryRowForTest(`SELECT detail FROM audit WHERE event = 'notes.approved' ORDER BY rowid DESC LIMIT 1`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if detail != `{"queued":3,"via":"mcp","token":"`+mt2.ID+`"}` {
		t.Errorf("audit = %s", detail)
	}
}

// 🔴 The owner's own items are a new FINISHED current run: never queued,
// so the post-call worker never claims it (nothing of a meeting goes to a
// model through it); drafts are redrawn from it; a recipient already sent
// to is left alone; an open meeting is refused; a meeting whose AI was off
// is allowed.
func TestAnOwnerRunIsFinishedCurrentAndNeverClaimed(t *testing.T) {
	s, _ := open(t)
	r := owner(t, s, subA, t0)
	id, first := ended(t, s, r)
	if err := s.ExecForTest(`UPDATE meetings SET ai = 0 WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	confirm(t, s, r)
	ds, _ := r.Drafts(ctx, id)
	if _, err := r.Approve(ctx, id, []string{ds[1].ID}, t0, nil); err != nil { // Grace's only
		t.Fatal(err)
	}
	its := []store.NewItem{{Source: store.SrcTranscript, Text: "Order the flowers", Assignee: "fixture name"}}
	run, res, err := r.AddOwnerRun(ctx, id, "mcp", its, true, t0.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if run.ID == first || run.Kind != "post" || run.State != "done" || !run.Current || run.ItemsModel != "mcp" || run.RequestedBy != "owner" {
		t.Fatalf("run = %+v", run)
	}
	if res.Made != 2 || res.AlreadySent != 1 {
		t.Errorf("redraft = %+v, want 2 made (owner, Linus) and Grace left alone", res)
	}
	if _, ok, err := s.ClaimRun(ctx, t0.Add(48*time.Hour)); ok || err != nil {
		t.Fatalf("the worker claimed a run (%v): the owner's run must never be queued", err)
	}
	ds, _ = r.Drafts(ctx, id)
	var got []string
	for _, d := range ds {
		var texts []string
		for _, it := range d.Items {
			texts = append(texts, it.Text)
		}
		got = append(got, d.Name+":"+d.State+":"+strings.Join(texts, "+"))
	}
	if strings.Join(got, " ") != "fixture name:draft:Order the flowers Grace H.:queued:Send the shortlist Linus T.:draft:Order the flowers" {
		t.Errorf("drafts = %v", got)
	}
	// An open meeting is refused.
	c, _ := r.CreateMeeting(ctx, t0)
	if _, _, err := r.AddOwnerRun(ctx, c.Meeting.ID, "mcp", its, true, t0); !errors.Is(err, store.ErrMeetingOpen) {
		t.Errorf("an open meeting = %v, want ErrMeetingOpen", err)
	}
	// Another owner's meeting is not found.
	if _, _, err := owner(t, s, subB, t0).AddOwnerRun(ctx, id, "mcp", its, true, t0); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("another owner's meeting = %v, want ErrNotFound", err)
	}
}
