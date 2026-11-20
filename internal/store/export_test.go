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

// SealedForTest is a meeting's sealed code (nil when NULL).
func (s *Store) SealedForTest(id string) []byte {
	var b []byte
	if err := s.db.QueryRow(`SELECT join_code_sealed FROM meetings WHERE id = ?`, id).Scan(&b); err != nil {
		panic(err)
	}
	return b
}

// SetSealedForTest overwrites a meeting's sealed code.
func (s *Store) SetSealedForTest(id string, b []byte) {
	if _, err := s.db.Exec(`UPDATE meetings SET join_code_sealed = ? WHERE id = ?`, b, id); err != nil {
		panic(err)
	}
}

// EndKeepingLinkForTest ends a meeting without touching its sealed code.
func (s *Store) EndKeepingLinkForTest(id string) {
	if _, err := s.db.Exec(`UPDATE meetings SET state = 'ended', ended_at = 1 WHERE id = ?`, id); err != nil {
		panic(err)
	}
}
