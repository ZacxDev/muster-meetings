package store

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"regexp"
	"time"
)

// The owner's MCP tokens (migration 8; README: "Using Meetings from Claude
// Code (MCP)"): a bearer the owner mints in Meetings settings and gives to
// their own Claude Code session, which reaches POST /mcp with it and
// nothing else. 🔴 Stored only as its SHA-256 and shown once; no expiry
// (the owner revokes it, and muster ending the app for them revokes it:
// EndSubjects, EndAll); it opens no owner-session route (its prefix is not
// mmo_), and an agent read token (mmr_) does not open /mcp.

// MCPTokenPrefix starts every MCP token.
const MCPTokenPrefix = "mmc_"

// MaxMCPTokens is the most unrevoked MCP tokens one owner may hold.
const MaxMCPTokens = 10

var (
	mcpTokenRE   = regexp.MustCompile(`^mmc_[A-Za-z0-9_-]{43}$`)
	mcpTokenIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{12}$`)
)

// ValidMCPToken checks an MCP token's shape before it is hashed.
func ValidMCPToken(s string) bool { return mcpTokenRE.MatchString(s) }

// MCPToken is one of an owner's MCP tokens, as their settings list shows it
// (never the token itself).
type MCPToken struct {
	ID         string     `json:"id"`
	Subject    string     `json:"-"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

// ErrTooManyTokens is a mint past MaxMCPTokens.
var ErrTooManyTokens = errors.New("too many MCP tokens")

// MintMCPToken makes a new MCP token for the owner: only a live owner
// session may (the MCP door itself mints none).
func (r *OwnerRepo) MintMCPToken(ctx context.Context, now time.Time) (string, MCPToken, error) {
	if r.session == "" {
		return "", MCPToken{}, ErrNoSession
	}
	tx, err := r.s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", MCPToken{}, err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM mcp_tokens WHERE subject = ? AND revoked_at IS NULL`, r.subject).Scan(&n); err != nil {
		return "", MCPToken{}, err
	}
	if n >= MaxMCPTokens {
		return "", MCPToken{}, ErrTooManyTokens
	}
	tok := MCPTokenPrefix + random(32)
	t := MCPToken{ID: random(9), Subject: r.subject, CreatedAt: time.UnixMilli(ms(now)).UTC()}
	// The insert happens only while the session still exists (as
	// CreateMeeting's): a mint that raced a revocation makes nothing.
	res, err := tx.ExecContext(ctx, `INSERT INTO mcp_tokens (id, token_sha256, subject, created_at)
		SELECT ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM owner_sessions WHERE token_sha256 = ? AND subject = ? AND expires_at > ?)`,
		t.ID, Hash(tok), r.subject, ms(now), r.session, r.subject, ms(now))
	if err != nil {
		return "", MCPToken{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return "", MCPToken{}, ErrNoSession
	}
	if err := audit(ctx, tx, now, r.subject, "", "mcp_token.minted", `{"token":`+jsonString(t.ID)+`}`); err != nil {
		return "", MCPToken{}, err
	}
	return tok, t, tx.Commit()
}

// MCPTokens is the owner's unrevoked MCP tokens, newest first.
func (r *OwnerRepo) MCPTokens(ctx context.Context) ([]MCPToken, error) {
	rows, err := r.s.db.QueryContext(ctx, `SELECT id, created_at, last_used_at FROM mcp_tokens WHERE subject = ? AND revoked_at IS NULL ORDER BY created_at DESC, id`, r.subject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MCPToken{}
	for rows.Next() {
		var t MCPToken
		var created int64
		var used sql.NullInt64
		if err := rows.Scan(&t.ID, &created, &used); err != nil {
			return nil, err
		}
		t.Subject, t.CreatedAt, t.LastUsedAt = r.subject, time.UnixMilli(created).UTC(), fromMS(used)
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeMCPToken revokes one of the owner's MCP tokens by its id; another
// owner's id, an unknown or a revoked one is ErrNotFound. Only a live owner
// session may.
func (r *OwnerRepo) RevokeMCPToken(ctx context.Context, id string, now time.Time) error {
	if r.session == "" {
		return ErrNoSession
	}
	if !mcpTokenIDRE.MatchString(id) {
		return ErrNotFound
	}
	res, err := r.s.db.ExecContext(ctx, `UPDATE mcp_tokens SET revoked_at = ? WHERE id = ? AND subject = ? AND revoked_at IS NULL`, ms(now), id, r.subject)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return audit(ctx, r.s.db, now, r.subject, "", "mcp_token.revoked", `{"token":`+jsonString(id)+`}`)
}

// mcpTouchEvery is how often a token's last use is written (a busy session
// does not write on every call).
const mcpTouchEvery = time.Minute

// LookupMCPToken is the unrevoked MCP token tok is, its last use recorded.
// 🔴 A malformed, unknown or revoked token is ErrNotFound alike.
func (s *Store) LookupMCPToken(ctx context.Context, tok string, now time.Time) (MCPToken, error) {
	if !ValidMCPToken(tok) {
		return MCPToken{}, ErrNotFound
	}
	h := Hash(tok)
	var t MCPToken
	var stored string
	var created int64
	var used, revoked sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT id, token_sha256, subject, created_at, last_used_at, revoked_at FROM mcp_tokens WHERE token_sha256 = ?`, h).
		Scan(&t.ID, &stored, &t.Subject, &created, &used, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return MCPToken{}, ErrNotFound
	}
	if err != nil {
		return MCPToken{}, err
	}
	if subtle.ConstantTimeCompare([]byte(stored), []byte(h)) != 1 || revoked.Valid {
		return MCPToken{}, ErrNotFound
	}
	t.CreatedAt, t.LastUsedAt = time.UnixMilli(created).UTC(), fromMS(used)
	if !used.Valid || ms(now)-used.Int64 >= mcpTouchEvery.Milliseconds() {
		if _, err := s.db.ExecContext(ctx, `UPDATE mcp_tokens SET last_used_at = ? WHERE id = ?`, ms(now), t.ID); err != nil {
			return MCPToken{}, err
		}
		u := time.UnixMilli(ms(now)).UTC()
		t.LastUsedAt = &u
	}
	return t, nil
}

// ---- what the MCP tools read beyond the owner app's routes ----

// Person is one person of a meeting as its owner reads it back: the owner,
// or a guest who knocked. CardEmail is the address the guest typed on the
// card, while it is kept for the notes ("" otherwise).
type Person struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Role      string     `json:"role"` // owner, guest
	State     string     `json:"state"`
	Admitted  bool       `json:"admitted"`
	KnockedAt time.Time  `json:"knocked_at"`
	LeftAt    *time.Time `json:"left_at"`
	CardEmail string     `json:"card_email,omitempty"`
}

// The owner's Person: ID and Role "owner" (RecipientOwner), State "joined"
// or "ended".

// People is the owner (first, once the meeting has started: they joined
// it), then every guest who knocked on one of the owner's meetings, in
// knock order.
func (r *OwnerRepo) People(ctx context.Context, meetingID string) ([]Person, error) {
	m, err := r.Meeting(ctx, meetingID)
	if err != nil {
		return nil, err
	}
	out := []Person{}
	if m.StartedAt != nil {
		name, err := r.DisplayName(ctx)
		if err != nil {
			return nil, err
		}
		state := "joined"
		if m.State == "ended" {
			state = "ended"
		}
		out = append(out, Person{ID: RecipientOwner, Name: name, Role: RecipientOwner, State: state, Admitted: true, KnockedAt: *m.StartedAt, LeftAt: m.EndedAt})
	}
	rows, err := r.s.db.QueryContext(ctx, `SELECT p.id, p.display_name, p.kind, p.state, p.admitted_at IS NOT NULL, p.knocked_at, p.left_at, coalesce(p.card_email, '')
		FROM participants p JOIN meetings m ON m.id = p.meeting_id WHERE m.id = ? AND m.subject = ? ORDER BY p.knocked_at, p.id`, meetingID, r.subject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p Person
		var knocked int64
		var left sql.NullInt64
		if err := rows.Scan(&p.ID, &p.Name, &p.Role, &p.State, &p.Admitted, &knocked, &left, &p.CardEmail); err != nil {
			return nil, err
		}
		p.KnockedAt, p.LeftAt = time.UnixMilli(knocked).UTC(), fromMS(left)
		out = append(out, p)
	}
	return out, rows.Err()
}

// ErrMeetingOpen is a write that needs the meeting to have ended.
var ErrMeetingOpen = errors.New("the meeting is still open")

// AddOwnerRun stores action items the owner supplied (their own Claude
// read the transcript: the MCP tool meeting_items_set) as a NEW finished
// post run of one of their ENDED meetings, labelled label (its
// items_model), and makes it current exactly as a post run's finish does
// (FinishRun: with mail on, every recipient not yet sent to is redrafted
// from it, and a recipient already sent to is left alone). 🔴 It is one
// transaction from insert to current: the run is never queued nor running,
// so the post-call worker can never claim it and send it to a model. The
// items were checked by the caller (items.GuardSupplied). A meeting whose
// AI action items were off IS allowed: these are the owner's own notes,
// and nothing goes to an AI service.
func (r *OwnerRepo) AddOwnerRun(ctx context.Context, meetingID, label string, its []NewItem, mail bool, now time.Time) (Run, RedraftResult, error) {
	m, err := r.Meeting(ctx, meetingID)
	if err != nil {
		return Run{}, RedraftResult{}, err
	}
	if m.State != "ended" {
		return Run{}, RedraftResult{}, ErrMeetingOpen
	}
	tx, err := r.s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, RedraftResult{}, err
	}
	defer tx.Rollback()
	if err := r.mayWrite(ctx, tx); err != nil {
		return Run{}, RedraftResult{}, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO ai_runs (meeting_id, kind, items_model, items_prefs, state, next_at, requested_by, created_at)
		SELECT id, 'post', ?, '', 'running', ?, 'owner', ? FROM meetings WHERE id = ? AND subject = ? AND state = 'ended'`,
		label, ms(now), ms(now), meetingID, r.subject)
	if err != nil {
		return Run{}, RedraftResult{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Run{}, RedraftResult{}, ErrNotFound
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Run{}, RedraftResult{}, err
	}
	run := Run{ID: id, MeetingID: meetingID, Subject: r.subject}
	rr, err := finishRunTx(ctx, tx, run, Outcome{Items: its}, mail, now)
	if err != nil {
		return Run{}, RedraftResult{}, err
	}
	out, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runCols+` FROM ai_runs r JOIN meetings m ON m.id = r.meeting_id WHERE r.id = ?`, id))
	if err != nil {
		return Run{}, RedraftResult{}, err
	}
	return out, rr, tx.Commit()
}
