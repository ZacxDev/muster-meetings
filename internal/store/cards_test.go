package store_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/ZacxDev/muster-meetings/internal/store"
)

// Migration 5 (contacts) on a database from before it: every row is kept,
// the card columns are NULL on every existing participant (so none is
// offered a card for a meeting that ended before the upgrade), the outbox
// starts empty, and the schema is 5. A build from before it refuses the
// database afterwards (TestReopenKeepsDataAndRefusesANewerSchema pins that
// refusal): the migration is one-way, as the README's rollback says.
func TestMigration5KeepsEveryRowAndAddsNoCard(t *testing.T) {
	p := filepath.Join(t.TempDir(), "m4.db")
	db, err := sql.Open("sqlite", "file:"+p+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range store.MigrationsBeforeContactsForTest {
		if _, err := db.Exec(m); err != nil {
			t.Fatalf("migration %d: %v", i+1, err)
		}
	}
	for _, q := range []string{
		`PRAGMA user_version = 4`,
		`INSERT INTO owners (subject, display_name, updated_at) VALUES ('U3ViamVjdE1pZ3JhdGlvbj', 'Ada L.', 1)`,
		`INSERT INTO meetings (id, subject, title, room, join_code_sha256, state, record, ai, created_at, ended_at)
		 VALUES ('bWlncmF0aW9uNW1l', 'U3ViamVjdE1pZ3JhdGlvbj', '', 'meeting-m5', 'hash-m5', 'ended', 0, 0, 1, 2)`,
		`INSERT INTO participants (id, meeting_id, kind, display_name, identity, ticket_sha256, state, knocked_at, admitted_at, left_at)
		 VALUES ('cGFydGljaXBhbnQ1', 'bWlncmF0aW9uNW1l', 'guest', 'Grace H.', 'guest-m5', 'ticket-m5', 'ended', 1, 1, 2)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	s, err := store.Open(p, keyA)
	if err != nil {
		t.Fatalf("opening a schema-4 database: %v", err)
	}
	defer s.Close()
	db, _ = sql.Open("sqlite", "file:"+p+"?mode=ro")
	defer db.Close()
	var v int
	db.QueryRow(`PRAGMA user_version`).Scan(&v)
	var name string
	var card, state sql.NullString
	if err := db.QueryRow(`SELECT display_name, card_sha256, card_state FROM participants WHERE id = 'cGFydGljaXBhbnQ1'`).Scan(&name, &card, &state); err != nil {
		t.Fatal(err)
	}
	n, err := s.CountOutbox(t.Context())
	if v != store.SchemaForTest || name != "Grace H." || card.Valid || state.Valid || err != nil || n != 0 {
		t.Fatalf("after migrations 5+: schema %d, row %q card %v state %v, outbox %d (%v)", v, name, card, state, n, err)
	}
	// An admitted guest of an ended meeting from before the upgrade is
	// eligible by the predicate, so the store would offer one only if
	// asked; nothing asks for a meeting that ended before (the guest's
	// stream is long gone). The offer itself works on the migrated row.
	if _, ok, err := s.OfferCard(t.Context(), "cGFydGljaXBhbnQ1", t0); !ok || err != nil {
		t.Fatalf("OfferCard on a migrated row = %v, %v", ok, err)
	}
}
