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
