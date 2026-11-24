package store

import "fmt"

// Test-only hooks, visible to store_test alone.

// SetUserVersionForTest sets the schema version.
func (s *Store) SetUserVersionForTest(v int) error {
	_, err := s.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, v))
	return err
}

// Migration1ForTest is the phase 2a schema, to build a 2a database with.
var Migration1ForTest = migrations[0]

// MigrationsBeforeContactsForTest are migrations 1–4: the schema before
// contacts (migration 5).
var MigrationsBeforeContactsForTest = append([]string(nil), migrations[:4]...)

// SchemaForTest is this build's schema version.
var SchemaForTest = len(migrations)

// MigrationsBeforeRecordingForTest are migrations 1–5: the schema before the
// recording (migration 6).
var MigrationsBeforeRecordingForTest = append([]string(nil), migrations[:5]...)

// LiveSharesForTest counts the meeting's share links not revoked.
func (s *Store) LiveSharesForTest(meetingID string) int {
	var n int
	s.db.QueryRow(`SELECT count(*) FROM recording_shares WHERE meeting_id = ? AND revoked_at IS NULL`, meetingID).Scan(&n)
	return n
}

// MigrationsBeforePostCallForTest are migrations 1–6: the schema before the
// post-call pass (migration 7).
var MigrationsBeforePostCallForTest = append([]string(nil), migrations[:6]...)

// ExecForTest runs one statement on the database.
func (s *Store) ExecForTest(q string, args ...any) error {
	_, err := s.db.Exec(q, args...)
	return err
}

// DraftStatesForTest is every draft of a meeting, superseded included:
// recipient → states in order.
func (s *Store) DraftStatesForTest(meetingID string) map[string][]string {
	rows, _ := s.db.Query(`SELECT recipient, state FROM mail_drafts WHERE meeting_id = ? ORDER BY created_at, rowid`, meetingID)
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var r, st string
		rows.Scan(&r, &st)
		out[r] = append(out[r], st)
	}
	return out
}
