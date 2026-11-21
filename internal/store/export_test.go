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
