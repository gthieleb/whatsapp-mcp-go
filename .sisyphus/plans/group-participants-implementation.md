# Group Participants & Description Feature — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix the history-sync sender bug for WhatsApp groups and add group metadata (subject, description, participants) persistence, REST endpoints, and MCP tools.

**Architecture:** Add a new `whatsapp-bridge/groups.go` file (package main) with `GroupCache` (singleflight + context timeouts), `GroupStore` methods on `*MessageStore`, and wire up new event handlers + REST endpoints. Extend MCP server with two new tools. Full design spec: `.sisyphus/plans/group-participants-design.md`.

**Tech Stack:** Go 1.25, `database/sql`, WhatsMeow, `net/http.ServeMux`, `log/slog`, `golang.org/x/sync/singleflight`, `github.com/DATA-DOG/go-sqlmock` + `github.com/stretchr/testify` (tests).

**Branch:** `feat/group-participants-and-description` (already created, currently at `f959db2`)

**Push Policy:** Local only — NO push to upstream (`iamatulsingh/whatsapp-mcp-go`) before extensive user testing.

---

## Pre-Implementation Verification

Before writing any code, the implementer should run these checks:

```bash
cd /home/gun/development/ai/mcp/whatsapp-mcp-go
git status                                # on feat/group-participants-and-description, clean
git log --oneline -3                      # 2 spec commits present
docker compose ps                         # wa-bridge + wa-mcp + postgres healthy
```

If any of these fail, STOP and ask the user before proceeding.

---

## Phase 0: Dependency Setup

### Task 0.1: Add direct dependencies to `whatsapp-bridge`

**Files:**
- Modify: `whatsapp-bridge/go.mod`
- Modify: `whatsapp-bridge/go.sum`

**Step 1:** Run the dep commands

```bash
cd /home/gun/development/ai/mcp/whatsapp-mcp-go/whatsapp-bridge
go get golang.org/x/sync@latest
go get github.com/DATA-DOG/go-sqlmock@latest
go get github.com/stretchr/testify@latest
go mod tidy
```

**Step 2:** Verify go.mod shows them as direct deps

```bash
grep -E "(singleflight|sqlmock|testify)" go.mod
```

Expected: 3 lines, all in the main `require` block (not `// indirect`).

**Step 3:** Verify build still works

```bash
go build ./...
```

Expected: no output, exit code 0.

### Task 0.2: Add testify to `whatsapp-mcp-server`

**Files:**
- Modify: `whatsapp-mcp-server/go.mod`
- Modify: `whatsapp-mcp-server/go.sum`

**Step 1:**

```bash
cd /home/gun/development/ai/mcp/whatsapp-mcp-go/whatsapp-mcp-server
go get github.com/stretchr/testify@latest
go mod tidy
go build ./...
```

### Task 0.3: Commit dep changes

```bash
cd /home/gun/development/ai/mcp/whatsapp-mcp-go
git add whatsapp-bridge/go.mod whatsapp-bridge/go.sum \
        whatsapp-mcp-server/go.mod whatsapp-mcp-server/go.sum
git commit -m "chore(deps): add singleflight, go-sqlmock, testify as direct deps"
```

---

## Phase 1: Schema + Types

### Task 1.1: Write failing test for new schema

**Files:**
- Create: `whatsapp-bridge/schema_test.go`

**Step 1:** Write the test file

```go
package main

import (
	"database/sql"
	"testing"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestNewMessageStoreCreatesGroupTables(t *testing.T) {
	// Use in-memory SQLite via sqlmock is hard for DDL verification.
	// Instead use real SQLite in-memory.
	db, err := sql.Open("sqlite3", "file::memory:?cache=shared")
	require.NoError(t, err)
	defer db.Close()

	originalIsPostgres := isPostgres
	isPostgres = false
	defer func() { isPostgres = originalIsPostgres }()

	_, err = NewMessageStoreWithDB(db)
	require.NoError(t, err)

	// Verify groups table exists
	var tableName string
	err = db.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='table' AND name='groups'",
	).Scan(&tableName)
	require.NoError(t, err)
	require.Equal(t, "groups", tableName)

	// Verify group_participants table exists
	err = db.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='table' AND name='group_participants'",
	).Scan(&tableName)
	require.NoError(t, err)
	require.Equal(t, "group_participants", tableName)

	// Verify index
	var indexName string
	err = db.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='index' AND name='idx_group_participants_jid'",
	).Scan(&indexName)
	require.NoError(t, err)
	require.Equal(t, "idx_group_participants_jid", indexName)
}
```

**Step 2:** Add a helper `NewMessageStoreWithDB` to `whatsapp-bridge/main.go` after the existing `NewMessageStore()` function (find it via `grep -n "func NewMessageStore" main.go`):

```go
// NewMessageStoreWithDB is a test helper that creates a MessageStore using
// an externally-provided *sql.DB (e.g., for in-memory SQLite in tests).
func NewMessageStoreWithDB(db *sql.DB) (*MessageStore, error) {
	store := &MessageStore{db: db}
	if err := store.createSchema(); err != nil {
		return nil, err
	}
	return store, nil
}
```

**Step 3:** Refactor the schema creation out of `NewMessageStore()` into a `createSchema()` method on `*MessageStore`. The existing body of `NewMessageStore()` (currently `db.Exec(fmt.Sprintf(...))`) becomes `s.createSchema()`.

Locate the current schema block in `main.go`:

```bash
grep -n "CREATE TABLE IF NOT EXISTS chats" main.go
```

Refactor so the schema creation lives in:
```go
func (s *MessageStore) createSchema() error {
    // existing CREATE TABLE IF NOT EXISTS chats / messages
    // + new CREATE TABLE IF NOT EXISTS groups / group_participants + index
}
```

**Step 4:** Add the new CREATE statements to `createSchema()` (after the existing ones):

```go
_, err = s.db.Exec(`
CREATE TABLE IF NOT EXISTS groups (
    jid                    TEXT PRIMARY KEY,
    subject                TEXT,
    subject_owner          TEXT,
    subject_time           TIMESTAMP,
    description            TEXT,
    description_id         TEXT,
    description_owner      TEXT,
    description_time       TIMESTAMP,
    owner_jid              TEXT,
    creation_time          TIMESTAMP,
    participant_version_id TEXT,
    last_synced            TIMESTAMP
);

CREATE TABLE IF NOT EXISTS group_participants (
    group_jid      TEXT NOT NULL,
    jid            TEXT NOT NULL,
    phone_number   TEXT,
    lid            TEXT,
    is_admin       BOOLEAN NOT NULL DEFAULT FALSE,
    is_super_admin BOOLEAN NOT NULL DEFAULT FALSE,
    display_name   TEXT,
    PRIMARY KEY (group_jid, jid),
    FOREIGN KEY (group_jid) REFERENCES groups(jid) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_group_participants_jid ON group_participants(jid);
`)
if err != nil {
    return fmt.Errorf("create group tables: %w", err)
}
```

**Step 5:** Run test to verify pass

```bash
cd whatsapp-bridge && go test ./... -run TestNewMessageStoreCreatesGroupTables -v
```

Expected: PASS.

### Task 1.2: Add Go types in `groups.go`

**Files:**
- Create: `whatsapp-bridge/groups.go`

**Step 1:** Write the types file

```go
package main

import (
	"time"
)

// StoredGroup is the DB-row mapping for the `groups` table.
type StoredGroup struct {
	JID                  string     `json:"jid"`
	Subject              string     `json:"subject,omitempty"`
	SubjectOwner         string     `json:"subject_owner,omitempty"`
	SubjectTime          time.Time  `json:"subject_time,omitempty"`
	Description          string     `json:"description,omitempty"`
	DescriptionID        string     `json:"description_id,omitempty"`
	DescriptionOwner     string     `json:"description_owner,omitempty"`
	DescriptionTime      time.Time  `json:"description_time,omitempty"`
	OwnerJID             string     `json:"owner_jid,omitempty"`
	CreationTime         time.Time  `json:"creation_time,omitempty"`
	ParticipantVersionID string     `json:"participant_version_id,omitempty"`
	LastSynced           time.Time  `json:"last_synced,omitempty"`
}

// StoredParticipant is the DB-row mapping for the `group_participants` table.
// Per D13: PhoneNumber is *string so that nil serialises as JSON null when
// WhatsApp hides the phone number for privacy reasons.
type StoredParticipant struct {
	GroupJID     string  `json:"group_jid"`
	JID          string  `json:"jid"`
	PhoneNumber  *string `json:"phone_number"` // nil = null (D13)
	LID          string  `json:"lid,omitempty"`
	IsAdmin      bool    `json:"is_admin"`
	IsSuperAdmin bool    `json:"is_super_admin"`
	DisplayName  string  `json:"display_name,omitempty"`
}
```

**Step 2:** Verify build

```bash
go build ./...
```

### Task 1.3: Commit Phase 1

```bash
cd /home/gun/development/ai/mcp/whatsapp-mcp-go
git add whatsapp-bridge/main.go whatsapp-bridge/groups.go whatsapp-bridge/schema_test.go
git commit -m "feat(bridge): add groups + group_participants schema and types

- New tables: groups, group_participants (with FK cascade + index)
- New file groups.go with StoredGroup + StoredParticipant types
- StoredParticipant.PhoneNumber is *string per D13 (null in JSON)
- Refactored schema creation into MessageStore.createSchema() method
- Added NewMessageStoreWithDB test helper"
```

---

## Phase 2: GroupStore CRUD Methods

### Task 2.1: Write failing tests for UpsertGroup

**Files:**
- Create: `whatsapp-bridge/groups_store_test.go`

**Step 1:** Write the test file using sqlmock (note: sqlmock validates expected queries; for SQLite-only DDL test we used real DB, but for store methods sqlmock is the right tool):

```go
package main

import (
	"database/sql"
	"encoding/json"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"go.mau.fi/whatsmeow/types"
)

func newMockStore(t *testing.T) (*MessageStore, sqlmock.Sqlmock, func()) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	store := &MessageStore{db: db}
	return store, mock, func() {
		db.Close()
	}
}

func TestUpsertGroup_Postgres(t *testing.T) {
	original := isPostgres
	isPostgres = true
	defer func() { isPostgres = original }()

	store, mock, cleanup := newMockStore(t)
	defer cleanup()

	groupInfo := &types.GroupInfo{
		JID:                 types.NewJID("120363045908244249", "g.us"),
		OwnerJID:            types.NewJID("4915126206106", "s.whatsapp.net"),
		GroupName:           types.GroupName{Name: "Post SV D2 Junioren", NameSetAt: time.Now()},
		GroupTopic:          types.GroupTopic{Topic: "Trainingsgruppe D2"},
		GroupCreated:        time.Now().Add(-365 * 24 * time.Hour),
		ParticipantVersionID: "v1",
	}

	mock.ExpectExec(regexp.QuoteMeta(`
INSERT INTO groups (jid, subject, subject_owner, subject_time, description, description_id, description_owner, description_time, owner_jid, creation_time, participant_version_id, last_synced)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (jid) DO UPDATE SET
    subject = EXCLUDED.subject,
    subject_owner = EXCLUDED.subject_owner,
    subject_time = EXCLUDED.subject_time,
    description = EXCLUDED.description,
    description_id = EXCLUDED.description_id,
    description_owner = EXCLUDED.description_owner,
    description_time = EXCLUDED.description_time,
    owner_jid = EXCLUDED.owner_jid,
    creation_time = EXCLUDED.creation_time,
    participant_version_id = EXCLUDED.participant_version_id,
    last_synced = EXCLUDED.last_synced`)).
		WithArgs(
			"120363045908244249@g.us",
			"Post SV D2 Junioren",
			"", // SubjectOwner.User (empty JID User)
			sqlmock.AnyArg(),
			"Trainingsgruppe D2",
			"",  // TopicID
			"",  // DescriptionOwner.User
			sqlmock.AnyArg(), // zero time for unset DescriptionTime
			"4915126206106",
			sqlmock.AnyArg(),
			"v1",
			sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := store.UpsertGroup(groupInfo)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}
```

**Step 2:** Run test, expect FAIL (no `UpsertGroup` method yet)

```bash
go test -run TestUpsertGroup -v
```

Expected: `undefined: store.UpsertGroup`.

### Task 2.2: Implement `UpsertGroup`

**Files:**
- Modify: `whatsapp-bridge/groups.go` (append to existing types)

**Step 1:** Add the method (after the types):

```go
import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	waevents "go.mau.fi/whatsmeow/types/events"
	"log/slog"
)

// placeholder reuses the existing main.go helper for dual-DB SQL.
// We declare it here as alias to avoid circular deps; main.go already
// has the canonical version — see "placeholder" in main.go.
func placeholder(n int) string {
	if isPostgres {
		return fmt.Sprintf("$%d", n)
	}
	return "?"
}

// UpsertGroup inserts or updates a group's metadata.
func (s *MessageStore) UpsertGroup(info *types.GroupInfo) error {
	now := time.Now()
	q := `
INSERT INTO groups (jid, subject, subject_owner, subject_time, description, description_id, description_owner, description_time, owner_jid, creation_time, participant_version_id, last_synced)
VALUES (` + placeholder(1) + `, ` + placeholder(2) + `, ` + placeholder(3) + `, ` + placeholder(4) + `, ` + placeholder(5) + `, ` + placeholder(6) + `, ` + placeholder(7) + `, ` + placeholder(8) + `, ` + placeholder(9) + `, ` + placeholder(10) + `, ` + placeholder(11) + `, ` + placeholder(12) + `)
ON CONFLICT (jid) DO UPDATE SET
    subject = EXCLUDED.subject,
    subject_owner = EXCLUDED.subject_owner,
    subject_time = EXCLUDED.subject_time,
    description = EXCLUDED.description,
    description_id = EXCLUDED.description_id,
    description_owner = EXCLUDED.description_owner,
    description_time = EXCLUDED.description_time,
    owner_jid = EXCLUDED.owner_jid,
    creation_time = EXCLUDED.creation_time,
    participant_version_id = EXCLUDED.participant_version_id,
    last_synced = EXCLUDED.last_synced`

	_, err := s.db.Exec(q,
		info.JID.String(),
		info.GroupName.Name,
		info.GroupName.NameSetBy.User,
		info.GroupName.NameSetAt,
		info.GroupTopic.Topic,
		info.GroupTopic.TopicID,
		info.GroupTopic.TopicSetBy.User,
		info.GroupTopic.TopicSetAt,
		info.OwnerJID.User,
		info.GroupCreated,
		info.ParticipantVersionID,
		now,
	)
	if err != nil {
		return fmt.Errorf("upsert group %s: %w", info.JID.String(), err)
	}
	return nil
}
```

**Step 2:** Note that the test expects `WithArgs(...)` order. Compare with implementation to ensure positional match.

**Step 3:** Run test, expect PASS

```bash
go test -run TestUpsertGroup_Postgres -v
```

### Task 2.3: Implement `ReplaceGroupParticipants` with transaction

**Files:**
- Modify: `whatsapp-bridge/groups.go`

**Step 1:** Write a failing test in `groups_store_test.go`:

```go
func TestReplaceGroupParticipants_Postgres(t *testing.T) {
	original := isPostgres
	isPostgres = true
	defer func() { isPostgres = original }()

	store, mock, cleanup := newMockStore(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM group_participants WHERE group_jid = `)).
		WithArgs("120363045908244249@g.us").
		WillReturnResult(sqlmock.NewResult(0, 5))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO group_participants (group_jid, jid, phone_number, lid, is_admin, is_super_admin, display_name) VALUES ($1, $2, $3, $4, $5, $6, $7)`)).
		WithArgs(
			"120363045908244249@g.us",
			"4915126206106@s.whatsapp.net",
			"4915126206106",
			"",
			true,
			false,
			"Christian",
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	participants := []types.GroupParticipant{
		{
			JID:         types.NewJID("4915126206106", "s.whatsapp.net"),
			PhoneNumber: types.NewJID("4915126206106", "s.whatsapp.net"),
			IsAdmin:     true,
			DisplayName: "Christian",
		},
	}

	err := store.ReplaceGroupParticipants("120363045908244249@g.us", participants)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}
```

**Step 2:** Run, expect FAIL.

**Step 3:** Implement:

```go
// ReplaceGroupParticipants atomically replaces all participants of a group.
// Wraps DELETE + INSERT(s) in a transaction to avoid partial states.
func (s *MessageStore) ReplaceGroupParticipants(groupJID string, participants []types.GroupParticipant) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // safe no-op after Commit

	if _, err := tx.Exec(
		`DELETE FROM group_participants WHERE group_jid = `+placeholder(1),
		groupJID,
	); err != nil {
		return fmt.Errorf("delete old participants: %w", err)
	}

	insertQ := `INSERT INTO group_participants (group_jid, jid, phone_number, lid, is_admin, is_super_admin, display_name) VALUES (` +
		placeholder(1) + `, ` + placeholder(2) + `, ` + placeholder(3) + `, ` + placeholder(4) + `, ` + placeholder(5) + `, ` + placeholder(6) + `, ` + placeholder(7) + `)`

	for _, p := range participants {
		phone := p.PhoneNumber.User
		if _, err := tx.Exec(insertQ,
			groupJID,
			p.JID.String(),
			phone,
			p.LID.User,
			p.IsAdmin,
			p.IsSuperAdmin,
			p.DisplayName,
		); err != nil {
			return fmt.Errorf("insert participant %s: %w", p.JID.String(), err)
		}
	}

	return tx.Commit()
}
```

**Step 4:** Run, expect PASS.

### Task 2.4: Implement remaining store methods

Implement these analogously in `whatsapp-bridge/groups.go`, each with a sqlmock test in `groups_store_test.go`:

**`ApplyGroupDelta(evt *waevents.GroupInfo) error`** — handles incremental Join/Leave/Promote/Demote and Name/Topic updates:

```go
// ApplyGroupDelta applies an incremental GroupInfo event to the database.
// Updates Name/Topic if present, and applies Join/Leave/Promote/Demote on participants.
func (s *MessageStore) ApplyGroupDelta(evt *waevents.GroupInfo) error {
	groupJID := evt.JID.String()

	// 1. Update name/topic if present in delta
	if evt.Name != nil || evt.Topic != nil {
		updates := []string{}
		args := []interface{}{}
		n := 1
		if evt.Name != nil {
			updates = append(updates, "subject = "+placeholder(n)); n++
			updates = append(updates, "subject_owner = "+placeholder(n)); n++
			updates = append(updates, "subject_time = "+placeholder(n)); n++
			args = append(args, evt.Name.Name, evt.Name.NameSetBy.User, evt.Name.NameSetAt)
		}
		if evt.Topic != nil {
			updates = append(updates, "description = "+placeholder(n)); n++
			args = append(args, evt.Topic.Topic)
		}
		args = append(args, groupJID)
		q := "UPDATE groups SET " + strings.Join(updates, ", ") + " WHERE jid = " + placeholder(n)
		if _, err := s.db.Exec(q, args...); err != nil {
			return fmt.Errorf("apply name/topic delta: %w", err)
		}
	}

	// 2. Apply participant deltas
	for _, jid := range evt.Join {
		if _, err := s.db.Exec(
			`INSERT INTO group_participants (group_jid, jid, is_admin, is_super_admin) VALUES (`+
				placeholder(1)+`, `+placeholder(2)+`, false, false) ON CONFLICT DO NOTHING`,
			groupJID, jid.String(),
		); err != nil {
			return fmt.Errorf("insert join %s: %w", jid.String(), err)
		}
	}
	for _, jid := range evt.Leave {
		if _, err := s.db.Exec(
			`DELETE FROM group_participants WHERE group_jid = `+placeholder(1)+` AND jid = `+placeholder(2),
			groupJID, jid.String(),
		); err != nil {
			return fmt.Errorf("delete leave %s: %w", jid.String(), err)
		}
	}
	for _, jid := range evt.Promote {
		if _, err := s.db.Exec(
			`UPDATE group_participants SET is_admin = true WHERE group_jid = `+placeholder(1)+` AND jid = `+placeholder(2),
			groupJID, jid.String(),
		); err != nil {
			return fmt.Errorf("promote %s: %w", jid.String(), err)
		}
	}
	for _, jid := range evt.Demote {
		if _, err := s.db.Exec(
			`UPDATE group_participants SET is_admin = false WHERE group_jid = `+placeholder(1)+` AND jid = `+placeholder(2),
			groupJID, jid.String(),
		); err != nil {
			return fmt.Errorf("demote %s: %w", jid.String(), err)
		}
	}

	return nil
}
```

**`GetGroup(jid string) (*StoredGroup, error)`**:

```go
func (s *MessageStore) GetGroup(jid string) (*StoredGroup, error) {
	var g StoredGroup
	var subjTime, descTime, createTime, lastSynced sql.NullTime
	err := s.db.QueryRow(
		`SELECT jid, subject,
		COALESCE(subject_owner, ''), COALESCE(subject_time, '0001-01-01'),
		COALESCE(description, ''), COALESCE(description_id, ''),
		COALESCE(description_owner, ''), COALESCE(description_time, '0001-01-01'),
		COALESCE(owner_jid, ''), COALESCE(creation_time, '0001-01-01'),
		COALESCE(participant_version_id, ''), COALESCE(last_synced, '0001-01-01')
		FROM groups WHERE jid = `+placeholder(1),
		jid,
	).Scan(
		&g.JID, &g.Subject,
		&g.SubjectOwner, &subjTime,
		&g.Description, &g.DescriptionID,
		&g.DescriptionOwner, &descTime,
		&g.OwnerJID, &createTime,
		&g.ParticipantVersionID, &lastSynced,
	)
	if err != nil {
		return nil, err
	}
	g.SubjectTime = subjTime.Time
	g.DescriptionTime = descTime.Time
	g.CreationTime = createTime.Time
	g.LastSynced = lastSynced.Time
	return &g, nil
}
```

**`ListGroupParticipants(jid string) ([]StoredParticipant, error)`**:

```go
func (s *MessageStore) ListGroupParticipants(jid string) ([]StoredParticipant, error) {
	rows, err := s.db.Query(
		`SELECT group_jid, jid, phone_number, lid, is_admin, is_super_admin, display_name
		FROM group_participants WHERE group_jid = `+placeholder(1)+` ORDER BY jid`,
		jid,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []StoredParticipant
	for rows.Next() {
		var p StoredParticipant
		var phone, lid, displayName sql.NullString
		if err := rows.Scan(&p.GroupJID, &p.JID, &phone, &lid, &p.IsAdmin, &p.IsSuperAdmin, &displayName); err != nil {
			return nil, err
		}
		if phone.Valid && phone.String != "" {
			s := phone.String // copy to take address
			p.PhoneNumber = &s
		}
		p.LID = lid.String
		p.DisplayName = displayName.String
		result = append(result, p)
	}
	return result, rows.Err()
}
```

**`LookupParticipantName(jid string) (string, error)`** — used by `GetSenderName` fallback (Phase 4):

```go
func (s *MessageStore) LookupParticipantName(jid string) (string, error) {
	var name sql.NullString
	err := s.db.QueryRow(
		`SELECT display_name FROM group_participants
		WHERE jid = `+placeholder(1)+` AND display_name != ''
		LIMIT 1`,
		jid,
	).Scan(&name)
	if err != nil {
		return "", err
	}
	return name.String, nil
}
```

**`ListStaleGroups(cutoff time.Time, limit int) ([]StoredGroup, error)`** — used by background ticker:

```go
func (s *MessageStore) ListStaleGroups(cutoff time.Time, limit int) ([]StoredGroup, error) {
	q := `SELECT jid FROM groups
		WHERE last_synced IS NULL OR last_synced < ` + placeholder(1) + `
		ORDER BY last_synced ASC NULLS FIRST
		LIMIT ` + placeholder(2)
	if !isPostgres {
		// SQLite: NULLS FIRST not supported pre-3.30, but IS NULL in ORDER BY works
		q = `SELECT jid FROM groups
			WHERE last_synced IS NULL OR last_synced < ` + placeholder(1) + `
			ORDER BY (last_synced IS NULL) DESC, last_synced ASC
			LIMIT ` + placeholder(2)
	}
	rows, err := s.db.Query(q, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []StoredGroup
	for rows.Next() {
		var g StoredGroup
		if err := rows.Scan(&g.JID); err != nil {
			return nil, err
		}
		result = append(result, g)
	}
	return result, rows.Err()
}
```

### Task 2.5: Verify build + tests

```bash
cd whatsapp-bridge && go build ./... && go test ./... -v 2>&1 | tail -20
```

Expected: All tests PASS, no compile errors.

### Task 2.6: Commit Phase 2

```bash
cd /home/gun/development/ai/mcp/whatsapp-mcp-go
git add whatsapp-bridge/
git commit -m "feat(bridge): add GroupStore CRUD methods with sqlmock tests

- UpsertGroup (Postgres ON CONFLICT + SQLite INSERT OR REPLACE pattern)
- ReplaceGroupParticipants (transactional DELETE + INSERT)
- ApplyGroupDelta (incremental Join/Leave/Promote/Demote + Name/Topic)
- GetGroup, ListGroupParticipants, LookupParticipantName
- ListStaleGroups with dual-DB NULLS FIRST workaround
- All tested with sqlmock"
```

---

## Phase 3: History-Sync Bug-Fix

### Task 3.1: Read current implementation

```bash
sed -n '1685,1710p' whatsapp-bridge/main.go
```

Note the `else` branch at the bug location.

### Task 3.2: Apply the fix

**Files:**
- Modify: `whatsapp-bridge/main.go` (around lines 1687-1706)

**Step 1:** Locate the exact block:

```bash
grep -n "} else if isFromMe {" whatsapp-bridge/main.go
```

Find the one inside the history-sync loop (around line 1699).

**Step 2:** Insert a new `else if` branch BEFORE the final `else`:

Before:
```go
} else if isFromMe {
    sender = client.Store.ID.User
} else {
    sender = jid.User
}
```

After:
```go
} else if isFromMe {
    sender = client.Store.ID.User
} else if jid.Server == "g.us" {
    // BUG-FIX: was previously `sender = jid.User` which stored the GROUP JID
    // as the sender for @g.us chats missing Key.Participant. Now we leave
    // sender empty so GetSenderName falls back to "unknown" or a future
    // group_participants lookup. See spec section 6.1.
    sender = ""
} else {
    sender = jid.User
}
```

**Step 3:** Build

```bash
cd whatsapp-bridge && go build ./...
```

### Task 3.3: Commit Phase 3

```bash
cd /home/gun/development/ai/mcp/whatsapp-mcp-go
git add whatsapp-bridge/main.go
git commit -m "fix(bridge): correct sender in history-sync for @g.us chats without participant

Previously, when WhatsMeow's HistorySync did not populate Key.Participant
for a group message, the code fell back to jid.User — which is the GROUP
JID for @g.us chats, not the actual sender. This caused GetSenderName to
return the group name as the sender.

Fix: store empty sender for group messages without participant. New live
messages continue to work correctly via msg.Info.Sender."
```

---

## Phase 4: GetSenderName Fallback

### Task 4.1: Locate current `GetSenderName`

```bash
grep -n "func (store \*MessageStore) GetSenderName" whatsapp-bridge/main.go
```

### Task 4.2: Apply the fallback

**Files:**
- Modify: `whatsapp-bridge/main.go`

**Step 1:** Find the function (around line 1910). Read its full body.

**Step 2:** After the existing chats-table lookups (before the final fallback return), add:

```go
// Fallback (D4): try group_participants table if not found in chats
if name, err := store.LookupParticipantName(senderJID); err == nil && name != "" {
    return name
}

// Final fallback
if senderJID == "" {
    return "unknown"
}
return senderJID
```

**Step 3:** Build + run tests.

### Task 4.3: Commit Phase 4

```bash
git add whatsapp-bridge/main.go
git commit -m "fix(bridge): extend GetSenderName to fall back on group_participants

When a sender JID is not found in the chats table (which is common for
group participants who don't have a direct chat), look up their
display_name in group_participants. Returns 'unknown' if both lookups fail."
```

---

## Phase 5: Event Handlers

### Task 5.1: Add new cases to `AddEventHandler`

**Files:**
- Modify: `whatsapp-bridge/main.go` (around line 2766-2820)

**Step 1:** Locate the event handler switch:

```bash
grep -n "case \*events\." whatsapp-bridge/main.go
```

Find the switch inside `client.AddEventHandler(...)`.

**Step 2:** Add two new cases (after `*events.ClientOutdated`):

```go
case *events.JoinedGroup:
    // Full GroupInfo snapshot when joining a group
    go func() {
        defer func() {
            if r := recover(); r != nil {
                slog.Error("persistGroupInfoSnapshot panic",
                    "err", r, "group", v.JID.String())
            }
        }()
        if err := messageStore.UpsertGroup(&v.GroupInfo); err != nil {
            slog.Warn("upsert group from JoinedGroup",
                "err", err, "group", v.JID.String())
            return
        }
        if err := messageStore.ReplaceGroupParticipants(
            v.JID.String(), v.GroupInfo.Participants,
        ); err != nil {
            slog.Warn("replace participants from JoinedGroup",
                "err", err, "group", v.JID.String())
            return
        }
        slog.Info("group snapshot persisted",
            "group", v.JID.String(),
            "participants", len(v.GroupInfo.Participants))
    }()

case *events.GroupInfo:
    // Incremental update (Join/Leave/Promote/Demote + Name/Topic)
    go func() {
        defer func() {
            if r := recover(); r != nil {
                slog.Error("applyGroupInfoDelta panic",
                    "err", r, "group", v.JID.String())
            }
        }()
        if err := messageStore.ApplyGroupDelta(v); err != nil {
            slog.Warn("apply group delta",
                "err", err, "group", v.JID.String())
            return
        }
        slog.Info("group delta applied",
            "group", v.JID.String(),
            "join", len(v.Join), "leave", len(v.Leave),
            "promote", len(v.Promote), "demote", len(v.Demote))
    }()
```

**Step 3:** Ensure `events` package import includes the new types — verify `go.mau.fi/whatsmeow/types/events` is already in the import block.

### Task 5.2: Build + commit Phase 5

```bash
cd whatsapp-bridge && go build ./... && \
cd .. && git add whatsapp-bridge/main.go && \
git commit -m "feat(bridge): add JoinedGroup and GroupInfo event handlers

Both handlers run as fire-and-forget goroutines with recover() guards,
matching the existing webhook-dispatcher pattern (main.go:836-839).
Per D11: panics are logged but do not crash the bridge process."
```

---

## Phase 6: GroupCache + Background Ticker

### Task 6.1: Add GroupCache type

**Files:**
- Modify: `whatsapp-bridge/groups.go`

**Step 1:** Append to `groups.go`:

```go
// GroupCache wraps client.GetGroupInfo calls with singleflight deduplication
// and a strict 5s timeout context. Multiple concurrent callers for the same
// JID share a single GetGroupInfo call.
type GroupCache struct {
	store      *MessageStore
	client     *whatsmeow.Client
	logger     *slog.Logger
	inflight   singleflight.Group
	maxPerTick int
	staleAfter time.Duration
}

// NewGroupCache constructs a GroupCache. All fields are immutable after construction.
func NewGroupCache(store *MessageStore, client *whatsmeow.Client, logger *slog.Logger) *GroupCache {
	return &GroupCache{
		store:      store,
		client:     client,
		logger:     logger,
		maxPerTick: 5,
		staleAfter: 12 * time.Hour,
	}
}

// RefreshGroup fetches fresh GroupInfo from WhatsApp and persists it.
// Concurrent calls for the same JID share a single GetGroupInfo call
// via singleflight (per D6).
func (c *GroupCache) RefreshGroup(jid string) error {
	_, err, _ := c.inflight.Do(jid, func() (interface{}, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		parsed, err := types.ParseJID(jid)
		if err != nil {
			return nil, fmt.Errorf("parse jid %s: %w", jid, err)
		}
		if parsed.Server != "g.us" {
			return nil, fmt.Errorf("not a group jid: %s", jid)
		}

		info, err := c.client.GetGroupInfo(ctx, parsed)
		if err != nil {
			return nil, fmt.Errorf("getGroupInfo %s: %w", jid, err)
		}

		if err := c.store.UpsertGroup(info); err != nil {
			return nil, fmt.Errorf("upsert: %w", err)
		}
		if err := c.store.ReplaceGroupParticipants(jid, info.Participants); err != nil {
			return nil, fmt.Errorf("replace participants: %w", err)
		}

		c.logger.Info("group refreshed",
			"group", jid,
			"participants", len(info.Participants))
		return nil, nil
	})
	return err
}

// RefreshStaleGroups is called by the background ticker every 30 minutes.
// Selects up to maxPerTick groups whose last_synced is older than staleAfter
// (or NULL) and refreshes them concurrently.
func (c *GroupCache) RefreshStaleGroups() {
	cutoff := time.Now().Add(-c.staleAfter)
	groups, err := c.store.ListStaleGroups(cutoff, c.maxPerTick)
	if err != nil {
		c.logger.Warn("list stale groups", "err", err)
		return
	}
	if len(groups) == 0 {
		return
	}
	c.logger.Info("refreshing stale groups", "count", len(groups))
	for _, g := range groups {
		// Fire-and-forget; RefreshGroup is singleflight-deduped.
		go func(jid string) {
			if err := c.RefreshGroup(jid); err != nil {
				c.logger.Debug("group refresh failed", "err", err, "group", jid)
			}
		}(g.JID)
	}
}

// StartGroupRefreshTicker launches a background goroutine that calls
// RefreshStaleGroups every `interval`. Runs until process termination.
func StartGroupRefreshTicker(cache *GroupCache, interval time.Duration) {
	ticker := time.NewTicker(interval)
	go func() {
		for range ticker.C {
			cache.RefreshStaleGroups()
		}
	}()
}
```

### Task 6.2: Test singleflight behavior

**Files:**
- Create: `whatsapp-bridge/groups_cache_test.go`

```go
package main

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sync/singleflight"
)

func TestSingleflightDedup(t *testing.T) {
	// Direct test of singleflight.Group behavior to verify our assumption.
	var sf singleflight.Group
	var calls int32

	concurrent := 10
	done := make(chan struct{})
	for i := 0; i < concurrent; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			_, _, _ = sf.Do("key", func() (interface{}, error) {
				atomic.AddInt32(&calls, 1)
				time.Sleep(50 * time.Millisecond)
				return nil, nil
			})
		}()
	}
	for i := 0; i < concurrent; i++ {
		<-done
	}
	require.Equal(t, int32(1), atomic.LoadInt32(&calls),
		"singleflight must collapse concurrent calls into 1 execution")
}
```

Run:

```bash
go test -run TestSingleflightDedup -v
```

Expected: PASS.

### Task 6.3: Wire GroupCache into main()

**Files:**
- Modify: `whatsapp-bridge/main.go`

**Step 1:** Locate where `startRESTServer` is called (around line 1530).

**Step 2:** Before `startRESTServer`, add:

```go
// Construct group cache and start background refresh ticker
groupCache := NewGroupCache(messageStore, client, slog.Default())
StartGroupRefreshTicker(groupCache, 30*time.Minute)
```

**Step 3:** Pass `groupCache` to `startRESTServer` (modify the signature to accept it; alternatively use a package-level variable, but signature change is cleaner).

If you change `startRESTServer` signature, update all callers. There should only be one.

### Task 6.4: Build + commit Phase 6

```bash
go build ./... && \
cd .. && git add whatsapp-bridge/ && \
git commit -m "feat(bridge): add GroupCache with singleflight + background refresh ticker

- GroupCache wraps GetGroupInfo with 5s context timeout
- Singleflight collapses concurrent RefreshGroup calls for same JID (D6)
- Background ticker: 30min interval, 12h stale cutoff, max 5 groups/tick (D7)
- Tests: TestSingleflightDedup verifies dedup assumption"
```

---

## Phase 7: REST Endpoints

### Task 7.1: Add `registerGroupHandlers`

**Files:**
- Modify: `whatsapp-bridge/groups.go`

**Step 1:** Append the REST handler registration:

```go
// RegisterGroupHandlers adds the /groups/ and /groups/{jid}/participants routes.
func RegisterGroupHandlers(apiMux *http.ServeMux, cache *GroupCache, store *MessageStore) {
	apiMux.HandleFunc("/groups/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/groups/")
		parts := strings.SplitN(path, "/", 2)
		if len(parts) == 0 || parts[0] == "" {
			respondError(w, http.StatusBadRequest, "group jid required")
			return
		}
		groupJID, err := types.ParseJID(parts[0])
		if err != nil || groupJID.Server != "g.us" {
			respondError(w, http.StatusBadRequest, "invalid group jid")
			return
		}
		refresh := r.URL.Query().Has("refresh")

		// Subresource: /groups/{jid}/participants
		if len(parts) == 2 && parts[1] == "participants" {
			if refresh {
				if err := cache.RefreshGroup(parts[0]); err != nil {
					slog.Warn("refresh group", "err", err, "group", parts[0])
					// Continue anyway: return whatever's in DB
				}
			}
			ps, err := store.ListGroupParticipants(parts[0])
			if err != nil {
				respondError(w, http.StatusInternalServerError, err.Error())
				return
			}
			respondJSON(w, http.StatusOK, map[string]interface{}{
				"participants": ps,
				"count":        len(ps),
			})
			return
		}

		// Full group info
		if refresh {
			if err := cache.RefreshGroup(parts[0]); err != nil {
				slog.Warn("refresh group", "err", err, "group", parts[0])
			}
		}
		g, err := store.GetGroup(parts[0])
		if err != nil {
			respondError(w, http.StatusNotFound,
				"group not synced yet; call with ?refresh=true")
			return
		}
		respondJSON(w, http.StatusOK, g)
	})
}
```

### Task 7.2: Call `RegisterGroupHandlers` from `startRESTServer`

**Files:**
- Modify: `whatsapp-bridge/main.go`

**Step 1:** Locate the `apiMux.HandleFunc` calls inside `startRESTServer` (search for `apiMux.HandleFunc("/chats/",`).

**Step 2:** Add a call to `RegisterGroupHandlers(apiMux, groupCache, messageStore)` near the other HandleFunc calls. The `groupCache` parameter must be threaded through to `startRESTServer`.

### Task 7.3: Build + smoke-test the endpoint manually

```bash
go build ./...
# In a separate terminal, get a JWT and curl the endpoint:
TOKEN=$(curl -s -H "Authorization: Bearer $(grep WHATSAPP_API_KEY .env | cut -d= -f2)" \
  http://localhost:8080/auth/login | jq -r .token)
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/groups/120363045908244249@g.us" | jq
```

Expected: JSON with `jid`, `subject`, `description`, etc. (or 404 if group not yet synced — then add `?refresh=true`).

### Task 7.4: Commit Phase 7

```bash
git add whatsapp-bridge/
git commit -m "feat(bridge): add GET /api/groups/ and /participants endpoints

- /api/groups/{jid} returns group metadata (subject, description, owner, ...)
- /api/groups/{jid}/participants returns participant array with admin flags
- ?refresh=true forces a fresh GetGroupInfo via singleflight-deduped cache
- 404 if group not yet synced (hint to use ?refresh=true)"
```

---

## Phase 8: GetChat Extension

### Task 8.1: Extend `Chat` struct

**Files:**
- Modify: `whatsapp-bridge/main.go`

**Step 1:** Find the `type Chat struct` definition (around line 61).

**Step 2:** Append new fields:

```go
type Chat struct {
	// ... existing fields stay unchanged ...

	// NEW: group-only fields (omitted from JSON for direct chats via omitempty)
	Subject      string              `json:"subject,omitempty"`
	Description  string              `json:"description,omitempty"`
	SubjectOwner string              `json:"subject_owner,omitempty"`
	SubjectTime  time.Time           `json:"subject_time,omitempty"`
	CreationTime time.Time           `json:"creation_time,omitempty"`
	Participants []StoredParticipant `json:"participants,omitempty"`
}
```

### Task 8.2: Update GetChat handler

**Files:**
- Modify: `whatsapp-bridge/main.go` (around line 2573 — the `/chats/` handler)

**Step 1:** Add `include_participants` query param check at the start of the handler:

```go
includeParticipants := r.URL.Query().Has("include_participants")
```

**Step 2:** After the existing Single-Row-Query that fills `chat`, add (only for group chats):

```go
if strings.HasSuffix(jid, "@g.us") {
    g, err := store.GetGroup(jid)
    if err == nil {
        chat.Subject = g.Subject
        chat.Description = g.Description
        chat.SubjectOwner = g.SubjectOwner
        chat.SubjectTime = g.SubjectTime
        chat.CreationTime = g.CreationTime
    }
    if includeParticipants {
        ps, _ := store.ListGroupParticipants(jid)
        chat.Participants = ps
    }
}
```

### Task 8.3: Build + commit Phase 8

```bash
go build ./... && \
git add whatsapp-bridge/main.go && \
git commit -m "feat(bridge): extend GetChat response with group metadata

For @g.us chats, the response now includes (when available):
- subject, subject_owner, subject_time
- description
- creation_time
- participants[] (only when ?include_participants=true, D8)"
```

---

## Phase 9: MCP Server — New Tools

### Task 9.1: Add `list_group_participants` tool

**Files:**
- Modify: `whatsapp-mcp-server/helpers/mcp_tool.go`

**Step 1:** Add the input struct (near other input structs, e.g., after `listChatsInput`):

```go
type listGroupParticipantsInput struct {
	GroupJid string `json:"group_jid" jsonschema:"description:The group JID ending in @g.us,required"`
	Refresh  bool   `json:"refresh" jsonschema:"default:false,description:Force a fresh fetch from WhatsApp before returning (slower, single-flight deduplicated)"`
}
```

**Step 2:** Register the tool (in the `RegisterTools` or wherever `mcp.AddTool` calls live):

```go
mcp.AddTool[listGroupParticipantsInput, any](server, &mcp.Tool{
	Name:        "list_group_participants",
	Description: "List participants of a WhatsApp group with admin flags. Returns JID, phone number (null if privacy-hidden), admin/super-admin status, and display name. Set refresh=true to force a fresh fetch from WhatsApp.",
}, listGroupParticipantsHandler)
```

**Step 3:** Implement the handler:

```go
func listGroupParticipantsHandler(ctx context.Context, req *mcp.CallToolRequest, in listGroupParticipantsInput) (*mcp.CallToolResult, any, error) {
	if in.GroupJid == "" || !strings.HasSuffix(in.GroupJid, "@g.us") {
		return ErrResult("group_jid must be a group JID ending in @g.us"), nil, nil
	}
	path := fmt.Sprintf("/groups/%s/participants?refresh=%v",
		url.PathEscape(in.GroupJid), in.Refresh)
	data, err := callAPI(http.MethodGet, path, nil)
	if err != nil {
		return ErrResult(err.Error()), nil, nil
	}
	var result struct {
		Participants []map[string]any `json:"participants"`
		Count        int              `json:"count"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return ErrResult("decode response: " + err.Error()), nil, nil
	}
	return OkResult(result), nil, nil
}
```

### Task 9.2: Add `get_group_info` tool

Analogous to 9.1 — same pattern. Add struct, register tool, implement handler:

```go
type getGroupInfoInput struct {
	GroupJid string `json:"group_jid" jsonschema:"required"`
	Refresh  bool   `json:"refresh" jsonschema:"default:false"`
}

mcp.AddTool[getGroupInfoInput, any](server, &mcp.Tool{
	Name:        "get_group_info",
	Description: "Get metadata for a WhatsApp group: subject (name), description (topic), owner, creation time, participant count, last sync time. Use refresh=true to force-fetch from WhatsApp.",
}, getGroupInfoHandler)

func getGroupInfoHandler(ctx context.Context, req *mcp.CallToolRequest, in getGroupInfoInput) (*mcp.CallToolResult, any, error) {
	if in.GroupJid == "" || !strings.HasSuffix(in.GroupJid, "@g.us") {
		return ErrResult("group_jid must be a group JID ending in @g.us"), nil, nil
	}
	path := fmt.Sprintf("/groups/%s?refresh=%v",
		url.PathEscape(in.GroupJid), in.Refresh)
	data, err := callAPI(http.MethodGet, path, nil)
	if err != nil {
		return ErrResult(err.Error()), nil, nil
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return ErrResult("decode response: " + err.Error()), nil, nil
	}
	return OkResult(result), nil, nil
}
```

### Task 9.3: Extend `get_chat` tool with `include_participants`

**Files:**
- Modify: `whatsapp-mcp-server/helpers/mcp_tool.go`

Find existing `getChatInput` struct and add:

```go
IncludeParticipants bool `json:"include_participants" jsonschema:"default:false,description:For group chats, include the full participant list"`
```

Update `getChatHandler` to append `&include_participants=...` to the query string.

### Task 9.4: Extend Chat struct in MCP server

**Files:**
- Modify: `whatsapp-mcp-server/helpers/whatsapp.go`

Add the new fields to the `Chat` struct (mirror of Phase 8.1 but with `[]map[string]any` since the MCP server doesn't import bridge types):

```go
type Chat struct {
	// ... existing fields ...

	Subject      string           `json:"subject,omitempty"`
	Description  string           `json:"description,omitempty"`
	SubjectOwner string           `json:"subject_owner,omitempty"`
	SubjectTime  time.Time        `json:"subject_time,omitempty"`
	CreationTime time.Time        `json:"creation_time,omitempty"`
	Participants []map[string]any `json:"participants,omitempty"`
}
```

### Task 9.5: Build + commit Phase 9

```bash
cd whatsapp-mcp-server && go build ./... && \
cd .. && git add whatsapp-mcp-server/ && \
git commit -m "feat(mcp): add list_group_participants + get_group_info tools

- list_group_participants: returns array with jid, phone_number (null for privacy),
  is_admin, is_super_admin, display_name
- get_group_info: returns subject, description, owner, creation time, last sync
- get_chat extended with include_participants flag (default false, D8)
- Chat struct mirrored in whatsapp.go with same fields"
```

---

## Phase 10: MCP Handler Tests

### Task 10.1: Write httptest-based MCP handler tests

**Files:**
- Create: `whatsapp-mcp-server/helpers/mcp_tool_test.go`

```go
package helpers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// mockBridge returns a test server that pretends to be the WhatsApp bridge.
// Set WHATSAPP_API_KEY to a test value via t.Setenv if needed by api_auth.
func mockBridge(t *testing.T, status int, response string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(response))
	}))
}

func TestListGroupParticipantsHandler_Validation(t *testing.T) {
	// Direct validation test: empty jid should return ErrResult without HTTP call.
	in := listGroupParticipantsInput{GroupJid: ""}
	result, _, _ := listGroupParticipantsHandler(nil, nil, in)
	require.Contains(t, result.Content[0].TextContent.Text, "group_jid must be a group JID")
}

func TestListGroupParticipantsHandler_Validation_NotGroupJid(t *testing.T) {
	in := listGroupParticipantsInput{GroupJid: "4915126206106@s.whatsapp.net"}
	result, _, _ := listGroupParticipantsHandler(nil, nil, in)
	require.Contains(t, result.Content[0].TextContent.Text, "group_jid must be a group JID ending in @g.us")
}

// Note: full success-path tests would require mocking apiBaseURL + JWT.
// Those are easier to validate via the smoke-test doc (manual end-to-end).
```

### Task 10.2: Build + commit Phase 10

```bash
cd whatsapp-mcp-server && go test ./... -v && \
cd .. && git add whatsapp-mcp-server/helpers/mcp_tool_test.go && \
git commit -m "test(mcp): add handler tests for list_group_participants validation

Validates that empty JID and non-@g.us JIDs are rejected before any
HTTP call is made to the bridge. Full success-path covered by smoke test."
```

---

## Phase 11: Smoke Test Documentation

### Task 11.1: Write smoke test doc

**Files:**
- Create: `docs/smoke-test-group-feature.md`

**Step 1:** Create the file (mirror of Spec section 9.2 — copy verbatim):

```markdown
# Smoke Test: Group Participants & Description Feature

## Voraussetzungen
- Docker läuft, Bridge ist gepaart
- Mindestens eine bekannte @g.us-Gruppe verfügbar (z.B. "Post SV D2 Junioren")

## Schritte

### 1. Fresh Reset (Datenverlust akzeptiert per D2)
\`\`\`bash
docker compose down
docker volume rm whatsapp-mcp-go_pgdata whatsapp-mcp-go_bridge-store
docker compose up --build -d
# QR aus \`docker compose logs wa-bridge\` scannen
docker compose logs -f wa-bridge | grep -i "qr\|history sync"
\`\`\`

### 2. Warten auf History-Sync
\`\`\`bash
docker compose logs -f wa-bridge | grep "history sync complete"
\`\`\`

### 3. DB inspizieren
\`\`\`bash
docker compose exec postgres psql -U whatsapp -d whatsapp \
  -c "SELECT count(*) FROM groups; SELECT count(*) FROM group_participants;"
\`\`\`
Erwartet: Beide Counts > 0.

### 4. Bridge REST direkt testen
\`\`\`bash
TOKEN=$(curl -s -H "Authorization: Bearer $(grep WHATSAPP_API_KEY .env | cut -d= -f2)" \
  http://localhost:8080/auth/login | jq -r .token)

# Group metadata
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/groups/120363045908244249@g.us?refresh=true" | jq

# Participants
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/groups/120363045908244249@g.us/participants" | jq
\`\`\`

### 5. MCP-Tools aus opencode
\`\`\`
list_group_participants(group_jid="120363045908244249@g.us")
get_group_info(group_jid="120363045908244249@g.us", refresh=true)
get_chat(chat_jid="120363045908244249@g.us", include_participants=true)
get_chat(chat_jid="120363045908244249@g.us", include_participants=false)
\`\`\`

### 6. Bug-Fix verifizieren
\`\`\`bash
docker compose exec postgres psql -U whatsapp -d whatsapp \
  -c "SELECT count(*) FROM messages WHERE chat_jid LIKE '%@g.us' AND sender = '';"
\`\`\`
Erwartet: > 0 (Nachrichten ohne Participant-Info haben jetzt leeren Sender statt Gruppen-JID).

### 7. Background-Ticker verifizieren
\`\`\`bash
# Warte 30min ODER ändere das Intervall im Code temporär auf 1min zum Testen.
docker compose logs wa-bridge 2>&1 | grep "group refreshed" | tail -5
\`\`\`

### 8. Concurrency verifizieren (singleflight)
\`\`\`bash
# Mehrere parallele Refresh-Aufrufe → Bridge sollte nur EIN GetGroupInfo pro Gruppe ausführen.
for i in 1 2 3 4 5; do
  curl -s -H "Authorization: Bearer $TOKEN" \
    "http://localhost:8080/api/groups/120363045908244249@g.us/participants?refresh=true" &
done
wait
docker compose logs wa-bridge --since 30s 2>&1 | grep -c "group refreshed"
# Erwartet: 1 (nicht 5)
\`\`\`
```

**Step 2:** Commit

```bash
git add docs/smoke-test-group-feature.md
git commit -m "docs: add smoke-test-group-feature.md for manual verification

8-step checklist: Fresh Reset, History-Sync, DB inspection, REST endpoints,
MCP tools, bug-fix verification, background ticker, singleflight concurrency."
```

---

## Phase 12: Final Verification

### Task 12.1: Run all tests for both modules

```bash
cd whatsapp-bridge && go test ./... -v 2>&1 | tail -30
cd ../whatsapp-mcp-server && go test ./... -v 2>&1 | tail -30
```

Expected: All tests PASS for both modules.

### Task 12.2: Verify build artifacts

```bash
cd /home/gun/development/ai/mcp/whatsapp-mcp-go
docker compose build wa-bridge wa-mcp
```

Expected: Both images build successfully.

### Task 12.3: Fresh reset + deploy

```bash
docker compose down
docker volume rm whatsapp-mcp-go_pgdata whatsapp-mcp-go_bridge-store
docker compose up -d
docker compose logs -f wa-bridge | grep -i "qr\|history sync\|group"
```

Pair via QR code in logs, then wait for `history sync complete` and `group snapshot persisted` messages.

### Task 12.4: Verify acceptance criteria from spec

Run through the 12 acceptance criteria in `.sisyphus/plans/group-participants-design.md` section 14.

### Task 12.5: Final commit (if any uncommitted work remains)

```bash
git status
# If anything's left, commit it.
```

---

## Acceptance Criteria (mirror of Spec §14)

- [ ] `go build ./...` and `go test ./...` pass for both modules
- [ ] Fresh Reset performed at least once, no panics during History-Sync
- [ ] `SELECT count(*) FROM groups` > 0 after History-Sync
- [ ] `SELECT count(*) FROM group_participants` > 0
- [ ] `curl /api/groups/{jid}?refresh=true` returns Subject + Description
- [ ] `curl /api/groups/{jid}/participants?refresh=true` returns array
- [ ] MCP tool `list_group_participants` returns same data as REST
- [ ] MCP tool `get_group_info` returns Subject, Description, Owner, Creation-Time
- [ ] `get_chat(jid="@g.us", include_participants=true)` includes Participants
- [ ] `get_chat(jid="@g.us", include_participants=false)` does NOT include Participants
- [ ] Background ticker logs "group refreshed" every 30min
- [ ] Smoke test doc committed
- [ ] NO commits on `main` or upstream repo

---

## Communication Checkpoints

After each Phase, post this update template in the opencode session:

```
📡 Phase [N] Complete — [Title]
Commits: [list of commit hashes]
Verified: [test results, build status]
Next: Phase [N+1] — [Title]
Blockers: [or "None"]
```

If a phase fails (red tests, build break), STOP and ask the user before proceeding to the next.

---

## Execution Approach

This plan has 12 phases. Recommended execution: **`superpowers:subagent-driven-development`** — each phase dispatched to a fresh subagent with this plan as context. Spec-compliance review + code-quality review after each phase.

Alternative: **`superpowers:executing-plans`** — sequential, single-session execution.

**Push Policy:** All commits stay local. User reviews before any `git push` or PR.

---

**Plan-Version:** 1.0
**Spec Reference:** `.sisyphus/plans/group-participants-design.md`
**Branch:** `feat/group-participants-and-description`
