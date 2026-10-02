package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// newContactsTestStore opens a throwaway sqlite MessageStore seeded with
// direct + group chats (the /contacts directory aggregates both). The
// store resolves its sqlite path from the CWD ("store/..."), so the test
// chdir's into a TempDir.
func newContactsTestStore(t *testing.T) *MessageStore {
	t.Helper()
	t.Chdir(t.TempDir())
	store, err := NewMessageStore()
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	base := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	require.NoError(t, store.StoreChat("49123456789@s.whatsapp.net", "Gunnar", base))
	require.NoError(t, store.StoreChat("49876543210@s.whatsapp.net", "Anette", base.Add(-time.Hour)))
	require.NoError(t, store.StoreChat("1203630218@g.us", "Familie", base.Add(-2*time.Hour)))
	require.NoError(t, store.StoreChat("1203999999@g.us", "Projekt", base.Add(-3*time.Hour)))
	return store
}

// Test_contacts_endpoint_envelope_pins_shapes pins GET /api/contacts:
// items carry the eventlab W5.18 contract (id/label/kind d m|group + meta).
func Test_contacts_endpoint_envelope_pins_shapes(t *testing.T) {
	ms := newContactsTestStore(t)
	body := contactsDirectoryJSON(t, ms)

	var env struct {
		Items []struct {
			ID    string         `json:"id"`
			Label string         `json:"label"`
			Kind  string         `json:"kind"`
			Meta  map[string]any `json:"meta"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(body, &env))
	require.Len(t, env.Items, 4)

	byID := map[string]map[string]any{}
	for _, it := range env.Items {
		byID[it.ID] = map[string]any{"label": it.Label, "kind": it.Kind, "meta": it.Meta}
	}

	g, ok := byID["1203630218@g.us"]
	require.True(t, ok)
	require.Equal(t, "group", g["kind"])
	require.Equal(t, "Familie", g["label"])

	d, ok := byID["49123456789@s.whatsapp.net"]
	require.True(t, ok)
	require.Equal(t, "dm", d["kind"])
	require.Equal(t, "Gunnar", d["label"])
}

// Test_contacts_endpoint_dm_only_filter pins ?kind=dm|group filtering.
func Test_contacts_endpoint_dm_only_filter(t *testing.T) {
	ms := newContactsTestStore(t)

	groups := contactsDirectoryBody(t, ms, "kind=group")
	dm := contactsDirectoryBody(t, ms, "kind=dm")
	var gEnv, dEnv struct {
		Items []struct {
			Kind string `json:"kind"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(groups, &gEnv))
	require.NoError(t, json.Unmarshal(dm, &dEnv))
	require.Len(t, gEnv.Items, 2)
	require.Len(t, dEnv.Items, 2)
	for _, it := range gEnv.Items {
		require.Equal(t, "group", it.Kind)
	}
	for _, it := range dEnv.Items {
		require.Equal(t, "dm", it.Kind)
	}
}

// Test_contacts_endpoint_method_guard pins non-GET rejection.
func Test_contacts_endpoint_method_guard(t *testing.T) {
	ms := newContactsTestStore(t)
	req := httptest.NewRequest(http.MethodPost, "/contacts", nil)
	rec := httptest.NewRecorder()
	handleContactsEndpoint(ms, rec, req)
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// Test_contacts_endpoint_empty_store pins the no-history case: empty items,
// HTTP 200 (a freshly paired bridge has no chats yet — not an error).
func Test_contacts_endpoint_empty_store(t *testing.T) {
	t.Chdir(t.TempDir())
	store, err := NewMessageStore()
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	req := httptest.NewRequest(http.MethodGet, "/contacts", nil)
	rec := httptest.NewRecorder()
	handleContactsEndpoint(store, rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"items":[]}`, rec.Body.String())
}

// helper: serve the endpoint via the mux shape used in main (recording).
func contactsDirectoryJSON(t *testing.T, ms *MessageStore) []byte {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/contacts", nil)
	rec := httptest.NewRecorder()
	handleContactsEndpoint(ms, rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	return rec.Body.Bytes()
}

func contactsDirectoryBody(t *testing.T, ms *MessageStore, query string) []byte {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/contacts?"+query, nil)
	rec := httptest.NewRecorder()
	handleContactsEndpoint(ms, rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	return rec.Body.Bytes()
}

// silence unused import when the test run prunes os usage in refactors.
