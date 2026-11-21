package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// The lobby's numbers (design: "Limits").
const (
	// MaxWaiting is the most knocks waiting on one meeting at once; the next
	// is ErrLobbyFull (429).
	MaxWaiting = 20
	// MaxAdmittedGuests is the most guests admitted at once: 8 participants,
	// owner included (decision 13).
	MaxAdmittedGuests = 7
	// TicketBytes is a knock's ticket: 128 bits, 22 base64url characters.
	TicketBytes = 16
	// ParticipantIDBytes is a lobby row's id (in owner URLs; not a credential).
	ParticipantIDBytes = 12
)

// OwnerIdentity is the owner's LiveKit identity in their meeting's room. A
// meeting has one owner, so one identity: a second device of the owner
// joining takes the place of the first, which is LiveKit's duplicate-identity
// rule. It names no subject, so no guest ever learns the owner's subject.
const OwnerIdentity = "owner"

// The participant states (design: "participants … state").
const (
	StWaiting  = "waiting"
	StAdmitted = "admitted"
	StDenied   = "denied"
	StRemoved  = "removed"
	StLeft     = "left"
	StEnded    = "ended"
)

// Event is something that moves a participant between states.
type Event string

const (
	EvAdmit  Event = "admit"
	EvDeny   Event = "deny"
	EvRemove Event = "remove"
	EvLeave  Event = "leave"
	EvEnd    Event = "end"
)

// transitions is the lobby's state machine, the one place it is written
// (design: "waiting → admitted / denied / ended; admitted → removed / left /
// ended; nothing leaves ended"). Every state change in this package is an
// UPDATE guarded by the from-states this table names for its event, so a
// change the table does not allow changes no row.
var transitions = map[Event]struct {
	from []string
	to   string
}{
	EvAdmit:  {[]string{StWaiting}, StAdmitted},
	EvDeny:   {[]string{StWaiting}, StDenied},
	EvRemove: {[]string{StAdmitted}, StRemoved},
	EvLeave:  {[]string{StAdmitted}, StLeft},
	EvEnd:    {[]string{StWaiting, StAdmitted}, StEnded},
}

// Transition is the state event moves from to, and whether it may.
func Transition(from string, ev Event) (string, bool) {
	t, ok := transitions[ev]
	if !ok {
		return "", false
	}
	for _, f := range t.from {
		if f == from {
			return t.to, true
		}
	}
	return "", false
}

// fromList is the SQL list of an event's from-states, for an IN (…) guard.
func fromList(ev Event) string {
	s := ""
	for i, f := range transitions[ev].from {
		if i > 0 {
			s += ", "
		}
		s += "'" + f + "'"
	}
	return s
}

// Errors the lobby answers.
var (
	// ErrGone is a meeting that has ended, or is LinkLifetime old: its link
	// answers 410.
	ErrGone = errors.New("the meeting has ended")
	// ErrLobbyFull is a knock past MaxWaiting.
	ErrLobbyFull = errors.New("too many people are waiting to join")
	// ErrCallFull is an admit past MaxAdmittedGuests.
	ErrCallFull = errors.New("the call is full")
	// ErrState is a lobby change the participant's state does not allow
	// (admitting someone already denied, say).
	ErrState = errors.New("that person is not in a state for this")
	// ErrSwitchLocked is turning Record or AI on after a guest has knocked.
	ErrSwitchLocked = errors.New("a guest has already knocked, so this can only stay off")
)

// Notice is the state of a meeting's two switches: what the consent notice
// says (design: "Consent").
type Notice struct {
	Record bool `json:"record"`
	AI     bool `json:"ai"`
}

// NoticeMismatch is a knock whose notice is not the meeting's current one:
// the guest read a notice that is no longer true (design: 409).
type NoticeMismatch struct{ Current Notice }

func (e *NoticeMismatch) Error() string {
	return "the notice the guest saw is not the meeting's current one"
}

// live is the SQL for "the meeting is open and its link has not reached
// LinkLifetime", for a meetings row aliased m, with now and the cutoff
// (now - LinkLifetime) bound in that order.
const live = `m.state = 'open' AND m.created_at > ?`

func cutoff(now time.Time) int64 { return ms(now.Add(-LinkLifetime)) }

// ---- the guest side: everything keyed on the link code ----

// GuestMeeting is what a guest link shows: whose meeting, and the notice.
type GuestMeeting struct {
	ID        string
	Room      string
	OwnerName string
	Notice    Notice
}

// MeetingByCode is the meeting a guest link names. An unknown code is
// ErrNotFound; one whose meeting has ended or is LinkLifetime old is ErrGone.
func (s *Store) MeetingByCode(ctx context.Context, code string, now time.Time) (GuestMeeting, error) {
	if !ValidJoinCode(code) {
		return GuestMeeting{}, ErrNotFound
	}
	var g GuestMeeting
	var state string
	var created int64
	var rec, ai int
	err := s.db.QueryRowContext(ctx, `SELECT m.id, m.room, m.state, m.created_at, m.record, m.ai, o.display_name
		FROM meetings m JOIN owners o ON o.subject = m.subject WHERE m.join_code_sha256 = ?`, Hash(code)).
		Scan(&g.ID, &g.Room, &state, &created, &rec, &ai, &g.OwnerName)
	if errors.Is(err, sql.ErrNoRows) {
		return GuestMeeting{}, ErrNotFound
	}
	if err != nil {
		return GuestMeeting{}, err
	}
	if state != "open" || created <= cutoff(now) {
		return GuestMeeting{}, ErrGone
	}
	g.Notice = Notice{Record: rec == 1, AI: ai == 1}
	return g, nil
}

// Knocked is a knock just stored.
type Knocked struct {
	ParticipantID string
	Ticket        string // the guest's; stored only as its hash
	MeetingID     string
	Waiting       int // how many wait now, this knock included
}

// Knock stores a guest's knock on the meeting code names, in one
// transaction: the meeting must be live (else ErrGone, or ErrNotFound for an
// unknown code); the notice the guest was shown must be the current one (else
// *NoticeMismatch with it); fewer than MaxWaiting may be waiting (else
// ErrLobbyFull). name must already be clean (lobby.CleanName).
func (s *Store) Knock(ctx context.Context, code, name string, shown Notice, now time.Time) (Knocked, error) {
	if !ValidJoinCode(code) {
		return Knocked{}, ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Knocked{}, err
	}
	defer tx.Rollback()
	var id, subject, state string
	var created int64
	var rec, ai int
	err = tx.QueryRowContext(ctx, `SELECT id, subject, state, created_at, record, ai FROM meetings WHERE join_code_sha256 = ?`, Hash(code)).
		Scan(&id, &subject, &state, &created, &rec, &ai)
	if errors.Is(err, sql.ErrNoRows) {
		return Knocked{}, ErrNotFound
	}
	if err != nil {
		return Knocked{}, err
	}
	if state != "open" || created <= cutoff(now) {
		return Knocked{}, ErrGone
	}
	if cur := (Notice{Record: rec == 1, AI: ai == 1}); shown != cur {
		return Knocked{}, &NoticeMismatch{Current: cur}
	}
	var waiting int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM participants WHERE meeting_id = ? AND state = 'waiting'`, id).Scan(&waiting); err != nil {
		return Knocked{}, err
	}
	if waiting >= MaxWaiting {
		return Knocked{}, ErrLobbyFull
	}
	k := Knocked{ParticipantID: random(ParticipantIDBytes), Ticket: random(TicketBytes), MeetingID: id, Waiting: waiting + 1}
	if _, err := tx.ExecContext(ctx, `INSERT INTO participants (id, meeting_id, kind, display_name, identity, ticket_sha256, state, knocked_at)
		VALUES (?, ?, 'guest', ?, ?, ?, 'waiting', ?)`,
		k.ParticipantID, id, name, "guest-"+random(TicketBytes), Hash(k.Ticket), ms(now)); err != nil {
		return Knocked{}, err
	}
	if err := audit(ctx, tx, now, subject, id, "participant.knocked", k.ParticipantID); err != nil {
		return Knocked{}, err
	}
	return k, tx.Commit()
}

// Guest is a ticket holder as the guest routes see them.
type Guest struct {
	ParticipantID string
	MeetingID     string
	Subject       string // the meeting's owner
	Room          string
	Identity      string
	Name          string
	State         string // the participant's state
	MeetingLive   bool   // open and under LinkLifetime
	Notice        Notice // the meeting's CURRENT switches
}

// GuestByTicket is the participant ticket names on the meeting code names.
// 🔴 Both must match one row: a ticket from another meeting, or a code with
// another meeting's ticket, is ErrNotFound, so a ticket opens only its own
// meeting's streams. An unknown code is ErrNotFound too.
func (s *Store) GuestByTicket(ctx context.Context, code, ticket string, now time.Time) (Guest, error) {
	if !ValidJoinCode(code) || !ValidTicket(ticket) {
		return Guest{}, ErrNotFound
	}
	return s.guest(ctx, `m.join_code_sha256 = ? AND p.ticket_sha256 = ?`, now, Hash(code), Hash(ticket))
}

// GuestByParticipant re-reads a guest GuestByTicket already found, by its
// row id: what a guest's stream does on every change.
func (s *Store) GuestByParticipant(ctx context.Context, participantID string, now time.Time) (Guest, error) {
	return s.guest(ctx, `p.id = ? AND p.kind = 'guest'`, now, participantID)
}

func (s *Store) guest(ctx context.Context, where string, now time.Time, args ...any) (Guest, error) {
	var g Guest
	var mstate string
	var created int64
	var rec, ai int
	err := s.db.QueryRowContext(ctx, `SELECT p.id, m.id, m.subject, m.room, p.identity, p.display_name, p.state, m.state, m.created_at, m.record, m.ai
		FROM participants p JOIN meetings m ON m.id = p.meeting_id WHERE `+where, args...).
		Scan(&g.ParticipantID, &g.MeetingID, &g.Subject, &g.Room, &g.Identity, &g.Name, &g.State, &mstate, &created, &rec, &ai)
	if errors.Is(err, sql.ErrNoRows) {
		return Guest{}, ErrNotFound
	}
	if err != nil {
		return Guest{}, err
	}
	g.MeetingLive = mstate == "open" && created > cutoff(now)
	g.Notice = Notice{Record: rec == 1, AI: ai == 1}
	return g, nil
}

// ---- the machine side: LiveKit's webhook and the reconcile loop ----

// MayBeInRoom is THE admission predicate (design: "any identity that is not
// currently admitted in its own state … is removed"): identity may be
// connected to room only while room's meeting is live and identity is its
// owner's, or a guest whose row is admitted. Revoked, removed, denied, left,
// waiting, unknown, and anyone in an ended meeting's room may not. The guest
// token mint, the participant_joined check and the reconcile loop all ask
// this one function.
func (s *Store) MayBeInRoom(ctx context.Context, room, identity string, now time.Time) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM meetings m WHERE m.room = ? AND `+live+` AND (
			? = '`+OwnerIdentity+`'
			OR EXISTS (SELECT 1 FROM participants p WHERE p.meeting_id = m.id AND p.identity = ? AND p.kind = 'guest' AND p.state = 'admitted'))`,
		room, cutoff(now), identity, identity).Scan(&n)
	return n > 0, err
}

// LiveRooms is the room of every live meeting: what the reconcile loop walks.
func (s *Store) LiveRooms(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.room FROM meetings m WHERE `+live+` ORDER BY m.room`, cutoff(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RoomLive says room belongs to a live meeting. A room no meeting has (not
// ours) is false.
func (s *Store) RoomLive(ctx context.Context, room string, now time.Time) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM meetings m WHERE m.room = ? AND `+live, room, cutoff(now)).Scan(&n)
	return n > 0, err
}

// MeetingOfRoom is the id and owner of the meeting whose room this is, live
// or not; ErrNotFound when no meeting has it.
func (s *Store) MeetingOfRoom(ctx context.Context, room string) (id, subject string, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT id, subject FROM meetings WHERE room = ?`, room).Scan(&id, &subject)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	return id, subject, err
}

// LobbyCount is a meeting's owner and how many guests wait on it (lobby's
// knock notifier). A meeting that does not exist is ("", 0, nil).
func (s *Store) LobbyCount(ctx context.Context, meetingID string) (string, int, error) {
	var subject string
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT m.subject, (SELECT count(*) FROM participants p WHERE p.meeting_id = m.id AND p.state = 'waiting')
		FROM meetings m WHERE m.id = ?`, meetingID).Scan(&subject, &n)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, nil
	}
	return subject, n, err
}

// MarkLeft moves an admitted guest who left room to left: their ticket and
// identity are spent, and they knock again to come back.
func (s *Store) MarkLeft(ctx context.Context, room, identity string, now time.Time) (meetingID string, changed bool, err error) {
	err = s.db.QueryRowContext(ctx, `UPDATE participants SET state = 'left', left_at = ?
		WHERE identity = ? AND state IN (`+fromList(EvLeave)+`) AND meeting_id = (SELECT id FROM meetings WHERE room = ?)
		RETURNING meeting_id`, ms(now), identity, room).Scan(&meetingID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return meetingID, err == nil, err
}

// EndRoom ends the meeting whose room this is (LiveKit's room_finished: the
// room sat empty for its timeout, or was deleted). It returns the meeting id
// and whether it was open.
func (s *Store) EndRoom(ctx context.Context, room, why string, now time.Time) (string, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	var id, subject string
	err = tx.QueryRowContext(ctx, `UPDATE meetings SET state = 'ended', ended_at = ? WHERE room = ? AND state = 'open' RETURNING id, subject`,
		ms(now), room).Scan(&id, &subject)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if err := endParticipants(ctx, tx, now); err != nil {
		return "", false, err
	}
	if err := audit(ctx, tx, now, subject, id, "meeting.ended", why); err != nil {
		return "", false, err
	}
	return id, true, tx.Commit()
}

// endParticipants moves everyone still waiting or admitted in an ended
// meeting to ended, inside the transaction that ended it.
func endParticipants(ctx context.Context, tx *sql.Tx, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE participants SET state = 'ended', left_at = coalesce(left_at, ?)
		WHERE state IN (`+fromList(EvEnd)+`) AND meeting_id IN (SELECT id FROM meetings WHERE state = 'ended')`, ms(now))
	return err
}

// ---- the owner side: through OwnerRepo, so always the owner's own meeting ----

// Participant is a lobby row as the owner sees it.
type Participant struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"` // what the guest typed: render as text only
	State    string     `json:"state"`
	Knocked  time.Time  `json:"knocked_at"`
	Admitted *time.Time `json:"admitted_at"`
}

// Lobby is the meeting's guests who are waiting or admitted, in knock order.
func (r *OwnerRepo) Lobby(ctx context.Context, meetingID string) ([]Participant, error) {
	if _, err := r.Meeting(ctx, meetingID); err != nil {
		return nil, err
	}
	rows, err := r.s.db.QueryContext(ctx, `SELECT p.id, p.display_name, p.state, p.knocked_at, p.admitted_at
		FROM participants p JOIN meetings m ON m.id = p.meeting_id
		WHERE m.id = ? AND m.subject = ? AND p.kind = 'guest' AND p.state IN ('waiting', 'admitted')
		ORDER BY p.knocked_at, p.id`, meetingID, r.subject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Participant{}
	for rows.Next() {
		var p Participant
		var knocked int64
		var admitted sql.NullInt64
		if err := rows.Scan(&p.ID, &p.Name, &p.State, &knocked, &admitted); err != nil {
			return nil, err
		}
		p.Knocked = time.UnixMilli(knocked).UTC()
		p.Admitted = fromMS(admitted)
		out = append(out, p)
	}
	return out, rows.Err()
}

// Waiting is how many guests wait on the meeting. Zero for a meeting that is
// not the owner's.
func (r *OwnerRepo) Waiting(ctx context.Context, meetingID string) (int, error) {
	var n int
	err := r.s.db.QueryRowContext(ctx, `SELECT count(*) FROM participants p JOIN meetings m ON m.id = p.meeting_id
		WHERE m.id = ? AND m.subject = ? AND p.state = 'waiting'`, meetingID, r.subject).Scan(&n)
	return n, err
}

// Knocked says a guest has knocked on the owner's meeting, whatever became of
// them: the switches can then only stay off (SetSwitch).
func (r *OwnerRepo) Knocked(ctx context.Context, meetingID string) (bool, error) {
	if _, err := r.Meeting(ctx, meetingID); err != nil {
		return false, err
	}
	var n int
	err := r.s.db.QueryRowContext(ctx, knockedSQL, meetingID).Scan(&n)
	return n > 0, err
}

// knockedSQL counts the guests who ever knocked on a meeting: the one test of
// "a guest has knocked" that both Knocked and SetSwitch run.
const knockedSQL = `SELECT count(*) FROM participants WHERE meeting_id = ? AND kind = 'guest'`

// LiveMeeting is the owner's meeting while it is live: ErrNotFound for none
// (or another person's), ErrGone once ended or LinkLifetime old.
func (r *OwnerRepo) LiveMeeting(ctx context.Context, id string, now time.Time) (Meeting, string, error) {
	m, err := r.Meeting(ctx, id)
	if err != nil {
		return Meeting{}, "", err
	}
	if m.State != "open" || !now.Before(m.LinkExpiresAt) {
		return Meeting{}, "", ErrGone
	}
	var room string
	if err := r.s.db.QueryRowContext(ctx, `SELECT room FROM meetings WHERE id = ? AND subject = ?`, id, r.subject).Scan(&room); err != nil {
		return Meeting{}, "", err
	}
	return m, room, nil
}

// MarkStarted records when the owner first joined the call.
func (r *OwnerRepo) MarkStarted(ctx context.Context, id string, now time.Time) error {
	_, err := r.s.db.ExecContext(ctx, `UPDATE meetings SET started_at = ? WHERE id = ? AND subject = ? AND started_at IS NULL`, ms(now), id, r.subject)
	return err
}

// Moved is a lobby change the owner made.
type Moved struct {
	Room     string
	Identity string
}

// Move applies ev to one guest of the owner's live meeting: admit, deny or
// remove. The participant must be in this meeting of this owner, and in a
// from-state of ev (else ErrState). Admitting past MaxAdmittedGuests is
// ErrCallFull. It returns the room and the guest's identity.
func (r *OwnerRepo) Move(ctx context.Context, meetingID, pid string, ev Event, now time.Time) (Moved, error) {
	t, ok := transitions[ev]
	if !ok || ev == EvEnd || ev == EvLeave {
		return Moved{}, fmt.Errorf("store: %q is not an owner's lobby change", ev)
	}
	tx, err := r.s.db.BeginTx(ctx, nil)
	if err != nil {
		return Moved{}, err
	}
	defer tx.Rollback()
	var room string
	var created int64
	var state string
	err = tx.QueryRowContext(ctx, `SELECT room, state, created_at FROM meetings WHERE id = ? AND subject = ?`, meetingID, r.subject).Scan(&room, &state, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Moved{}, ErrNotFound
	}
	if err != nil {
		return Moved{}, err
	}
	if state != "open" || created <= cutoff(now) {
		return Moved{}, ErrGone
	}
	if ev == EvAdmit {
		var admitted int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM participants WHERE meeting_id = ? AND state = 'admitted'`, meetingID).Scan(&admitted); err != nil {
			return Moved{}, err
		}
		if admitted >= MaxAdmittedGuests {
			return Moved{}, ErrCallFull
		}
	}
	set := `state = ?`
	args := []any{t.to}
	switch ev {
	case EvAdmit:
		set += `, admitted_at = ?`
		args = append(args, ms(now))
	case EvRemove:
		set += `, left_at = ?`
		args = append(args, ms(now))
	}
	args = append(args, pid, meetingID)
	var identity string
	err = tx.QueryRowContext(ctx, `UPDATE participants SET `+set+`
		WHERE id = ? AND meeting_id = ? AND kind = 'guest' AND state IN (`+fromList(ev)+`) RETURNING identity`, args...).Scan(&identity)
	if errors.Is(err, sql.ErrNoRows) {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM participants WHERE id = ? AND meeting_id = ?`, pid, meetingID).Scan(&n); err != nil {
			return Moved{}, err
		}
		if n == 0 {
			return Moved{}, ErrNotFound
		}
		return Moved{}, ErrState
	}
	if err != nil {
		return Moved{}, err
	}
	if err := audit(ctx, tx, now, r.subject, meetingID, "participant."+string(t.to), pid); err != nil {
		return Moved{}, err
	}
	return Moved{Room: room, Identity: identity}, tx.Commit()
}

// End ends the owner's live meeting: the meeting is ended, everyone waiting
// or admitted is ended, so the link answers 410 and no identity may be in
// the room. It returns the room, for the caller to delete. A meeting already
// ended is ErrGone.
func (r *OwnerRepo) End(ctx context.Context, id string, now time.Time) (string, error) {
	tx, err := r.s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var room string
	err = tx.QueryRowContext(ctx, `UPDATE meetings SET state = 'ended', ended_at = ? WHERE id = ? AND subject = ? AND state = 'open' RETURNING room`,
		ms(now), id, r.subject).Scan(&room)
	if errors.Is(err, sql.ErrNoRows) {
		// Not open: ended already, or not this owner's. Asked on the
		// transaction's own connection (the store has one).
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM meetings WHERE id = ? AND subject = ?`, id, r.subject).Scan(&n); err != nil {
			return "", err
		}
		if n == 0 {
			return "", ErrNotFound
		}
		return "", ErrGone
	}
	if err != nil {
		return "", err
	}
	if err := endParticipants(ctx, tx, now); err != nil {
		return "", err
	}
	if err := audit(ctx, tx, now, r.subject, id, "meeting.ended", "End meeting"); err != nil {
		return "", err
	}
	return room, tx.Commit()
}

// Switch names one of a meeting's two switches.
type Switch string

const (
	SwitchRecord Switch = "record"
	SwitchAI     Switch = "ai"
)

// SetSwitch turns Record or AI on or off on the owner's live meeting.
// 🔴 Off is always allowed. On is refused with ErrSwitchLocked once any guest
// has knocked on the meeting, whatever became of them (waiting, admitted,
// denied, removed, left), so nobody is recorded or sent to the AI service who
// knocked while it was off (design: "Consent").
func (r *OwnerRepo) SetSwitch(ctx context.Context, id string, which Switch, on bool, now time.Time) (Notice, error) {
	col := ""
	switch which {
	case SwitchRecord:
		col = "record"
	case SwitchAI:
		col = "ai"
	default:
		return Notice{}, fmt.Errorf("store: no switch %q", which)
	}
	tx, err := r.s.db.BeginTx(ctx, nil)
	if err != nil {
		return Notice{}, err
	}
	defer tx.Rollback()
	var state string
	var created int64
	err = tx.QueryRowContext(ctx, `SELECT state, created_at FROM meetings WHERE id = ? AND subject = ?`, id, r.subject).Scan(&state, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Notice{}, ErrNotFound
	}
	if err != nil {
		return Notice{}, err
	}
	if state != "open" || created <= cutoff(now) {
		return Notice{}, ErrGone
	}
	if on {
		var knocked int
		if err := tx.QueryRowContext(ctx, knockedSQL, id).Scan(&knocked); err != nil {
			return Notice{}, err
		}
		if knocked > 0 {
			return Notice{}, ErrSwitchLocked
		}
	}
	v := 0
	if on {
		v = 1
	}
	if _, err := tx.ExecContext(ctx, `UPDATE meetings SET `+col+` = ? WHERE id = ? AND subject = ?`, v, id, r.subject); err != nil {
		return Notice{}, err
	}
	var rec, ai int
	if err := tx.QueryRowContext(ctx, `SELECT record, ai FROM meetings WHERE id = ? AND subject = ?`, id, r.subject).Scan(&rec, &ai); err != nil {
		return Notice{}, err
	}
	if err := audit(ctx, tx, now, r.subject, id, "meeting.switch", fmt.Sprintf("%s=%v", col, on)); err != nil {
		return Notice{}, err
	}
	return Notice{Record: rec == 1, AI: ai == 1}, tx.Commit()
}

// NameInRoom is the name identity is shown by in room's meeting: the owner's
// display name ("" when muster gave none), or what a guest typed, with guest
// true. ErrNotFound when the identity is neither.
func (s *Store) NameInRoom(ctx context.Context, room, identity string) (name string, guest bool, err error) {
	if identity == OwnerIdentity {
		err = s.db.QueryRowContext(ctx, `SELECT o.display_name FROM meetings m JOIN owners o ON o.subject = m.subject WHERE m.room = ?`, room).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, ErrNotFound
		}
		return name, false, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT p.display_name FROM participants p JOIN meetings m ON m.id = p.meeting_id
		WHERE m.room = ? AND p.identity = ? AND p.kind = 'guest'`, room, identity).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, ErrNotFound
	}
	return name, true, err
}
