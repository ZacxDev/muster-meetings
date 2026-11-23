package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"regexp"
	"time"
)

// The guest card (muster's docs/design/contacts.md, "The guest card").
const (
	// CardBytes is a card id: 32 random bytes, 43 base64url characters. It is
	// a credential (it spends the card), held in the guest page's memory and
	// stored only as its SHA-256.
	CardBytes = 32
	// CardLifetime is how long an offered card can be spent.
	CardLifetime = 30 * time.Minute
	// OutboxLifetime is how long an undelivered outbox row is kept before it
	// is deleted undelivered (design: "older than 72 hours").
	OutboxLifetime = 72 * time.Hour
)

// The card states.
const (
	CardOffered = "offered"
	CardSent    = "sent"
	CardSkipped = "skipped"
)

var cardRE = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// ValidCard is a card id's shape.
func ValidCard(s string) bool { return cardRE.MatchString(s) }

// CardOffer is a card just minted for a guest: the id their page holds, and
// what it shows.
type CardOffer struct {
	ID        string // the secret; stored only as its hash
	TypedName string // what they typed at the knock: prefills the card
	OwnerName string // the meeting owner's display name ("" when muster gave none)
}

// cardEligible is THE card predicate, in SQL over participants p (design:
// "When the card shows", conditions 2 and 3; condition 1, the contacts URL
// and the owner's accepting answer, is the server's, and 4 has no source
// before receipts and invites): a GUEST (never the owner), who was
// ADMITTED at some point, whose meeting ended for them by the meeting's end
// or their own Leave (a removed or denied guest gets none: their state is
// neither), and who has never been offered one (once per guest per
// meeting).
const cardEligible = `p.kind = 'guest' AND p.admitted_at IS NOT NULL AND p.state IN ('ended', 'left') AND p.card_state IS NULL`

// OfferCard mints the card for participant pid when cardEligible holds, in
// one statement, so a card is minted at most once whatever races it. ok is
// false (and nothing changes) when it does not hold.
func (s *Store) OfferCard(ctx context.Context, pid string, now time.Time) (CardOffer, bool, error) {
	id := random(CardBytes)
	var off CardOffer
	err := s.db.QueryRowContext(ctx, `UPDATE participants AS p SET card_sha256 = ?, card_expires_at = ?, card_state = 'offered'
		WHERE p.id = ? AND `+cardEligible+`
		RETURNING display_name, (SELECT o.display_name FROM meetings m JOIN owners o ON o.subject = m.subject WHERE m.id = meeting_id)`,
		Hash(id), ms(now.Add(CardLifetime)), pid).Scan(&off.TypedName, &off.OwnerName)
	if errors.Is(err, sql.ErrNoRows) {
		return CardOffer{}, false, nil
	}
	if err != nil {
		return CardOffer{}, false, err
	}
	off.ID = id
	return off, true, nil
}

// Card is an offered, unexpired card as the card routes see it: who it is
// for, and the meeting it was for.
type Card struct {
	// CardID is the card's PUBLIC id: its stored SHA-256, base64url (43
	// characters, contactsapi.ValidCardID). It names the card to contacts
	// (and keys the submission, card:<CardID>) without the secret the
	// guest's page holds ever leaving meetings.
	CardID        string
	ParticipantID string
	Identity      string
	TypedName     string
	AdmittedAt    *time.Time
	LeftAt        *time.Time
	MeetingID     string
	Title         string
	CreatedAt     time.Time
	StartedAt     *time.Time
	EndedAt       *time.Time
	Subject       string // the meeting's owner
	OwnerName     string
}

// cardCols reads a Card from participants p, meetings m and owners o.
const cardCols = `p.card_sha256, p.id, p.identity, p.display_name, p.admitted_at, p.left_at, m.id, m.title, m.created_at, m.started_at, m.ended_at, m.subject, o.display_name`

func scanCard(sc interface{ Scan(...any) error }) (Card, error) {
	var c Card
	var admitted, left, started, ended sql.NullInt64
	var created int64
	var sum string
	if err := sc.Scan(&sum, &c.ParticipantID, &c.Identity, &c.TypedName, &admitted, &left, &c.MeetingID, &c.Title, &created, &started, &ended, &c.Subject, &c.OwnerName); err != nil {
		return Card{}, err
	}
	c.AdmittedAt, c.LeftAt, c.StartedAt, c.EndedAt = fromMS(admitted), fromMS(left), fromMS(started), fromMS(ended)
	c.CreatedAt = time.UnixMilli(created).UTC()
	raw, err := hex.DecodeString(sum)
	if err != nil || len(raw) != 32 {
		return Card{}, errors.New("store: a card's hash is not a SHA-256")
	}
	c.CardID = base64.RawURLEncoding.EncodeToString(raw)
	return c, nil
}

// liveCard is "offered and unexpired", for a card looked up by its hash.
const liveCard = `p.card_sha256 = ? AND p.card_state = 'offered' AND p.card_expires_at > ?`

// CardByID is the offered, unexpired card id names. A malformed, unknown,
// spent or expired id is ErrNotFound: one answer for all of them.
func (s *Store) CardByID(ctx context.Context, id string, now time.Time) (Card, error) {
	if !ValidCard(id) {
		return Card{}, ErrNotFound
	}
	c, err := scanCard(s.db.QueryRowContext(ctx, `SELECT `+cardCols+` FROM participants p JOIN meetings m ON m.id = p.meeting_id JOIN owners o ON o.subject = m.subject
		WHERE `+liveCard, Hash(id), ms(now)))
	if errors.Is(err, sql.ErrNoRows) {
		return Card{}, ErrNotFound
	}
	return c, err
}

// OutboxItem is a row to enqueue: built from the card, in SpendCard's
// transaction.
type OutboxItem struct {
	Key  string
	Kind string // "submission"
	Path string
	Body []byte
}

// SpendCard spends the offered, unexpired card id names, in one
// transaction: its state becomes state (CardSent or CardSkipped), and for a
// send, the outbox row build makes from the card is written in the same
// transaction (design: "in the same transaction that spends the card"). A
// card that is not live is ErrNotFound and nothing is written. 🔴 A skip
// enqueues nothing: build is not called.
func (s *Store) SpendCard(ctx context.Context, id, state string, now time.Time, build func(Card) (OutboxItem, error)) (Card, error) {
	if !ValidCard(id) || (state != CardSent && state != CardSkipped) {
		return Card{}, ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Card{}, err
	}
	defer tx.Rollback()
	c, err := scanCard(tx.QueryRowContext(ctx, `SELECT `+cardCols+` FROM participants p JOIN meetings m ON m.id = p.meeting_id JOIN owners o ON o.subject = m.subject
		WHERE `+liveCard, Hash(id), ms(now)))
	if errors.Is(err, sql.ErrNoRows) {
		return Card{}, ErrNotFound
	}
	if err != nil {
		return Card{}, err
	}
	r, err := tx.ExecContext(ctx, `UPDATE participants SET card_state = ? WHERE id = ? AND card_state = 'offered'`, state, c.ParticipantID)
	if err != nil {
		return Card{}, err
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return Card{}, ErrNotFound
	}
	if state == CardSent {
		it, err := build(c)
		if err != nil {
			return Card{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO contacts_outbox (key, kind, path, body, created_at, next_at) VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (key) DO NOTHING`, it.Key, it.Kind, it.Path, string(it.Body), ms(now), ms(now)); err != nil {
			return Card{}, err
		}
	}
	// The audit row names the participant (a random id) and the outcome,
	// never what was typed.
	if err := audit(ctx, tx, now, c.Subject, c.MeetingID, "card."+state, c.ParticipantID); err != nil {
		return Card{}, err
	}
	return c, tx.Commit()
}

// OutboxRow is one undelivered row.
type OutboxRow struct {
	ID        int64
	Key       string
	Kind      string
	Path      string
	Body      []byte
	CreatedAt time.Time
	Attempts  int
}

// DueOutbox is up to limit rows due at now, oldest first (by insertion).
func (s *Store) DueOutbox(ctx context.Context, now time.Time, limit int) ([]OutboxRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, key, kind, path, body, created_at, attempts FROM contacts_outbox WHERE next_at <= ? ORDER BY id LIMIT ?`, ms(now), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxRow
	for rows.Next() {
		var r OutboxRow
		var body string
		var created int64
		if err := rows.Scan(&r.ID, &r.Key, &r.Kind, &r.Path, &body, &created, &r.Attempts); err != nil {
			return nil, err
		}
		r.Body, r.CreatedAt = []byte(body), time.UnixMilli(created).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// NextOutbox is when the next row falls due; ok false with none waiting.
func (s *Store) NextOutbox(ctx context.Context) (time.Time, bool, error) {
	var v sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT min(next_at) FROM contacts_outbox`).Scan(&v); err != nil {
		return time.Time{}, false, err
	}
	if !v.Valid {
		return time.Time{}, false, nil
	}
	return time.UnixMilli(v.Int64).UTC(), true, nil
}

// DeleteOutbox deletes one row (delivered, or refused for good).
func (s *Store) DeleteOutbox(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM contacts_outbox WHERE id = ?`, id)
	return err
}

// RetryOutbox counts a failed attempt on a row and sets when it is next due.
func (s *Store) RetryOutbox(ctx context.Context, id int64, next time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE contacts_outbox SET attempts = attempts + 1, next_at = ? WHERE id = ?`, ms(next), id)
	return err
}

// DropExpiredOutbox deletes every row created OutboxLifetime or more before
// now, undelivered, and returns how many.
func (s *Store) DropExpiredOutbox(ctx context.Context, now time.Time) (int64, error) {
	r, err := s.db.ExecContext(ctx, `DELETE FROM contacts_outbox WHERE created_at <= ?`, ms(now.Add(-OutboxLifetime)))
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// ClearOutbox deletes every row: a boot with no contacts URL (design: "the
// URL was removed; their PII has nowhere to go").
func (s *Store) ClearOutbox(ctx context.Context) (int64, error) {
	r, err := s.db.ExecContext(ctx, `DELETE FROM contacts_outbox`)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// CountOutbox is how many rows wait.
func (s *Store) CountOutbox(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM contacts_outbox`).Scan(&n)
	return n, err
}

// EnqueueOutbox writes a row outside a card's transaction (the worker's
// tests; phase 1c's attendance rows). A key already waiting is kept as it is.
func (s *Store) EnqueueOutbox(ctx context.Context, it OutboxItem, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO contacts_outbox (key, kind, path, body, created_at, next_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (key) DO NOTHING`, it.Key, it.Kind, it.Path, string(it.Body), ms(now), ms(now))
	return err
}
