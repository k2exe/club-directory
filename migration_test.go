package main

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// preNetRemindMembersSchema is the members table exactly as it shipped
// before NetRemind added net_remind_opt_in/net_remind_lead_mins — i.e. every
// column in memberCols except the last two.
const preNetRemindMembersSchema = `
CREATE TABLE meta (
	id                INTEGER PRIMARY KEY CHECK (id = 1),
	version           INTEGER NOT NULL,
	secret            TEXT NOT NULL,
	club_name         TEXT NOT NULL,
	club_call_sign    TEXT NOT NULL,
	tagline           TEXT NOT NULL,
	public_directory  INTEGER NOT NULL,
	signups_open      INTEGER NOT NULL,
	show_sk_publicly  INTEGER NOT NULL,
	show_former_pub   INTEGER NOT NULL
);
CREATE TABLE members (
	id                 TEXT PRIMARY KEY,
	email              TEXT NOT NULL UNIQUE,
	name               TEXT NOT NULL DEFAULT '',
	call_sign          TEXT NOT NULL DEFAULT '',
	role               TEXT NOT NULL,
	status             TEXT NOT NULL,
	status_reason      TEXT NOT NULL DEFAULT '',
	status_changed_at  TEXT NOT NULL,
	sk_date            TEXT NOT NULL DEFAULT '',
	contact_email      TEXT NOT NULL DEFAULT '',
	phone              TEXT NOT NULL DEFAULT '',
	address_line1      TEXT NOT NULL DEFAULT '',
	address_line2      TEXT NOT NULL DEFAULT '',
	address_city       TEXT NOT NULL DEFAULT '',
	address_state      TEXT NOT NULL DEFAULT '',
	address_postal     TEXT NOT NULL DEFAULT '',
	photo_file         TEXT NOT NULL DEFAULT '',
	share_email        INTEGER NOT NULL DEFAULT 0,
	share_phone        INTEGER NOT NULL DEFAULT 0,
	share_address      INTEGER NOT NULL DEFAULT 0,
	share_photo        INTEGER NOT NULL DEFAULT 0,
	needs_review       INTEGER NOT NULL DEFAULT 0,
	totp_secret        TEXT NOT NULL DEFAULT '',
	totp_enabled_at    TEXT,
	totp_last_step     INTEGER NOT NULL DEFAULT 0,
	backup_codes       TEXT NOT NULL DEFAULT '[]',
	session_epoch      INTEGER NOT NULL DEFAULT 0,
	admin_notes        TEXT NOT NULL DEFAULT '',
	joined_at          TEXT NOT NULL,
	updated_at         TEXT NOT NULL,
	last_login_at      TEXT
);
`

// Regression: OpenStore against a database created before NetRemind's two
// new members columns existed used to fail every query that touched them
// ("no such column: net_remind_opt_in") while still reporting a healthy
// open — silently locking out every existing member. OpenStore must add
// the missing columns itself.
func TestRegressionMigratesPreNetRemindDatabase(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "directory.db")

	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(preNetRemindMembersSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO meta (id, version, secret, club_name, club_call_sign, tagline,
			public_directory, signups_open, show_sk_publicly, show_former_pub)
		VALUES (1, 1, 'c2VjcmV0', 'Old Club', '', '', 1, 0, 1, 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO members (id, email, role, status, status_reason, status_changed_at,
			joined_at, updated_at)
		VALUES ('existing1', 'veteran@example.com', 'member', 'active', '', '2020-01-01T00:00:00Z',
			'2020-01-01T00:00:00Z', '2020-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore on a pre-NetRemind database failed: %v", err)
	}
	defer st.Close()

	// The pre-existing member must still be readable...
	m, err := st.ByEmail("veteran@example.com")
	if err != nil {
		t.Fatalf("ByEmail on a migrated row: %v", err)
	}
	if m.ID != "existing1" {
		t.Fatalf("got member %+v, want existing1", m)
	}
	// ...and All() must not silently come back empty.
	if all := st.All(); len(all) != 1 {
		t.Fatalf("All() returned %d members, want 1", len(all))
	}
	// ...and it must be fully writable, including the new columns.
	updated, err := st.Update("existing1", func(x *Member) error {
		x.NetRemindOptIn = true
		x.NetRemindLeadMinutes = 15
		return nil
	})
	if err != nil {
		t.Fatalf("Update after migration: %v", err)
	}
	if !updated.NetRemindOptIn || updated.NetRemindLeadMinutes != 15 {
		t.Errorf("new columns did not round-trip: %+v", updated)
	}

	// Reopening an already-migrated database must be a safe no-op.
	st2, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("reopening an already-migrated database: %v", err)
	}
	st2.Close()
}
