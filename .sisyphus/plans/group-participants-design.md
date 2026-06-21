# Design Spec: Group Participants & Description Feature

**Status:** Draft (awaiting user review)
**Date:** 2026-06-21
**Author:** Gunnar Thielebein (via opencode build agent)
**Branch:** `feat/group-participants-and-description`
**Supersedes:** –
**Depends on:** –

---

## 1. Problem Statement

Der aktuelle `whatsapp-mcp-go`-Bridge hat zwei strukturelle Schwächen im Umgang mit WhatsApp-Gruppen (`@g.us`-JIDs):

1. **History-Sync-Bug** (`whatsapp-bridge/main.go:1687-1706`): Wenn WhatsMeow in der initialen History-Sync das `Key.Participant`-Feld nicht liefert, fällt der Sender-Fallback auf die Gruppen-JID (`jid.User`) zurück. Dadurch stehen in der Datenbank für viele Gruppen-Nachrichten die Gruppen-ID als Sender, und `GetSenderName` (`main.go:1910`) liefert den Gruppennamen statt des tatsächlichen Verfassers.

2. **Fehlende Gruppen-Metadaten:** Die Bridge ruft zwar `client.GetGroupInfo` bereits auf (in `GetChatName`, main.go:1583), verwertet davon aber ausschließlich `Name`. Topic (Beschreibung), Participants, OwnerJID, GroupCreated etc. werden ignoriert. Es existiert keine Tabelle für Gruppen-Metadaten und kein MCP-Tool, um Teilnehmerlisten oder Beschreibungen abzurufen.

**Konsequenz für den User:** Aus opencode/MCP-Sicht ist auf Gruppen-Nachrichten nicht nachvollziehbar, wer sie geschrieben hat, und es gibt keinen Weg, die Teilnehmerliste oder Beschreibung einer Gruppe zu lesen.

---

## 2. Goals / Non-Goals

### Goals
- [G1] Persistierung von Gruppen-Metadaten (`subject`, `description`, `owner_jid`, `creation_time`, `participant_version_id`, `last_synced`) in Postgres und SQLite.
- [G2] Persistierung der Teilnehmerliste (`group_participants`) mit Admin-Flags und Display-Name.
- [G3] Bug-Fix: Gruppen-Nachrichten ohne Participant-Info werden als "unknown sender" markiert statt mit der Gruppen-JID.
- [G4] `GetSenderName` fällt auf `group_participants` zurück, wenn in `chats` kein Eintrag gefunden wird.
- [G5] Event-Handler für `*events.JoinedGroup` und `*events.GroupInfo` (inkrementelle Updates via Join/Leave/Promote/Demote).
- [G6] Background-Refresh für stale Gruppen-Metadaten (30min Ticker, 12h Schwelle, max 5/Tick).
- [G7] REST-Endpoints `GET /api/groups/{jid}` und `GET /api/groups/{jid}/participants`.
- [G8] `GetChat`-Response wird für Gruppen optional um Metadaten + Participants erweitert (default `include_participants=false`).
- [G9] Zwei neue MCP-Tools: `list_group_participants`, `get_group_info`.
- [G10] Unit-Tests mit `go-sqlmock` + `testify` für Store-Layer und Handler-Logik.
- [G11] Smoke-Test-Dokumentation für manuelle End-to-End-Verifikation.

### Non-Goals
- [NG1] Kein Refactor des bestehenden 2892-Zeilen-Monolithen `main.go`. Neue Logik wird in neuem File `whatsapp-bridge/groups.go` (`package main`) untergebracht.
- [NG2] Kein Migrations-Framework (golembic, goose, sql-migrate etc.). Schema bleibt bei `CREATE TABLE IF NOT EXISTS`-Idiom.
- [NG3] Kein PR an upstream (`iamatulsingh/whatsapp-mcp-go`) vor ausgiebigem Test durch den User.
- [NG4] Keine Änderung am whatsmeow-internen `sqlstore`-Schema (`whatsmeow_*`-Tabellen).
- [NG5] Kein neues HTTP-Framework (bleibt bei `net/http.ServeMux`).
- [NG6] Keine Auth-Middleware-Änderungen (JWT-Flow bleibt unverändert).
- [NG7] Kein Graceful-HTTP-Shutdown (separates Thema).
- [NG8] Keine Group-Picture- oder Group-Avatar-Unterstützung (YAGNI).

---

## 3. User Decisions (from Brainstorming)

| # | Frage | Entscheidung |
|---|---|---|
| D1 | Scope | Alle 6 Punkte + Gruppenbeschreibung |
| D2 | DB-Migration | Fresh Reset (Datenverlust akzeptiert) |
| D3 | DB-Backend | Postgres **und** SQLite |
| D4 | Branch-Strategie | `feat/group-participants-and-description`, lokal, kein PR vor Test |
| D5 | Test-Aufwand | Unit-Tests + Smoke-Test |
| D6 | singleflight-Abhängigkeit | Ja, `golang.org/x/sync/singleflight` |
| D7 | Background-Ticker-Intervall | 30min Ticker, 12h stale-Schwelle, max 5 Gruppen/Tick |
| D8 | `get_chat` Participants-Default | `include_participants=false` |
| D9 | Code-Organisation | Neues File `whatsapp-bridge/groups.go` in `package main` |

---

## 4. Architecture Overview

### 4.1 Bestandspflege — keine Architektur-Revolution

Das Feature orientiert sich **voll** an der vorhandenen Go-Architektur:

| Aspekt | Bestand | Feature übernimmt |
|---|---|---|
| HTTP | `net/http.ServeMux` (main.go:1148) | ✓ unverändert |
| Logging | `log/slog` JSON-Handler (logger/logger.go) | ✓ unverändert |
| Config | `config.LoadConfig()` (config/config.go) | ✓ unverändert |
| Auth | JWT + Rate-Limit (auth/login.go) | ✓ unverändert |
| Mutex-Pattern | `sync.RWMutex` (wastate/wastate.go:9) | ✓ neues `groupCache` analog |
| Background-Job | `time.NewTicker(6*time.Hour)` (main.go:2831) | ✓ neuer Ticker analog |
| Fire-and-forget | Webhook-Dispatcher `go func()` (main.go:830) | ✓ Event-Handler analog |
| Context | 1× `context.WithTimeout(2s)` (main.go:239) | ✓ `GetGroupInfo` immer mit 5s Timeout |
| Dual-DB | `isPostgres` + `placeholder(n)` Helper | ✓ unverändert |

### 4.2 Neue Komponenten

```
whatsapp-bridge/
├── main.go                            (2892 Zeilen, unverändert + 2 kleine Edits)
├── groups.go                          (NEU, ~400 Zeilen, package main)
│   ├── type GroupCache                (sync.RWMutex + sync.Map + singleflight)
│   ├── type GroupStore                (Methoden an *MessageStore)
│   ├── type StoredGroup               (DB-Row-Mapping)
│   ├── type StoredParticipant         (DB-Row-Mapping)
│   ├── persistGroupInfoSnapshot()     (aus *events.JoinedGroup)
│   ├── applyGroupInfoDelta()          (aus *events.GroupInfo)
│   ├── refreshGroupInBackground()     (singleflight + context.WithTimeout)
│   ├── startGroupRefreshTicker()      (30min, scannt stale Gruppen)
│   └── registerGroupHandlers()        (REST-Endpoints /api/groups/, /api/groups/.../participants)
├── groups_store_test.go               (NEU, sqlmock + testify)
├── groups_cache_test.go               (NEU)
└── go.mod                             (+ golang.org/x/sync, + go-sqlmock, + testify als direct dep)

whatsapp-mcp-server/
├── helpers/mcp_tool.go                (+ list_group_participants, + get_group_info)
├── helpers/whatsapp.go                (Chat-Struct erweitert für Gruppen-Meta)
└── helpers/mcp_tool_test.go           (NEU, httptest.MockBridge)

.sisyphus/plans/
├── group-participants-design.md       (dieses Dokument)
└── group-participants-implementation.md (folgt via writing-plans)

docs/
└── smoke-test-group-feature.md        (NEU, manuelle Verifikations-Checkliste)
```

### 4.3 Komponenten-Diagramm (Datenfluss)

```
┌──────────────────────────────────────────────────────────────────────────────┐
│ WHATSAPP SERVER (WA)                                                          │
└─────────────────────────────┬────────────────────────────────────────────────┘
                              │
                              │ events.JoinedGroup / events.GroupInfo
                              │ events.HistorySync / events.Message
                              ▼
┌──────────────────────────────────────────────────────────────────────────────┐
│ WHATSAPP-BRIDGE (Go, package main)                                            │
│                                                                               │
│  ┌─────────────────────────────┐    ┌─────────────────────────────────────┐  │
│  │ Event-Dispatcher (whatsmeow) │    │ REST-API (net/http.ServeMux)        │  │
│  │  - *events.Message           │    │  /api/groups/{jid}                  │  │
│  │  - *events.HistorySync       │    │  /api/groups/{jid}/participants     │  │
│  │  - *events.JoinedGroup [NEW] │    │  /api/chats/{jid} (extended)        │  │
│  │  - *events.GroupInfo [NEW]   │    └─────────────────────────────────────┘  │
│  └──────────┬──────────────────┘                  ▲                          │
│             │                                      │                          │
│             ▼                                      │                          │
│  ┌─────────────────────────────┐    ┌─────────────────────────────────────┐  │
│  │ GroupCache [NEW]             │    │ GroupStore [NEW] (an *MessageStore) │  │
│  │  - sync.RWMutex              │◄──►│  UpsertGroup                        │  │
│  │  - sync.Map (in-flight)      │    │  ReplaceGroupParticipants (Tx)      │  │
│  │  - singleflight.Group        │    │  ApplyGroupDelta                    │  │
│  │  - GetGroupInfo (5s ctx)     │    │  GetGroup / ListParticipants        │  │
│  └──────────┬──────────────────┘    │  LookupParticipantName              │  │
│             │                       └─────────────────────────────────────┘  │
│             ▼                                      ▲                          │
│  ┌─────────────────────────────────────────────────┴──────────────────────┐  │
│  │ Background-Ticker (30min, scannt groups WHERE last_synced < 12h)       │  │
│  │  - singleflight dedup                                                  │  │
│  │  - max 5 Gruppen pro Tick                                              │  │
│  │  - 5s context.WithTimeout pro GetGroupInfo                             │  │
│  └────────────────────────────────────────────────────────────────────────┘  │
│                                                                               │
│  Datenbank (Postgres ODER SQLite):                                            │
│   - chats (bestehend)                                                         │
│   - messages (bestehend, mit Bug-Fix)                                         │
│   - groups [NEU]                                                              │
│   - group_participants [NEU]                                                  │
└─────────────────────────────┬────────────────────────────────────────────────┘
                              │ HTTP + JWT (bestehend)
                              ▼
┌──────────────────────────────────────────────────────────────────────────────┐
│ WHATSAPP-MCP-SERVER (Go)                                                      │
│   Tools:                                                                      │
│    - list_chats (bestehend)                                                   │
│    - get_chat (extended: + subject, + description, + participants[])          │
│    - list_messages (bestehend)                                                │
│    - list_group_participants [NEU]                                            │
│    - get_group_info [NEU]                                                     │
│    - ... (weitere bestehende)                                                 │
└─────────────────────────────┬────────────────────────────────────────────────┘
                              │ MCP via HTTP (streamable-http, Port 5777)
                              ▼
┌──────────────────────────────────────────────────────────────────────────────┐
│ OPENCODE / MCP-CLIENT                                                         │
└──────────────────────────────────────────────────────────────────────────────┘
```

---

## 5. Data Model

### 5.1 Schema-Erweiterung

Idempotent per `CREATE TABLE IF NOT EXISTS`, ergänzt in `NewMessageStore()` (main.go:189-213) als zweiter `db.Exec`-Block.

```sql
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
```

**Bemerkungen:**
- Alle Spalten sind Standard-SQL-Typen, kein `BYTEA`/`BLOB`-Shim nötig (anders als `messages.media_key`).
- `description` entspricht dem WhatsApp-Topic (`types.GroupTopic.Topic`), nicht zu verwechseln mit `subject` (`types.GroupName.Name`).
- Foreign Key Cascade: Wenn eine Gruppe gelöscht wird, verschwinden ihre Participants automatisch.
- SQLite unterstützt `ON DELETE CASCADE` nur mit `PRAGMA foreign_keys=ON` — der Connection-String `file:store/messages.db?_foreign_keys=on` (main.go:128) aktiviert dies bereits.

### 5.2 Go-Types (in `groups.go`)

```go
type StoredGroup struct {
    JID                  string
    Subject              string
    SubjectOwner         string
    SubjectTime          time.Time
    Description          string
    DescriptionID        string
    DescriptionOwner     string
    DescriptionTime      time.Time
    OwnerJID             string
    CreationTime         time.Time
    ParticipantVersionID string
    LastSynced           time.Time
}

type StoredParticipant struct {
    GroupJID     string
    JID          string
    PhoneNumber  string
    LID          string
    IsAdmin      bool
    IsSuperAdmin bool
    DisplayName  string
}
```

---

## 6. Detailed Design — Bridge

### 6.1 Bug-Fix: History-Sync Sender-Fallback

**Datei:** `whatsapp-bridge/main.go`
**Zeilen:** 1687-1706

**Vorher:**
```go
if !isFromMe && msg.Message.Key.Participant != nil && *msg.Message.Key.Participant != "" {
    // ... korrekter Participant-Pfad
} else if isFromMe {
    sender = client.Store.ID.User
} else {
    sender = jid.User  // BUG: bei @g.us = Gruppen-ID
}
```

**Nachher:**
```go
if !isFromMe && msg.Message.Key.Participant != nil && *msg.Message.Key.Participant != "" {
    // ... korrekter Participant-Pfad (unverändert)
} else if isFromMe {
    sender = client.Store.ID.User
} else if jid.Server == "g.us" {
    // NEU: leer statt Gruppen-JID
    // GetSenderName fällt auf group_participants-Lookup zurück,
    // oder rendert als "unknown" wenn Participant noch nicht in DB.
    sender = ""
    logger.Debugf("history-sync: group msg without participant, group=%s msg=%s", jid.String(), msgID)
} else {
    sender = jid.User
}
```

**Kompatibilität:** Bestehende Direkt-Chat-Nachrichten (Server `s.whatsapp.net`) sind nicht betroffen — der `else`-Fall greift dort weiterhin korrekt.

### 6.2 `GetSenderName`-Erweiterung

**Datei:** `whatsapp-bridge/main.go:1910`

Der bestehende Lookup in `chats` schlägt für Gruppen-Sender fehl (dort steht die Gruppen-JID, nicht die des Verfassers). Die Erweiterung fügt einen Fallback auf `group_participants.display_name` hinzu:

```go
func (store *MessageStore) GetSenderName(senderJID string) string {
    // ... bestehende chats-Lookups (unverändert) ...

    // NEU: Fallback auf group_participants wenn chats-Lookup leer
    if isPostgres {
        err = store.db.QueryRow(
            `SELECT display_name FROM group_participants
             WHERE jid = $1 AND display_name != ''
             LIMIT 1`, senderJID,
        ).Scan(&name)
    } else {
        err = store.db.QueryRow(
            `SELECT display_name FROM group_participants
             WHERE jid = ? AND display_name != ''
             LIMIT 1`, senderJID,
        ).Scan(&name)
    }
    if err == nil && name != "" {
        return name
    }

    // Final fallback: Telefonnummer oder "unknown"
    if senderJID == "" {
        return "unknown"
    }
    return senderJID
}
```

### 6.3 Event-Handler-Erweiterung

**Datei:** `whatsapp-bridge/main.go:2766-2820`

Zwei neue `case`-Zweige im zentralen `AddEventHandler`-Switch:

```go
case *events.JoinedGroup:
    // v.GroupInfo ist embedded → full snapshot
    go func() {
        defer func() {
            if r := recover(); r != nil {
                slog.Error("persistGroupInfoSnapshot panic", "err", r, "group", v.JID.String())
            }
        }()
        if err := messageStore.UpsertGroup(&v.GroupInfo); err != nil {
            slog.Warn("upsert group from JoinedGroup", "err", err, "group", v.JID.String())
            return
        }
        if err := messageStore.ReplaceGroupParticipants(v.JID.String(), v.GroupInfo.Participants); err != nil {
            slog.Warn("replace participants from JoinedGroup", "err", err, "group", v.JID.String())
        }
        slog.Info("group snapshot persisted", "group", v.JID.String(), "participants", len(v.GroupInfo.Participants))
    }()

case *events.GroupInfo:
    // Inkrementell: Join/Leave/Promote/Demote + Name/Topic-Änderung
    go func() {
        defer func() {
            if r := recover(); r != nil {
                slog.Error("applyGroupInfoDelta panic", "err", r, "group", v.JID.String())
            }
        }()
        if err := messageStore.ApplyGroupDelta(v); err != nil {
            slog.Warn("apply group delta", "err", err, "group", v.JID.String())
            return
        }
        slog.Info("group delta applied", "group", v.JID.String(),
            "join", len(v.Join), "leave", len(v.Leave),
            "promote", len(v.Promote), "demote", len(v.Demote))
    }()
```

**Warum `go func` fire-and-forget?** Folgt dem Webhook-Dispatch-Pattern (main.go:830). Event-Loop darf nicht auf DB-IO blocken. `recover()`-Guard wie bei Webhook (main.go:836-839).

### 6.4 Background-Refresh-Ticker

**Datei:** `whatsapp-bridge/groups.go`

```go
type GroupCache struct {
    mu         sync.RWMutex
    inflight   singleflight.Group
    store      *MessageStore
    client     *whatsmeow.Client
    logger     *slog.Logger
    maxPerTick int
    staleAfter time.Duration
}

func startGroupRefreshTicker(cache *GroupCache, interval, staleAfter time.Duration) {
    ticker := time.NewTicker(interval)
    go func() {
        for range ticker.C {
            cache.refreshStaleGroups()
        }
    }()
}

func (c *GroupCache) refreshStaleGroups() {
    cutoff := time.Now().Add(-c.staleAfter)
    groups, err := c.store.ListStaleGroups(cutoff, c.maxPerTick)
    if err != nil {
        c.logger.Warn("list stale groups", "err", err)
        return
    }
    for _, g := range groups {
        // singleflight dedup: parallel ticks oder Event-Handler + Tick
        go c.refreshGroup(g.JID)
    }
}

func (c *GroupCache) refreshGroup(jid string) {
    _, err, _ := c.inflight.Do(jid, func() (interface{}, error) {
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()
        parsed, _ := types.ParseJID(jid)
        info, err := c.client.GetGroupInfo(ctx, parsed)
        if err != nil {
            c.logger.Warn("getGroupInfo", "err", err, "group", jid)
            return nil, err
        }
        if err := c.store.UpsertGroup(info); err != nil {
            return nil, err
        }
        if err := c.store.ReplaceGroupParticipants(jid, info.Participants); err != nil {
            return nil, err
        }
        c.logger.Info("group refreshed", "group", jid, "participants", len(info.Participants))
        return nil, nil
    })
    if err != nil {
        c.logger.Debug("group refresh failed", "err", err, "group", jid)
    }
}
```

**Konfiguration (hardcoded defaults, kein Env-Var nötig):**
- `interval = 30 * time.Minute`
- `staleAfter = 12 * time.Hour`
- `maxPerTick = 5`
- `GetGroupInfo timeout = 5 * time.Second`

### 6.5 REST-Endpoints

**Datei:** `whatsapp-bridge/groups.go` (Funktion `registerGroupHandlers(apiMux, cache, store)`; `client` nicht nötig, hängt am `cache`)

```go
func registerGroupHandlers(apiMux *http.ServeMux, cache *GroupCache, store *MessageStore) {
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

        // Subresource /participants
        if len(parts) == 2 && parts[1] == "participants" {
            if refresh {
                cache.refreshGroup(parts[0])
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
            cache.refreshGroup(parts[0])
        }
        g, err := store.GetGroup(parts[0])
        if err != nil {
            respondError(w, http.StatusNotFound, "group not synced yet; call with ?refresh=true")
            return
        }
        respondJSON(w, http.StatusOK, g)
    })
}
```

**Verhalten:**
- `GET /api/groups/{jid}` → `StoredGroup` als JSON
- `GET /api/groups/{jid}/participants` → `{participants: [...], count: N}`
- `?refresh=true` → triggert `cache.refreshGroup` synchron (singleflight-deduped)
- 404 wenn Gruppe noch nie synchronisiert wurde (Hinweis im Body)

### 6.6 `GetChat`-Response erweitern

**Datei:** `whatsapp-bridge/main.go:2573` (`GetChat`)

Für `@g.us`-JIDs wird die Antwort um optionale Felder erweitert. Standardmäßig OHNE Participants (Performance).

```go
// Query-Param-Check am Anfang des Handlers:
includeParticipants := r.URL.Query().Has("include_participants")

// Bestehende Single-Row-Query bleibt unverändert.
// Für Gruppen: zusätzliche Queries, wenn include_participants=true:
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

**Chat-Struct-Erweiterung** (`main.go:61-72`):

```go
type Chat struct {
    // ... bestehende Felder ...

    // NEU: Gruppen-only (leer für Direkt-Chats)
    Subject      string              `json:"subject,omitempty"`
    Description  string              `json:"description,omitempty"`
    SubjectOwner string              `json:"subject_owner,omitempty"`
    SubjectTime  time.Time           `json:"subject_time,omitempty"`
    CreationTime time.Time           `json:"creation_time,omitempty"`
    Participants []StoredParticipant `json:"participants,omitempty"`
}
```

**MCP-Server** (`whatsapp-mcp-server/helpers/whatsapp.go`) muss die `Chat`-Struct parallel erweitern, damit die Felder durchgereicht werden.

### 6.7 `StoreMessage`-Aufruf in History-Sync

**Datei:** `whatsapp-bridge/main.go:1720-1734`

Aufruf bleibt unverändert. Der leere `sender` (`""`) ist für `StoreMessage` kein Problem (Spalte ist TEXT ohne NOT-NULL-Constraint). `GetSenderName("")` rendert als `"unknown"`.

### 6.8 Live-Nachrichten-Path

**Datei:** `whatsapp-bridge/main.go:879`

Live-Pfad (`sender := normalizeUserJID(client, msg.Info.Sender).User`) ist **bereits korrekt** für Gruppen — WhatsMeow liefert bei echten Message-Events immer den tatsächlichen Sender. Keine Änderung nötig.

---

## 7. Detailed Design — MCP-Server

### 7.1 Neues Tool: `list_group_participants`

**Datei:** `whatsapp-mcp-server/helpers/mcp_tool.go`

```go
type listGroupParticipantsInput struct {
    GroupJid string `json:"group_jid" jsonschema:"description:The group JID ending in @g.us,required"`
    Refresh  bool   `json:"refresh" jsonschema:"default:false,description:Force refresh from WhatsApp before returning"`
}

mcp.AddTool[listGroupParticipantsInput, any](server, &mcp.Tool{
    Name:        "list_group_participants",
    Description: "List participants of a WhatsApp group with admin flags. Returns JID, phone number, admin/super-admin status, and display name (if available). Set refresh=true to force a fresh fetch from WhatsApp (slower, single-flight deduplicated).",
}, listGroupParticipantsHandler)

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

### 7.2 Neues Tool: `get_group_info`

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

### 7.3 `get_chat`-Tool erweitern

**Datei:** `whatsapp-mcp-server/helpers/mcp_tool.go`

Bestehendes `getChatInput` um ein optionales Feld ergänzen:

```go
type getChatInput struct {
    ChatJid            string `json:"chat_jid" jsonschema:"required"`
    IncludeLastMessage bool   `json:"include_last_message" jsonschema:"default:true"`
    IncludeParticipants bool  `json:"include_participants" jsonschema:"default:false,description:For group chats, include the full participant list"`
}
```

Handler ergänzt `&include_participants=...` an den Query-String.

### 7.4 Chat-Struct im MCP-Server

**Datei:** `whatsapp-mcp-server/helpers/whatsapp.go` — parallele Struct-Erweiterung wie in 6.6, identische Felder:

```go
type Chat struct {
    // ... bestehende Felder (jid, name, last_message_time, ...) ...

    // NEU (parallel zu bridge main.go):
    Subject      string              `json:"subject,omitempty"`
    Description  string              `json:"description,omitempty"`
    SubjectOwner string              `json:"subject_owner,omitempty"`
    SubjectTime  time.Time           `json:"subject_time,omitempty"`
    CreationTime time.Time           `json:"creation_time,omitempty"`
    Participants []map[string]any    `json:"participants,omitempty"`
}
```

Die `Participants` werden im MCP-Server als `[]map[string]any` deklariert (statt stark typisiert als `StoredParticipant`), weil der MCP-Server die Bridge-Structs nicht importiert und die JSON-Deserialisation ohnehin lose bleibt. Tag-Namen stimmen mit der Bridge-Response überein.

---

## 8. Concurrency-Safety-Analyse

| Ressource | Zugriffe | Schutz |
|---|---|---|
| `MessageStore.db` | Multi-Goroutine (Event-Handler, Ticker, REST) | `*sql.DB` ist thread-safe per spec |
| `GroupCache.inflight` | Multi-Goroutine | `singleflight.Group` (intern synchronisiert) |
| `GroupCache` Konfiguration (`store`, `client`, `logger`, `maxPerTick`, `staleAfter`) | Set-once nach Init, danach nur-Read | Kein Mutex nötig (immutable nach Konstruktion) |
| Background-Ticker | 1 Goroutine (lifecycle) | Ticker-Channel inhärent synchronisiert |
| `*whatsmeow.Client` | Multi-Goroutine | whatsmeow-internal `groupCacheLock` + thread-safe Client |
| REST-Handler | 1 Goroutine pro Request | `net/http`-Server-Modell |

**Potenzielle Race-Conditions:**
1. **Event-Handler vs. Ticker:** Beide rufen `refreshGroup(jid)` → `singleflight` deduped. ✓ safe.
2. **REST ?refresh=true vs. Ticker:** Gleich — `singleflight` ✓ safe.
3. **ReplaceGroupParticipants (Tx) vs. ApplyGroupDelta (UPDATE):** Beide operieren auf derselben Gruppe. `singleflight` nicht betroffen (delta ist nicht full-refresh). Lösung: kurze `BEGIN; ... ; COMMIT;`-Tx um beide Operationen, mit `SELECT ... FOR UPDATE` (Postgres) bzw. `BEGIN IMMEDIATE` (SQLite). **Impl:** `ApplyGroupDelta` und `ReplaceGroupParticipants` verwenden beide `db.BeginTx` mit Serializable-Isolation (Postgres) bzw. leben damit, dass SQLite per Default seriell ist.

**Goroutine-Lifecycle:**
- Ticker-Goroutine: läuft bis SIGTERM, kein expliciter Stop (akzeptiert, da Process-Ende).
- Event-Handler-Goroutinen: fire-and-forget, leben kurz (<1s pro GroupInfo-Delta).
- `recover()`-Guards überall wie im bestehenden Webhook-Pattern.

---

## 9. Testing Strategy

### 9.1 Unit-Tests (Go testing)

**File:** `whatsapp-bridge/groups_store_test.go`

Coverage:
- `TestUpsertGroup_Postgres` / `TestUpsertGroup_SQLite` — Insert + Update-Pfad
- `TestReplaceGroupParticipants_InsertUpdateDelete` — Tx-Wrapping, Cascade
- `TestApplyGroupDelta_JoinLeavePromoteDemote` — inkrementelle Updates
- `TestGetGroup_NotFound` — 404-Verhalten
- `TestListGroupParticipants` — Sortierung, Filterung
- `TestLookupParticipantName_Fallback` — Hauptfall für Bug-Fix
- `TestListStaleGroups_LimitAndCutoff` — Ticker-Selektionslogik

**File:** `whatsapp-bridge/groups_cache_test.go`

- `TestRefreshGroup_SingleflightDedup` — parallele Aufrufe → 1× GetGroupInfo
- `TestRefreshGroup_GetGroupInfoError` — Graceful degradation
- `TestRefreshGroup_ContextTimeout` — 5s Timeout wird respektiert

**File:** `whatsapp-mcp-server/helpers/mcp_tool_test.go`

- `TestListGroupParticipantsHandler_Validation` — leere JID, kein @g.us
- `TestListGroupParticipantsHandler_Success` — mit httptest.Server als Bridge-Mock
- `TestListGroupParticipantsHandler_BridgeError` — 5xx vom Backend

**Mocking:**
- `go-sqlmock` für Store-Tests (ersetzt `*sql.DB`).
- `httptest.Server` für MCP-Handler-Tests (ersetzt Bridge-REST).
- WhatsMeow-Client nicht gemockt — GroupCache wird so gebaut, dass `client` nil sein darf, wenn `refreshGroup` nicht aufgerufen wird (Tests umgehen die Methode).

### 9.2 Smoke-Test (manuell, dokumentiert)

**File:** `docs/smoke-test-group-feature.md`

```markdown
# Smoke Test: Group Participants Feature

## Voraussetzungen
- Docker läuft, Bridge ist gepaart
- Mindestens eine bekannte @g.us-Gruppe verfügbar (z.B. Test-Gruppe oder "Post SV D2 Junioren")

## Schritte

1. **Fresh Reset** (optional, wenn DB migriert werden muss):
   docker compose down
   docker volume rm whatsapp-mcp-go_pgdata whatsapp-mcp-go_bridge-store
   docker compose up --build
   # QR aus `docker compose logs wa-bridge` scannen

2. **Warten auf History-Sync**:
   docker compose logs -f wa-bridge | grep "history sync complete"

3. **DB inspizieren**:
   docker compose exec postgres psql -U whatsapp -d whatsapp \
     -c "SELECT count(*) FROM groups; SELECT count(*) FROM group_participants;"

4. **Bridge REST direkt testen**:
   TOKEN=$(curl -s -H "Authorization: Bearer $WHATSAPP_API_KEY" \
     http://localhost:8080/auth/login | jq -r .token)
   curl -s -H "Authorization: Bearer $TOKEN" \
     http://localhost:8080/api/groups/120363045908244249@g.us | jq
   curl -s -H "Authorization: Bearer $TOKEN" \
     "http://localhost:8080/api/groups/120363045908244249@g.us/participants?refresh=true" | jq

5. **MCP-Tool aus opencode**:
   list_group_participants(group_jid="120363045908244249@g.us")
   get_group_info(group_jid="120363045908244249@g.us", refresh=true)
   get_chat(chat_jid="120363045908244249@g.us", include_participants=true)

6. **Bug-Fix verifizieren**:
   # In der History müssen Gruppen-Nachrichten mit fehlendem Participant nun "unknown"
   # als Sender haben statt der Gruppen-JID.
   docker compose exec postgres psql -U whatsapp -d whatsapp \
     -c "SELECT count(*) FROM messages WHERE chat_jid LIKE '%@g.us' AND sender = '';"
   # Erwartet: > 0 für die "unknown"-Messages
```

---

## 10. Migration & Deployment

### 10.1 Fresh-Reset-Prozedur

Vom User ausdrücklich gewünscht (D2):

```bash
cd /home/gun/development/ai/mcp/whatsapp-mcp-go
docker compose down
docker volume rm whatsapp-mcp-go_pgdata whatsapp-mcp-go_bridge-store
docker compose up --build -d
# In den Logs den QR-Code finden und mit WhatsApp-App pairen
docker compose logs -f wa-bridge | grep -i "qr\|history sync"
# Warten bis "history sync complete" erscheint (kann mehrere Minuten dauern)
```

### 10.2 Schema-Erstellung

Erfolgt automatisch beim Bridge-Start via `CREATE TABLE IF NOT EXISTS` in `NewMessageStore()` — kein manuelles `psql` nötig.

### 10.3 Build

```bash
cd whatsapp-bridge && go build ./...
cd whatsapp-mcp-server && go build ./...
```

CGO nicht erforderlich, da reine Postgres-Tests; SQLite-Codepfad existiert, wird aber nicht compiliert in Docker (`CGO_ENABLED=0`).

### 10.4 Docker-Image-Rebuild

```bash
docker compose build wa-bridge wa-mcp
docker compose up -d
```

---

## 11. Branch & Commit-Strategie

**Branch:** `feat/group-participants-and-description` (lokal, kein PR vor User-Test)

**Commit-Häppchen** (jeweils `go build && go test ./...` grün):

1. `feat(bridge): add groups + group_participants schema and types`
2. `feat(bridge): add GroupStore CRUD methods with sqlmock tests`
3. `fix(bridge): correct sender in history-sync for @g.us chats without participant`
4. `fix(bridge): extend GetSenderName to fall back on group_participants`
5. `feat(bridge): add JoinedGroup and GroupInfo event handlers`
6. `feat(bridge): add GroupCache with singleflight + context timeouts`
7. `feat(bridge): add background group-refresh ticker (30m/12h)`
8. `feat(bridge): add GET /api/groups/ and /participants endpoints`
9. `feat(bridge): extend GetChat response with group metadata`
10. `feat(mcp): add list_group_participants tool`
11. `feat(mcp): add get_group_info tool`
12. `feat(mcp): extend get_chat with include_participants flag`
13. `test(mcp): add handler tests with httptest mock bridge`
14. `docs: add smoke-test-group-feature.md`
15. `chore(deps): add singleflight, go-sqlmock, testify as direct deps`

**Push-Strategie:** Lokale Commits only. Optional `git push origin feat/group-participants-and-description` zum eigenen Fork, wenn der User Backup will — **nicht** zu upstream.

---

## 12. Risks & Mitigations

| # | Risiko | Wahrscheinlichkeit | Auswirkung | Mitigation |
|---|---|---|---|---|
| R1 | `client.GetGroupInfo` rate-limit bei vielen Gruppen | Medium | WA blockt Client | 5/Tick-Limit + 30min-Intervall + `singleflight` |
| R2 | SQLite-Connection hat `foreign_keys=on` nicht aktiv | Low | Kein Cascade-Delete | Verify in bestehendem Conn-String (main.go:128) |
| R3 | `Key.Participant == nil` für die MEISTEN History-Sync-Nachrichten | High | Sehr viele "unknown"-Sender | Erwartet & akzeptiert; künftige Live-Nachrichten werden korrekt sein |
| R4 | whatsmeow `types.GroupInfo`-Struktur ändert sich | Low | Compile-Break | Vendor-Pinning in go.sum; Test schlägt fehl → erkennbar |
| R5 | `singleflight` deadlocked bei Context-Cancel | Very Low | Ticker hängt | `singleflight.Do` propagiert Error, nächste Runde läuft wieder |
| R6 | Postgres-Serialisable-Isolation zu restriktiv | Low | Performance | Default-Read-Committed reicht; nur `ReplaceGroupParticipants` braucht strikte Tx |
| R7 | Beschreibung (`GroupTopic.Topic`) ist leer bei vielen Gruppen | High | `description=""` in Response | OK, `omitempty` im JSON |
| R8 | `singleflight`-Abhängigkeit lässt sich nicht hinzufügen | Very Low | Build-Break | Bereits indirect in go.sum, wird zu direct |

---

## 13. Open Questions (für User-Review)

1. **Schema-Migration Pfad**: Akzeptiert du, dass bei einem späteren Upgrade (ohne Fresh Reset) die alten `messages`-Zeilen mit Gruppen-JID als Sender nicht automatisch korrigiert werden? (Sie bleiben als Gruppen-JID stehen, nur neue Messages haben korrekte Sender.)
2. **Event-Handler-Panics**: Sollten diese den Bridge-Prozess killen (currently: nein, nur logged)? Vorschlag: bleiben geloggt, kein Process-Kill.
3. **`IncludeParticipants` bei `get_chat`**: Performance-Test bei einer Gruppe mit 100+ Teilnehmern nicht gemacht. Falls es zu langsam ist, nachträglich `LIMIT` einführen?
4. **Teilnehmer-Phone-Number-Auflösung**: `GroupParticipant.PhoneNumber` ist bei einigen Gruppen leer (Privacy). Akzeptiert du, dass dann `phone_number=""` in der Response steht?

---

## 14. Acceptance Criteria

Das Feature gilt als " fertig zum User-Test", wenn alle erfüllt sind:

- [ ] `go build ./...` und `go test ./...` sind grün für beide Module.
- [ ] Fresh Reset wurde mindestens 1× durchgespielt, Logs zeigen keine Panics während History-Sync.
- [ ] `SELECT count(*) FROM groups` liefert > 0 nach History-Sync.
- [ ] `SELECT count(*) FROM group_participants` liefert > 0.
- [ ] `curl /api/groups/{jid}?refresh=true` liefert Subject + Description (für eine Testgruppe).
- [ ] `curl /api/groups/{jid}/participants?refresh=true` liefert Teilnehmer-Array.
- [ ] MCP-Tool `list_group_participants` aus opencode liefert dieselben Daten.
- [ ] MCP-Tool `get_group_info` liefert Subject, Description, Owner, Creation-Time.
- [ ] `get_chat(jid="@g.us", include_participants=true)` enthält Participants-Array.
- [ ] `get_chat(jid="@g.us", include_participants=false)` enthält KEINE Participants (Performance-Default).
- [ ] Background-Ticker loggt "group refreshed" alle 30min für stale Gruppen.
- [ ] Smoke-Test-Doku `docs/smoke-test-group-feature.md` ist committed.
- [ ] Kein einziger Commit auf `main` oder upstream-Repo.

---

## 15. Next Steps

1. **User reviewt dieses Spec** → Feedback / Approval
2. **Bei Approval:** Übergang zur `writing-plans`-Skill → konkreter Implementation-Plan mit Code-Snippets, Test-Code, Schritt-für-Schritt-Anweisungen pro Wave
3. **Bei Approval des Plans:** Wave-für-Wave-Implementierung mit `subagent-driven-development` (parallele Wellen möglich für 1, 4, 6) oder strikt sequentiell via `executing-plans`
4. **Nach jeder Wave:** `go test` grün, Commit auf Feature-Branch, Update an User in dieser Session
5. **Nach Wave 7:** User übernimmt zum ausgiebigen Test
6. **Nach positivem User-Test (Wochen später):** optional PR-Vorbereitung an upstream

---

**Spec-Version:** 1.0
**Letzte Aktualisierung:** 2026-06-21
