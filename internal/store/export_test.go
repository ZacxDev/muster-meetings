package store

import (
	"context"
	"fmt"
)

// Test-only hooks, visible to store_test alone.

// AddParticipantForTest adds a waiting guest to a meeting (2a has no lobby
// yet, so nothing else writes the table).
func (s *Store) AddParticipantForTest(ctx context.Context, meetingID, name string) error {
	id := NewMeetingID()
	_, err := s.db.ExecContext(ctx, `INSERT INTO participants (id, meeting_id, kind, display_name, identity, state, knocked_at)
		VALUES (?, ?, 'guest', ?, ?, 'waiting', 0)`, id, meetingID, name, "guest-"+id)
	return err
}

// SetUserVersionForTest sets the schema version.
func (s *Store) SetUserVersionForTest(v int) error {
	_, err := s.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, v))
	return err
}
