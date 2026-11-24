package store

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"time"
)

// The meeting notes' drafts and their outbox (decisions 1–3, 7). 🔴 Rules
// the schema and these statements hold together:
//
//   - nothing is queued but by Approve, which the owner's Send calls, and
//     only with the owner's address confirmed;
//   - a recipient has at most one draft queued, sending, sent or of unknown
//     outcome per meeting (mail_drafts_one_send), and redraft never touches
//     one of those: a reprocess makes new drafts only for the unsent;
//   - a guest's address is kept only until their notes are sent, or
//     MailKeepDays after the meeting ended.

// Draft states.
const (
	DraftDraft      = "draft"
	DraftRemoved    = "removed"
	DraftSuperseded = "superseded"
	DraftQueued     = "queued"
	DraftSending    = "sending"
	DraftSent       = "sent"
	DraftFailed     = "failed"
	DraftUnknown    = "unknown"
)

// RecipientOwner is the owner's recipient key.
const RecipientOwner = "owner"

// MailKeepDays is how long a guest's card address is kept for the notes
// when they are never sent.
const MailKeepDays = 30

// DraftItem is one action item in a draft.
type DraftItem struct {
	Text     string `json:"text"`
	Assignee string `json:"assignee"`
	Due      string `json:"due"`
}

// Draft is one recipient's notes.
type Draft struct {
	ID         string      `json:"id"`
	MeetingID  string      `json:"-"`
	RunID      int64       `json:"run_id"`
	Recipient  string      `json:"recipient"` // owner, or a participant id
	Name       string      `json:"name"`
	Email      string      `json:"email"` // a guest's; "" for the owner's (and once sent or purged)
	Items      []DraftItem `json:"items"`
	State      string      `json:"state"`
	Attempts   int         `json:"attempts"`
	ApprovedAt *time.Time  `json:"approved_at"`
	SentAt     *time.Time  `json:"sent_at"`
	Error      string      `json:"error"`
	// ItemsChanged says a reprocess (or a revert) made this draft from a
	// run whose items differ from the ones the owner had edited: the edits
	// were not carried over.
	ItemsChanged bool `json:"items_changed"`
}

const draftCols = `d.id, d.meeting_id, d.run_id, d.recipient, d.name, coalesce(d.email, ''), d.items, d.state, d.attempts, d.approved_at, d.sent_at, d.error, d.changed`

func scanDraft(sc interface{ Scan(...any) error }) (Draft, error) {
	var d Draft
	var items string
	var appr, sent sql.NullInt64
	err := sc.Scan(&d.ID, &d.MeetingID, &d.RunID, &d.Recipient, &d.Name, &d.Email, &items, &d.State, &d.Attempts, &appr, &sent, &d.Error, &d.ItemsChanged)
	if errors.Is(err, sql.ErrNoRows) {
		return Draft{}, ErrNotFound
	}
	d.Items = []DraftItem{}
	_ = json.Unmarshal([]byte(items), &d.Items)
	d.ApprovedAt, d.SentAt = fromMS(appr), fromMS(sent)
	return d, err
}

// DraftIDBytes is a draft's id: 128 bits, 22 characters.
const DraftIDBytes = 16

var draftIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)

// ValidDraftID checks a draft id's shape.
func ValidDraftID(s string) bool { return draftIDRE.MatchString(s) }

// RedraftResult says what a redraft did: drafts made, and recipients left
// alone because their notes were already sent (or are on their way).
type RedraftResult struct {
	Made        int `json:"made"`
	AlreadySent int `json:"already_sent"`
}

type recipient struct{ key, name, email string }

// recipients is the owner and every guest who sent the card (with an
// address still kept), for a meeting.
func recipients(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, meetingID string) ([]recipient, error) {
	rows, err := q.QueryContext(ctx, `SELECT 'owner', o.display_name, '' FROM meetings m JOIN owners o ON o.subject = m.subject WHERE m.id = ?
		UNION ALL SELECT p.id, p.display_name, p.card_email FROM participants p WHERE p.meeting_id = ? AND p.kind = 'guest' AND p.card_email IS NOT NULL`, meetingID, meetingID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []recipient
	for rows.Next() {
		var r recipient
		if err := rows.Scan(&r.key, &r.name, &r.email); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func runDraftItems(ctx context.Context, tx *sql.Tx, runID int64) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT text, assignee, due FROM ai_items WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	out := []DraftItem{}
	for rows.Next() {
		var it DraftItem
		if err := rows.Scan(&it.Text, &it.Assignee, &it.Due); err != nil {
			return "", err
		}
		out = append(out, it)
	}
	b, _ := json.Marshal(out)
	return string(b), rows.Err()
}

// redraft makes the run's drafts: for each recipient not yet sent to, its
// unsent drafts are superseded and a new one drawn from the run's items.
// 🔴 A recipient with a draft queued, sending, sent or of unknown outcome
// is left exactly as it is.
func redraft(ctx context.Context, tx *sql.Tx, meetingID string, runID int64, now time.Time) (RedraftResult, error) {
	rs, err := recipients(ctx, tx, meetingID)
	if err != nil {
		return RedraftResult{}, err
	}
	items, err := runDraftItems(ctx, tx, runID)
	if err != nil {
		return RedraftResult{}, err
	}
	var res RedraftResult
	sent := map[string]bool{}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT recipient FROM mail_drafts WHERE meeting_id = ? AND state IN ('queued', 'sending', 'sent', 'unknown')`, meetingID)
	if err != nil {
		return RedraftResult{}, err
	}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return RedraftResult{}, err
		}
		sent[k] = true
	}
	rows.Close()
	res.AlreadySent = len(sent)
	for _, r := range rs {
		if sent[r.key] {
			continue
		}
		state, its, changed, err := carry(ctx, tx, meetingID, r.key, items)
		if err != nil {
			return RedraftResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE mail_drafts SET state = 'superseded', updated_at = ? WHERE meeting_id = ? AND recipient = ? AND state IN ('draft', 'removed', 'failed')`,
			ms(now), meetingID, r.key); err != nil {
			return RedraftResult{}, err
		}
		var email any
		if r.key != RecipientOwner {
			email = r.email
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO mail_drafts (id, meeting_id, run_id, recipient, name, email, items, state, changed, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, random(DraftIDBytes), meetingID, runID, r.key, r.name, email, its, state, changed, ms(now), ms(now)); err != nil {
			return RedraftResult{}, err
		}
		res.Made++
	}
	return res, nil
}

// carry is what a recipient's new draft takes from their newest unsent one
// (read BEFORE it is superseded): 🔴 "Don't send" stays (a guest the owner
// took out is not put back), and the owner's edits stay when they still
// apply. Edits apply item by item to the new run's items with the same text
// as an item the owner edited; when an edited item is gone, or the owner
// added or removed items and the run's items differ, the new run's items
// are taken as they are and changed says so (the archive shows it).
func carry(ctx context.Context, tx *sql.Tx, meetingID, rcpt, runItems string) (state, items string, changed bool, err error) {
	var prevItems, prevState string
	var prevRun int64
	err = tx.QueryRowContext(ctx, `SELECT items, state, run_id FROM mail_drafts WHERE meeting_id = ? AND recipient = ? AND state IN ('draft', 'removed', 'failed')
		ORDER BY rowid DESC LIMIT 1`, meetingID, rcpt).Scan(&prevItems, &prevState, &prevRun)
	if errors.Is(err, sql.ErrNoRows) {
		return DraftDraft, runItems, false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	state = DraftDraft
	if prevState == DraftRemoved {
		state = DraftRemoved
	}
	orig, err := runDraftItems(ctx, tx, prevRun)
	if err != nil {
		return "", "", false, err
	}
	if prevItems == orig {
		return state, runItems, false, nil // no edit to carry
	}
	if runItems == orig {
		return state, prevItems, false, nil // the same items: the edits stand as they are
	}
	var o, p, n []DraftItem
	_ = json.Unmarshal([]byte(orig), &o)
	_ = json.Unmarshal([]byte(prevItems), &p)
	_ = json.Unmarshal([]byte(runItems), &n)
	if len(o) != len(p) {
		return state, runItems, true, nil
	}
	edited := map[string]DraftItem{}
	for i := range o {
		if o[i] != p[i] {
			edited[o[i].Text] = p[i]
		}
	}
	applied := map[string]bool{}
	for i := range n {
		if e, ok := edited[n[i].Text]; ok {
			applied[n[i].Text] = true
			n[i] = e
		}
	}
	for t := range edited {
		if !applied[t] {
			changed = true
		}
	}
	b, _ := json.Marshal(n)
	return state, string(b), changed, nil
}

// DraftFor makes the draft of one guest whose card arrived after the
// meeting's current post run finished (with mail on): nothing when the run
// is not a finished post run, or the guest has one.
func (s *Store) DraftFor(ctx context.Context, meetingID, participantID string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var runID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM ai_runs WHERE meeting_id = ? AND current = 1 AND kind = 'post' AND state = 'done'`, meetingID).Scan(&runID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM mail_drafts WHERE meeting_id = ? AND recipient = ? AND state != 'superseded'`, meetingID, participantID).Scan(&n); err != nil || n > 0 {
		return err
	}
	var name, email string
	err = tx.QueryRowContext(ctx, `SELECT display_name, card_email FROM participants WHERE id = ? AND meeting_id = ? AND card_email IS NOT NULL`, participantID, meetingID).Scan(&name, &email)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	items, err := runDraftItems(ctx, tx, runID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO mail_drafts (id, meeting_id, run_id, recipient, name, email, items, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'draft', ?, ?)`, random(DraftIDBytes), meetingID, runID, participantID, name, email, items, ms(now), ms(now)); err != nil {
		return err
	}
	return tx.Commit()
}

// KeepCardEmail keeps the address a guest typed on the card, for the
// meeting notes.
func (s *Store) KeepCardEmail(ctx context.Context, participantID, email string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE participants SET card_email = ? WHERE id = ? AND kind = 'guest'`, email, participantID)
	return err
}

// Drafts is one of the owner's meetings' drafts that stand: every one but
// the superseded, owner first.
func (r *OwnerRepo) Drafts(ctx context.Context, meetingID string) ([]Draft, error) {
	if _, err := r.Meeting(ctx, meetingID); err != nil {
		return nil, err
	}
	rows, err := r.s.db.QueryContext(ctx, `SELECT `+draftCols+` FROM mail_drafts d JOIN meetings m ON m.id = d.meeting_id
		WHERE d.meeting_id = ? AND m.subject = ? AND d.state != 'superseded' ORDER BY d.recipient = 'owner' DESC, d.rowid`, meetingID, r.subject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Draft{}
	for rows.Next() {
		d, err := scanDraft(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ErrDraftLocked is an edit to a draft that is no longer a draft (queued,
// sending, sent, failed, unknown or superseded).
var ErrDraftLocked = errors.New("the notes were already sent, or are on their way")

// MaxDraftItems bounds a draft's items.
const MaxDraftItems = 100

// ErrNotGuest is an address edit on the owner's own draft (the owner's
// address is the confirmed one).
var ErrNotGuest = errors.New("only a guest's address is edited here")

// EditDraft changes a draft's items, whether it is removed, and (a guest's)
// its address. Only a draft, a removed one or a failed one changes; a failed
// one edited is a draft again, for the owner to Send.
func (r *OwnerRepo) EditDraft(ctx context.Context, meetingID, id string, items *[]DraftItem, removed *bool, email *string, now time.Time) (Draft, error) {
	if !ValidDraftID(id) {
		return Draft{}, ErrNotFound
	}
	if _, err := r.Meeting(ctx, meetingID); err != nil {
		return Draft{}, err
	}
	tx, err := r.s.db.BeginTx(ctx, nil)
	if err != nil {
		return Draft{}, err
	}
	defer tx.Rollback()
	d, err := scanDraft(tx.QueryRowContext(ctx, `SELECT `+draftCols+` FROM mail_drafts d JOIN meetings m ON m.id = d.meeting_id
		WHERE d.id = ? AND d.meeting_id = ? AND m.subject = ?`, id, meetingID, r.subject))
	if err != nil {
		return Draft{}, err
	}
	if d.State != DraftDraft && d.State != DraftRemoved && d.State != DraftFailed {
		return Draft{}, ErrDraftLocked
	}
	if email != nil && d.Recipient == RecipientOwner {
		return Draft{}, ErrNotGuest
	}
	if d.State == DraftFailed {
		if _, err := tx.ExecContext(ctx, `UPDATE mail_drafts SET state = 'draft', attempts = 0, error = '', updated_at = ? WHERE id = ?`, ms(now), id); err != nil {
			return Draft{}, err
		}
	}
	if items != nil {
		b, _ := json.Marshal(*items)
		if _, err := tx.ExecContext(ctx, `UPDATE mail_drafts SET items = ?, changed = 0, updated_at = ? WHERE id = ?`, string(b), ms(now), id); err != nil {
			return Draft{}, err
		}
	}
	if email != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE mail_drafts SET email = ?, updated_at = ? WHERE id = ?`, *email, ms(now), id); err != nil {
			return Draft{}, err
		}
	}
	if removed != nil {
		st := DraftDraft
		if *removed {
			st = DraftRemoved
		}
		if _, err := tx.ExecContext(ctx, `UPDATE mail_drafts SET state = ?, updated_at = ? WHERE id = ?`, st, ms(now), id); err != nil {
			return Draft{}, err
		}
	}
	d, err = scanDraft(tx.QueryRowContext(ctx, `SELECT `+draftCols+` FROM mail_drafts d WHERE d.id = ?`, id))
	if err != nil {
		return Draft{}, err
	}
	return d, tx.Commit()
}

// ErrNoOwnerEmail is a Send before the owner's address is confirmed.
var ErrNoOwnerEmail = errors.New("the owner's address is not confirmed")

// Approve is the owner's Send: every draft (or only ids, when given) of the
// meeting that is a draft is queued, now. 🔴 It refuses unless the owner's
// address is confirmed, and a guest draft whose address was purged is left
// a draft. share, when not nil, is called in the transaction for each
// queued draft (the notes' video link). It answers how many were queued.
func (r *OwnerRepo) Approve(ctx context.Context, meetingID string, ids []string, now time.Time, share func(tx *sql.Tx, d Draft) error) (int, error) {
	if r.session == "" {
		return 0, ErrNoSession
	}
	if _, err := r.Meeting(ctx, meetingID); err != nil {
		return 0, err
	}
	tx, err := r.s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var email sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT email FROM owners WHERE subject = ?`, r.subject).Scan(&email); err != nil {
		return 0, err
	}
	if !email.Valid || email.String == "" {
		return 0, ErrNoOwnerEmail
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+draftCols+` FROM mail_drafts d JOIN meetings m ON m.id = d.meeting_id
		WHERE d.meeting_id = ? AND m.subject = ? AND d.state = 'draft' AND (d.recipient = 'owner' OR d.email IS NOT NULL)`, meetingID, r.subject)
	if err != nil {
		return 0, err
	}
	var ds []Draft
	for rows.Next() {
		d, err := scanDraft(rows)
		if err != nil {
			rows.Close()
			return 0, err
		}
		ds = append(ds, d)
	}
	rows.Close()
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	n := 0
	for _, d := range ds {
		if len(ids) > 0 && !want[d.ID] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE mail_drafts SET state = 'queued', approved_at = ?, next_at = ?, updated_at = ? WHERE id = ? AND state = 'draft'`,
			ms(now), ms(now), ms(now), d.ID); err != nil {
			return 0, err
		}
		if share != nil {
			if err := share(tx, d); err != nil {
				return 0, err
			}
		}
		n++
	}
	if err := audit(ctx, tx, now, r.subject, meetingID, "notes.approved", `{"queued":`+itoa(n)+`}`); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// RetryDraft queues a failed draft (or one of unknown outcome, which the
// owner was told may have arrived) again.
func (r *OwnerRepo) RetryDraft(ctx context.Context, meetingID, id string, now time.Time) error {
	if !ValidDraftID(id) {
		return ErrNotFound
	}
	if _, err := r.Meeting(ctx, meetingID); err != nil {
		return err
	}
	res, err := r.s.db.ExecContext(ctx, `UPDATE mail_drafts SET state = 'queued', attempts = 0, next_at = ?, error = '', updated_at = ?
		WHERE id = ? AND meeting_id = ? AND state IN ('failed', 'unknown') AND meeting_id IN (SELECT id FROM meetings WHERE subject = ?)`,
		ms(now), ms(now), id, meetingID, r.subject)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrDraftLocked
	}
	return nil
}

// OutMail is a queued draft with what its message needs.
type OutMail struct {
	Draft
	Subject    string // the meeting's owner
	OwnerName  string
	OwnerEmail string
	Meeting    Meeting
}

// ClaimMail takes the oldest due queued draft: it is sending from now.
func (s *Store) ClaimMail(ctx context.Context, now time.Time) (OutMail, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return OutMail{}, false, err
	}
	defer tx.Rollback()
	var o OutMail
	var owner sql.NullString
	d, err := scanDraft(tx.QueryRowContext(ctx, `SELECT `+draftCols+` FROM mail_drafts d WHERE d.state = 'queued' AND d.next_at <= ? ORDER BY d.next_at, d.id LIMIT 1`, ms(now)))
	if errors.Is(err, ErrNotFound) {
		return OutMail{}, false, nil
	}
	if err != nil {
		return OutMail{}, false, err
	}
	o.Draft = d
	if err := tx.QueryRowContext(ctx, `SELECT m.subject, o.display_name, o.email FROM meetings m JOIN owners o ON o.subject = m.subject WHERE m.id = ?`, d.MeetingID).
		Scan(&o.Subject, &o.OwnerName, &owner); err != nil {
		return OutMail{}, false, err
	}
	o.OwnerEmail = owner.String
	if o.Meeting, err = scanMeeting(tx.QueryRowContext(ctx, `SELECT `+meetingCols+` FROM meetings WHERE id = ?`, d.MeetingID)); err != nil {
		return OutMail{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mail_drafts SET state = 'sending', attempts = attempts + 1, updated_at = ? WHERE id = ?`, ms(now), d.ID); err != nil {
		return OutMail{}, false, err
	}
	o.Attempts++
	return o, true, tx.Commit()
}

// NextMail is when the next queued draft is due.
func (s *Store) NextMail(ctx context.Context) (time.Time, bool, error) {
	var v sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT min(next_at) FROM mail_drafts WHERE state = 'queued'`).Scan(&v)
	if err != nil || !v.Valid {
		return time.Time{}, false, err
	}
	return time.UnixMilli(v.Int64), true, nil
}

// MailSent records a delivered draft: its address and its guest's card
// address are dropped.
func (s *Store) MailSent(ctx context.Context, id string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var meetingID, rcpt string
	if err := tx.QueryRowContext(ctx, `SELECT meeting_id, recipient FROM mail_drafts WHERE id = ? AND state = 'sending'`, id).Scan(&meetingID, &rcpt); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mail_drafts SET state = 'sent', sent_at = ?, email = NULL, error = '', updated_at = ? WHERE id = ?`, ms(now), ms(now), id); err != nil {
		return err
	}
	if rcpt != RecipientOwner {
		if _, err := tx.ExecContext(ctx, `UPDATE participants SET card_email = NULL WHERE id = ? AND meeting_id = ?`, rcpt, meetingID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE mail_drafts SET email = NULL WHERE meeting_id = ? AND recipient = ?`, meetingID, rcpt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MailRetry puts a sending draft back in the queue for next.
func (s *Store) MailRetry(ctx context.Context, id string, next time.Time, phrase string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE mail_drafts SET state = 'queued', next_at = ?, error = ?, updated_at = ? WHERE id = ? AND state = 'sending'`, ms(next), phrase, ms(now), id)
	return err
}

// MailDone ends a sending draft as failed or unknown.
func (s *Store) MailDone(ctx context.Context, id, state, phrase string, now time.Time) error {
	if state != DraftFailed && state != DraftUnknown {
		return errors.New("store: MailDone takes failed or unknown")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE mail_drafts SET state = ?, error = ?, updated_at = ? WHERE id = ? AND state = 'sending'`, state, phrase, ms(now), id)
	return err
}

// ResetSending makes every draft a stopped process was sending UNKNOWN (it
// may have been delivered): never sent again by itself.
func (s *Store) ResetSending(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE mail_drafts SET state = 'unknown', error = 'the plugin stopped while sending', updated_at = ? WHERE state = 'sending'`, ms(now))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PurgeMail drops every guest address kept for a meeting that ended
// MailKeepDays ago or more. It answers how many.
func (s *Store) PurgeMail(ctx context.Context, now time.Time) (int64, error) {
	before := ms(now.Add(-MailKeepDays * 24 * time.Hour))
	res, err := s.db.ExecContext(ctx, `UPDATE participants SET card_email = NULL WHERE card_email IS NOT NULL
		AND meeting_id IN (SELECT id FROM meetings WHERE state = 'ended' AND ended_at <= ?)`, before)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if _, err := s.db.ExecContext(ctx, `UPDATE mail_drafts SET email = NULL WHERE email IS NOT NULL
		AND meeting_id IN (SELECT id FROM meetings WHERE state = 'ended' AND ended_at <= ?)`, before); err != nil {
		return n, err
	}
	return n, nil
}

// ClearMail drops every kept guest address and every unsent draft's (at
// boot with no mail server configured: nothing will send them).
func (s *Store) ClearMail(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE participants SET card_email = NULL WHERE card_email IS NOT NULL`); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE mail_drafts SET email = NULL WHERE email IS NOT NULL`)
	return err
}

// ---- the owner's address ----

// EmailTokenBytes is a confirmation link's token: 256 bits.
const EmailTokenBytes = 32

// EmailTokenLifetime is how long a confirmation link works.
const EmailTokenLifetime = 24 * time.Hour

var emailTokenRE = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// ValidEmailToken checks a confirmation token's shape.
func ValidEmailToken(s string) bool { return emailTokenRE.MatchString(s) }

// OwnerEmail is the owner's confirmed address, and the one waiting for its
// link ("" for none).
type OwnerEmail struct {
	Confirmed string `json:"confirmed"`
	Pending   string `json:"pending"`
}

// Email is the owner's addresses.
func (r *OwnerRepo) Email(ctx context.Context, now time.Time) (OwnerEmail, error) {
	var c, p sql.NullString
	var exp sql.NullInt64
	err := r.s.db.QueryRowContext(ctx, `SELECT email, email_pending, email_token_expires_at FROM owners WHERE subject = ?`, r.subject).Scan(&c, &p, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return OwnerEmail{}, ErrNotFound
	}
	if err != nil {
		return OwnerEmail{}, err
	}
	out := OwnerEmail{Confirmed: c.String}
	if exp.Valid && exp.Int64 > ms(now) {
		out.Pending = p.String
	}
	return out, nil
}

// SetPendingEmail starts confirming addr: a fresh token (only its hash
// kept), live EmailTokenLifetime. The confirmed address, if any, stays
// until the new one is confirmed.
func (r *OwnerRepo) SetPendingEmail(ctx context.Context, addr string, now time.Time) (string, time.Time, error) {
	if r.session == "" {
		return "", time.Time{}, ErrNoSession
	}
	tok := random(EmailTokenBytes)
	exp := now.Add(EmailTokenLifetime)
	res, err := r.s.db.ExecContext(ctx, `UPDATE owners SET email_pending = ?, email_token_sha256 = ?, email_token_expires_at = ? WHERE subject = ?`,
		addr, Hash(tok), ms(exp), r.subject)
	if err != nil {
		return "", time.Time{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", time.Time{}, ErrNotFound
	}
	if err := audit(ctx, r.s.db, now, r.subject, "", "email.pending", ""); err != nil {
		return "", time.Time{}, err
	}
	return tok, exp, nil
}

// PendingByToken is the address a live confirmation token would confirm.
// 🔴 ErrNotFound alike for a malformed, unknown, used or expired token.
func (s *Store) PendingByToken(ctx context.Context, tok string, now time.Time) (string, error) {
	if !ValidEmailToken(tok) {
		return "", ErrNotFound
	}
	h := Hash(tok)
	var stored, pending string
	var exp int64
	err := s.db.QueryRowContext(ctx, `SELECT email_token_sha256, email_pending, email_token_expires_at FROM owners WHERE email_token_sha256 = ?`, h).Scan(&stored, &pending, &exp)
	if err != nil || subtle.ConstantTimeCompare([]byte(stored), []byte(h)) != 1 || exp <= ms(now) {
		return "", ErrNotFound
	}
	return pending, nil
}

// ConfirmEmail confirms the address a live token names, and spends it.
func (s *Store) ConfirmEmail(ctx context.Context, tok string, now time.Time) error {
	if _, err := s.PendingByToken(ctx, tok, now); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE owners SET email = email_pending, email_pending = NULL, email_token_sha256 = NULL, email_token_expires_at = NULL
		WHERE email_token_sha256 = ? AND email_token_expires_at > ?`, Hash(tok), ms(now))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- the notes' video link ----

// MailShareKey is the HMAC key the notes' video links are derived with,
// under its own label: a draft's link is derived from its id, so a retried
// send carries the same link and nothing stores it.
func (s *Store) MailShareKey() []byte {
	m := hmac.New(sha256.New, s.linkKey)
	m.Write([]byte("muster-meetings mail share v1"))
	return m.Sum(nil)
}

// MailShareToken is draft id's video link token.
func (s *Store) MailShareToken(draftID string) string {
	m := hmac.New(sha256.New, s.MailShareKey())
	m.Write([]byte(draftID))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// AddMailShare stores draft d's link (its hash) when the meeting has a
// finished recording that is not past its purge time; it answers whether
// it did.
func (s *Store) AddMailShare(ctx context.Context, tx *sql.Tx, d Draft, now time.Time) (bool, error) {
	var playable int
	var purge sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT m.purge_recording_at, (SELECT count(*) FROM recordings p WHERE p.meeting_id = m.id AND p.start_ms IS NOT NULL
			AND p.duration_ms > 0 AND p.bytes > 0 AND p.object_key != '') FROM meetings m WHERE m.id = ?`, d.MeetingID).Scan(&purge, &playable)
	if err != nil {
		return false, err
	}
	if playable == 0 || (purge.Valid && purge.Int64 <= ms(now)) {
		return false, nil
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO recording_shares (token_sha256, meeting_id, created_at, kind) VALUES (?, ?, ?, 'mail') ON CONFLICT DO NOTHING`,
		Hash(s.MailShareToken(d.ID)), d.MeetingID, ms(now))
	return err == nil, err
}

// MailShareLive reports draft id's link opens (it exists, is not revoked,
// and its recording is not purged) and until when (nil: no end).
func (s *Store) MailShareLive(ctx context.Context, draftID string, now time.Time) (bool, *time.Time, error) {
	sh, err := s.ShareByToken(ctx, s.MailShareToken(draftID), now)
	if errors.Is(err, ErrNotFound) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	return len(sh.Parts) > 0, sh.ExpiresAt, nil
}

// ---- agent read tokens ----

// AgentTokenPrefix starts every agent read token.
const AgentTokenPrefix = "mmr_"

// AgentTokenLifetime is how long one works.
const AgentTokenLifetime = 7 * 24 * time.Hour

var agentTokenRE = regexp.MustCompile(`^mmr_[A-Za-z0-9_-]{43}$`)

// MintAgentToken makes a read-only token for one of the owner's meetings'
// timeline (only its hash kept).
func (r *OwnerRepo) MintAgentToken(ctx context.Context, meetingID string, now time.Time) (string, time.Time, error) {
	if r.session == "" {
		return "", time.Time{}, ErrNoSession
	}
	if _, err := r.Meeting(ctx, meetingID); err != nil {
		return "", time.Time{}, err
	}
	tok := AgentTokenPrefix + random(32)
	exp := now.Add(AgentTokenLifetime)
	if _, err := r.s.db.ExecContext(ctx, `INSERT INTO agent_tokens (token_sha256, meeting_id, subject, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		Hash(tok), meetingID, r.subject, ms(now), ms(exp)); err != nil {
		return "", time.Time{}, err
	}
	return tok, exp, audit(ctx, r.s.db, now, r.subject, meetingID, "agent_token.minted", "")
}

// AgentToken is the owner and meeting a live agent token reads.
func (s *Store) AgentToken(ctx context.Context, tok string, now time.Time) (subject, meetingID string, err error) {
	if !agentTokenRE.MatchString(tok) {
		return "", "", ErrNotFound
	}
	var stored string
	var exp int64
	err = s.db.QueryRowContext(ctx, `SELECT token_sha256, subject, meeting_id, expires_at FROM agent_tokens WHERE token_sha256 = ?`, Hash(tok)).Scan(&stored, &subject, &meetingID, &exp)
	if err != nil || subtle.ConstantTimeCompare([]byte(stored), []byte(Hash(tok))) != 1 || exp <= ms(now) {
		return "", "", ErrNotFound
	}
	return subject, meetingID, nil
}
