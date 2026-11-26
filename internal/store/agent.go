package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// The meeting's agent (experimental; migration 9). The owner tells one of
// their own Claude Code sessions to join a live meeting: it joins through the
// MCP door with the owner's MCP token (JoinAgent), waits for the owner to
// press Ask agent (AskAgent, NextAsk), and answers in text (AnswerAsk), which
// everyone in the meeting sees (AgentFeed). 🔴 At most one agent is in a
// meeting, and at most one ask is outstanding: both are unique indexes, so
// two racing joins or asks cannot both succeed. An ask carries the meeting's
// lines since the agent's previous ask (all of them on its first): it stores
// only the segment-id range, never a copy. The question and the reply are
// words people wrote: never logged, never audited.

// Agent is the agent in a meeting.
type Agent struct {
	ID       int64     `json:"-"`
	Name     string    `json:"name"`
	Key      string    `json:"-"`
	TokenID  string    `json:"-"`
	JoinedAt time.Time `json:"joined_at"`
}

// Ask states.
const (
	AskOpen      = "open"
	AskDelivered = "delivered"
	AskAnswered  = "answered"
	AskCancelled = "cancelled"
)

// AgentAsk is one Ask agent.
type AgentAsk struct {
	ID          int64      `json:"id"`
	AgentID     int64      `json:"-"`
	FromSegment int64      `json:"-"`
	ToSegment   int64      `json:"-"`
	Question    string     `json:"question"`
	State       string     `json:"state"`
	AskedAt     time.Time  `json:"asked_at"`
	AnsweredAt  *time.Time `json:"answered_at,omitempty"`
	Reply       string     `json:"reply,omitempty"`
	// First says the ask carries the whole transcript so far (the agent's
	// first ask), not the lines since its previous one.
	First bool `json:"first"`
}

// AgentReply is an answered ask as the meeting shows it.
type AgentReply struct {
	AskID    int64  `json:"ask_id"`
	Name     string `json:"name"`
	Question string `json:"question"`
	Text     string `json:"text"`
	AtMS     int64  `json:"at_ms"` // meeting time of the answer
}

var (
	// ErrAgentTaken is a join while another agent is in the meeting.
	ErrAgentTaken = errors.New("another agent is in this meeting")
	// ErrNoAgent is an agent call (or an ask) with no agent in the meeting,
	// or with a key that is not the one in it.
	ErrNoAgent = errors.New("no such agent in this meeting")
	// ErrAskOutstanding is an ask while another is outstanding.
	ErrAskOutstanding = errors.New("an ask is outstanding")
	// ErrNoAsk is a wait with nothing asked, or a reply to an ask that is
	// not this agent's outstanding one.
	ErrNoAsk = errors.New("no such open ask")
	// ErrAnswered is a second, different reply to an ask.
	ErrAnswered = errors.New("the ask was already answered")
	// ErrAIOff is a join or an ask in a meeting whose AI switch is off.
	ErrAIOff = errors.New("the meeting's AI switch is off")
)

// liveForAgent is the meeting open, unexpired and the owner's, with its AI
// switch: ErrNotFound, ErrGone or ErrAIOff otherwise.
func (r *OwnerRepo) liveForAgent(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, meetingID string, now time.Time) error {
	if !ValidMeetingID(meetingID) {
		return ErrNotFound
	}
	var state string
	var ai int
	var created int64
	err := q.QueryRowContext(ctx, `SELECT state, ai, created_at FROM meetings WHERE id = ? AND subject = ?`, meetingID, r.subject).Scan(&state, &ai, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if state != "open" || !now.Before(time.UnixMilli(created).Add(LinkLifetime)) {
		return ErrGone
	}
	if ai != 1 {
		return ErrAIOff
	}
	return nil
}

const agentCols = `a.id, a.name, a.agent_key, a.token_id, a.joined_at`

func scanAgent(sc interface{ Scan(...any) error }) (Agent, error) {
	var a Agent
	var joined int64
	if err := sc.Scan(&a.ID, &a.Name, &a.Key, &a.TokenID, &joined); err != nil {
		return Agent{}, err
	}
	a.JoinedAt = time.UnixMilli(joined).UTC()
	return a, nil
}

// activeAgent is the meeting's agent in, scoped to the owner: ErrNoAgent
// when there is none.
func (r *OwnerRepo) activeAgent(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, meetingID string) (Agent, error) {
	a, err := scanAgent(q.QueryRowContext(ctx, `SELECT `+agentCols+` FROM meeting_agents a JOIN meetings m ON m.id = a.meeting_id
		WHERE a.meeting_id = ? AND m.subject = ? AND a.left_at IS NULL`, meetingID, r.subject))
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, ErrNoAgent
	}
	return a, err
}

// ActiveAgent is the agent in one of the owner's meetings, or ErrNoAgent.
func (r *OwnerRepo) ActiveAgent(ctx context.Context, meetingID string) (Agent, error) {
	if _, err := r.Meeting(ctx, meetingID); err != nil {
		return Agent{}, err
	}
	return r.activeAgent(ctx, r.s.db, meetingID)
}

// JoinAgent makes the MCP session that holds this repository's token the
// agent of one of the owner's LIVE meetings, under name (already checked by
// the caller), with a new key. 🔴 Only an MCP repository may (an owner
// session has no token to bind), only while its token stands (mayWrite),
// and only with the meeting's AI switch on. A join by the same token under
// the same name as the agent in answers that agent (already true): a retried
// join changes nothing. Any other join while an agent is in is
// ErrAgentTaken, with the agent that is.
func (r *OwnerRepo) JoinAgent(ctx context.Context, meetingID, name string, now time.Time) (Agent, bool, error) {
	if r.mcp == "" {
		return Agent{}, false, ErrNoSession
	}
	tx, err := r.s.db.BeginTx(ctx, nil)
	if err != nil {
		return Agent{}, false, err
	}
	defer tx.Rollback()
	if err := r.mayWrite(ctx, tx); err != nil {
		return Agent{}, false, err
	}
	if err := r.liveForAgent(ctx, tx, meetingID, now); err != nil {
		return Agent{}, false, err
	}
	cur, err := r.activeAgent(ctx, tx, meetingID)
	switch {
	case err == nil && cur.TokenID == r.mcp && cur.Name == name:
		return cur, true, nil
	case err == nil:
		return cur, false, ErrAgentTaken
	case !errors.Is(err, ErrNoAgent):
		return Agent{}, false, err
	}
	a := Agent{Name: name, Key: random(16), TokenID: r.mcp, JoinedAt: time.UnixMilli(ms(now)).UTC()}
	res, err := tx.ExecContext(ctx, `INSERT INTO meeting_agents (meeting_id, token_id, agent_key, name, joined_at) VALUES (?, ?, ?, ?, ?)`,
		meetingID, a.TokenID, a.Key, a.Name, ms(now))
	if err != nil {
		return Agent{}, false, err
	}
	if a.ID, err = res.LastInsertId(); err != nil {
		return Agent{}, false, err
	}
	if err := audit(ctx, tx, now, r.subject, meetingID, "agent.joined", r.via()); err != nil {
		return Agent{}, false, err
	}
	return a, false, tx.Commit()
}

// agentByKey is the agent in the meeting when it is the one key names and
// this repository's token joined it: ErrNoAgent otherwise (it left, was
// removed, another took its place, or the key is not its).
func (r *OwnerRepo) agentByKey(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, meetingID, key string) (Agent, error) {
	a, err := r.activeAgent(ctx, q, meetingID)
	if err != nil {
		return Agent{}, err
	}
	if key == "" || a.Key != key || a.TokenID != r.mcp {
		return Agent{}, ErrNoAgent
	}
	return a, nil
}

// endAgent takes the agent out (why: left or removed) and cancels its
// outstanding ask.
func endAgent(ctx context.Context, tx *sql.Tx, a Agent, why string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `UPDATE meeting_agents SET left_at = ?, left_why = ? WHERE id = ? AND left_at IS NULL`, ms(now), why, a.ID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE agent_asks SET state = 'cancelled' WHERE agent_id = ? AND state IN ('open', 'delivered')`, a.ID)
	return err
}

// LeaveAgent is the agent's own leave (MCP, by its key): it is out, and its
// outstanding ask is cancelled.
func (r *OwnerRepo) LeaveAgent(ctx context.Context, meetingID, key string, now time.Time) error {
	if r.mcp == "" {
		return ErrNoSession
	}
	// Read before the transaction: the store has one connection, which the
	// transaction holds until it ends.
	if _, err := r.Meeting(ctx, meetingID); err != nil {
		return err
	}
	tx, err := r.s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := r.mayWrite(ctx, tx); err != nil {
		return err
	}
	a, err := r.agentByKey(ctx, tx, meetingID, key)
	if err != nil {
		return err
	}
	if err := endAgent(ctx, tx, a, "left", now); err != nil {
		return err
	}
	if err := audit(ctx, tx, now, r.subject, meetingID, "agent.left", r.via()); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveAgent is the owner's Remove agent (an owner session only): the agent
// in the meeting is out, and its outstanding ask is cancelled; ErrNoAgent
// when none is in.
func (r *OwnerRepo) RemoveAgent(ctx context.Context, meetingID string, now time.Time) error {
	if r.session == "" {
		return ErrNoSession
	}
	if _, err := r.Meeting(ctx, meetingID); err != nil {
		return err
	}
	tx, err := r.s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	a, err := r.activeAgent(ctx, tx, meetingID)
	if err != nil {
		return err
	}
	if err := endAgent(ctx, tx, a, "removed", now); err != nil {
		return err
	}
	if err := audit(ctx, tx, now, r.subject, meetingID, "agent.removed", ""); err != nil {
		return err
	}
	return tx.Commit()
}

// MaxAskQuestion is the longest question an ask carries, in bytes (the
// caller cuts it in runes first).
const MaxAskQuestion = 4000

// AskAgent is the owner's Ask agent (an owner session only), on a LIVE
// meeting with its AI switch on and an agent in: a new outstanding ask
// carrying the lines since that agent's previous ask (from: the highest
// segment its earlier asks carried; 0 on its first, so all of them) up to
// the meeting's latest line now, and question (may be ""). ErrNoAgent with
// no agent, ErrAskOutstanding while another ask is.
func (r *OwnerRepo) AskAgent(ctx context.Context, meetingID, question string, now time.Time) (AgentAsk, error) {
	if r.session == "" {
		return AgentAsk{}, ErrNoSession
	}
	tx, err := r.s.db.BeginTx(ctx, nil)
	if err != nil {
		return AgentAsk{}, err
	}
	defer tx.Rollback()
	if err := r.liveForAgent(ctx, tx, meetingID, now); err != nil {
		return AgentAsk{}, err
	}
	a, err := r.activeAgent(ctx, tx, meetingID)
	if err != nil {
		return AgentAsk{}, err
	}
	var open int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM agent_asks WHERE meeting_id = ? AND state IN ('open', 'delivered')`, meetingID).Scan(&open); err != nil {
		return AgentAsk{}, err
	}
	if open > 0 {
		return AgentAsk{}, ErrAskOutstanding
	}
	// 🔴 The delta cursor: this agent's own earlier asks (a new agent starts
	// from the beginning: it has seen nothing).
	var from sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT max(to_segment) FROM agent_asks WHERE agent_id = ?`, a.ID).Scan(&from); err != nil {
		return AgentAsk{}, err
	}
	var to sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT max(id) FROM segments WHERE meeting_id = ?`, meetingID).Scan(&to); err != nil {
		return AgentAsk{}, err
	}
	k := AgentAsk{AgentID: a.ID, FromSegment: from.Int64, ToSegment: max(to.Int64, from.Int64), Question: question, State: AskOpen,
		AskedAt: time.UnixMilli(ms(now)).UTC(), First: !from.Valid}
	res, err := tx.ExecContext(ctx, `INSERT INTO agent_asks (meeting_id, agent_id, from_segment, to_segment, question, state, asked_at) VALUES (?, ?, ?, ?, ?, 'open', ?)`,
		meetingID, k.AgentID, k.FromSegment, k.ToSegment, k.Question, ms(now))
	if err != nil {
		return AgentAsk{}, err
	}
	if k.ID, err = res.LastInsertId(); err != nil {
		return AgentAsk{}, err
	}
	if err := audit(ctx, tx, now, r.subject, meetingID, "agent.asked", ""); err != nil {
		return AgentAsk{}, err
	}
	return k, tx.Commit()
}

const askCols = `k.id, k.agent_id, k.from_segment, k.to_segment, k.question, k.state, k.asked_at, k.answered_at, coalesce(k.reply, ''),
	NOT EXISTS (SELECT 1 FROM agent_asks e WHERE e.agent_id = k.agent_id AND e.id < k.id)`

func scanAsk(sc interface{ Scan(...any) error }) (AgentAsk, error) {
	var k AgentAsk
	var asked int64
	var answered sql.NullInt64
	if err := sc.Scan(&k.ID, &k.AgentID, &k.FromSegment, &k.ToSegment, &k.Question, &k.State, &asked, &answered, &k.Reply, &k.First); err != nil {
		return AgentAsk{}, err
	}
	k.AskedAt, k.AnsweredAt = time.UnixMilli(asked).UTC(), fromMS(answered)
	return k, nil
}

// AgentGone says the agent a wait is for is no longer the meeting's: the
// meeting ended (or is not the owner's), or the agent left or was removed.
// The wait answers it as "ended".
func (r *OwnerRepo) AgentGone(ctx context.Context, meetingID, key string, now time.Time) (bool, error) {
	m, err := r.Meeting(ctx, meetingID)
	if err != nil {
		return false, err
	}
	if m.State != "open" || !now.Before(m.LinkExpiresAt) {
		return true, nil
	}
	_, err = r.agentByKey(ctx, r.s.db, meetingID, key)
	if errors.Is(err, ErrNoAgent) {
		return true, nil
	}
	return false, err
}

// NextAsk is the agent's outstanding ask (by its key, through its token's
// MCP repository), marked delivered, and the lines it carries, oldest
// first: ErrNoAsk when nothing is asked, ErrNoAgent when the key is not the
// meeting's agent. An ask delivered and not yet answered is answered again
// (the agent's previous wait may never have reached it).
func (r *OwnerRepo) NextAsk(ctx context.Context, meetingID, key string, now time.Time) (AgentAsk, []Segment, error) {
	if r.mcp == "" {
		return AgentAsk{}, nil, ErrNoSession
	}
	tx, err := r.s.db.BeginTx(ctx, nil)
	if err != nil {
		return AgentAsk{}, nil, err
	}
	defer tx.Rollback()
	if err := r.mayWrite(ctx, tx); err != nil {
		return AgentAsk{}, nil, err
	}
	a, err := r.agentByKey(ctx, tx, meetingID, key)
	if err != nil {
		return AgentAsk{}, nil, err
	}
	k, err := scanAsk(tx.QueryRowContext(ctx, `SELECT `+askCols+` FROM agent_asks k WHERE k.agent_id = ? AND k.meeting_id = ? AND k.state IN ('open', 'delivered') ORDER BY k.id LIMIT 1`, a.ID, meetingID))
	if errors.Is(err, sql.ErrNoRows) {
		return AgentAsk{}, nil, ErrNoAsk
	}
	if err != nil {
		return AgentAsk{}, nil, err
	}
	// 🔴 No line leaves once the AI switch is off (the guests were told the
	// transcript goes to an AI service only while it is on): an ask made
	// before it went off is cancelled, not delivered.
	if err := r.liveForAgent(ctx, tx, meetingID, now); err != nil {
		if !errors.Is(err, ErrAIOff) {
			return AgentAsk{}, nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE agent_asks SET state = 'cancelled' WHERE id = ?`, k.ID); err != nil {
			return AgentAsk{}, nil, err
		}
		return AgentAsk{}, nil, errors.Join(ErrNoAsk, tx.Commit())
	}
	if k.State == AskOpen {
		if _, err := tx.ExecContext(ctx, `UPDATE agent_asks SET state = 'delivered', delivered_at = ? WHERE id = ?`, ms(now), k.ID); err != nil {
			return AgentAsk{}, nil, err
		}
		k.State = AskDelivered
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+segCols+` FROM segments s WHERE s.meeting_id = ? AND s.kind = 'text' AND s.id > ? AND s.id <= ? ORDER BY s.start_ms, s.id`,
		meetingID, k.FromSegment, k.ToSegment)
	if err != nil {
		return AgentAsk{}, nil, err
	}
	segs, err := scanSegments(rows)
	if err != nil {
		return AgentAsk{}, nil, err
	}
	return k, segs, tx.Commit()
}

// MaxAgentReply is the longest reply an agent may post, in bytes.
const MaxAgentReply = 16000

// AnswerAsk stores the agent's reply to its outstanding ask askID (by its
// key, through its token's MCP repository, while the token stands and the
// meeting is live). The same reply again answers unchanged true; another
// reply to an answered ask is ErrAnswered; an ask that is not this agent's
// outstanding one (cancelled, another's, none) is ErrNoAsk.
func (r *OwnerRepo) AnswerAsk(ctx context.Context, meetingID, key string, askID int64, text string, now time.Time) (AgentAsk, bool, error) {
	if r.mcp == "" {
		return AgentAsk{}, false, ErrNoSession
	}
	tx, err := r.s.db.BeginTx(ctx, nil)
	if err != nil {
		return AgentAsk{}, false, err
	}
	defer tx.Rollback()
	if err := r.mayWrite(ctx, tx); err != nil {
		return AgentAsk{}, false, err
	}
	if err := r.liveForAgent(ctx, tx, meetingID, now); err != nil && !errors.Is(err, ErrAIOff) {
		return AgentAsk{}, false, err
	}
	a, err := r.agentByKey(ctx, tx, meetingID, key)
	if err != nil {
		return AgentAsk{}, false, err
	}
	k, err := scanAsk(tx.QueryRowContext(ctx, `SELECT `+askCols+` FROM agent_asks k WHERE k.id = ? AND k.agent_id = ? AND k.meeting_id = ?`, askID, a.ID, meetingID))
	if errors.Is(err, sql.ErrNoRows) {
		return AgentAsk{}, false, ErrNoAsk
	}
	if err != nil {
		return AgentAsk{}, false, err
	}
	if k.State == AskAnswered {
		if k.Reply == text {
			return k, true, nil
		}
		return AgentAsk{}, false, ErrAnswered
	}
	// 🔴 Only an outstanding ask takes the reply: a cancelled one (Remove
	// agent, or the AI switch turned off before it was delivered) is no
	// row here.
	res, err := tx.ExecContext(ctx, `UPDATE agent_asks SET state = 'answered', answered_at = ?, reply = ? WHERE id = ? AND state IN ('open', 'delivered')`, ms(now), text, k.ID)
	if err != nil {
		return AgentAsk{}, false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return AgentAsk{}, false, ErrNoAsk
	}
	if err := audit(ctx, tx, now, r.subject, meetingID, "agent.replied", r.via()); err != nil {
		return AgentAsk{}, false, err
	}
	k.State, k.Reply = AskAnswered, text
	at := time.UnixMilli(ms(now)).UTC()
	k.AnsweredAt = &at
	return k, false, tx.Commit()
}

// AgentFeed is what a meeting's streams show of its agent: the agent in
// (nil for none), the outstanding ask (nil for none), and every reply so far,
// oldest first. The caller has bound the meeting to the stream (the owner's
// repository, or an admitted guest's ticket).
type AgentFeed struct {
	Agent   *Agent       `json:"agent"`
	Ask     *AgentAsk    `json:"ask"`
	Replies []AgentReply `json:"replies"`
}

// AgentFeed reads the meeting's feed, in one transaction: an answer that
// lands meanwhile is either in the replies with no ask outstanding, or not
// yet in them with its ask outstanding, never both.
func (s *Store) AgentFeed(ctx context.Context, meetingID string) (AgentFeed, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return AgentFeed{}, err
	}
	defer tx.Rollback()
	out := AgentFeed{Replies: []AgentReply{}}
	a, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM meeting_agents a WHERE a.meeting_id = ? AND a.left_at IS NULL`, meetingID))
	switch {
	case err == nil:
		out.Agent = &a
	case !errors.Is(err, sql.ErrNoRows):
		return AgentFeed{}, err
	}
	k, err := scanAsk(tx.QueryRowContext(ctx, `SELECT `+askCols+` FROM agent_asks k WHERE k.meeting_id = ? AND k.state IN ('open', 'delivered')`, meetingID))
	switch {
	case err == nil:
		out.Ask = &k
	case !errors.Is(err, sql.ErrNoRows):
		return AgentFeed{}, err
	}
	if out.Replies, err = agentReplies(ctx, tx, meetingID); err != nil {
		return AgentFeed{}, err
	}
	return out, nil
}

// AgentReplies is every answered ask of the meeting, oldest first, with the
// name of the agent that answered and the meeting time of the answer.
func (s *Store) AgentReplies(ctx context.Context, meetingID string) ([]AgentReply, error) {
	return agentReplies(ctx, s.db, meetingID)
}

func agentReplies(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, meetingID string) ([]AgentReply, error) {
	rows, err := q.QueryContext(ctx, `SELECT k.id, a.name, k.question, k.reply, k.answered_at - coalesce(m.started_at, m.created_at)
		FROM agent_asks k JOIN meeting_agents a ON a.id = k.agent_id JOIN meetings m ON m.id = k.meeting_id
		WHERE k.meeting_id = ? AND k.state = 'answered' ORDER BY k.answered_at, k.id`, meetingID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentReply{}
	for rows.Next() {
		var v AgentReply
		if err := rows.Scan(&v.AskID, &v.Name, &v.Question, &v.Text, &v.AtMS); err != nil {
			return nil, err
		}
		v.AtMS = max(v.AtMS, 0)
		out = append(out, v)
	}
	return out, rows.Err()
}
