package api

import (
	"net/http"
	"testing"

	"proxback/internal/store"
)

// TestSettingsDefaultsAndThroughputRoundTrip covers the settings contract the web
// UI is built against: the throughput fields are always present, default on a
// database that has never stored them, and survive a partial PUT.
func TestSettingsDefaultsAndThroughputRoundTrip(t *testing.T) {
	ts := newTestServer(t)

	code, body := ts.request(t, http.MethodGet, "/api/settings", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/settings = %d", code)
	}
	for field, want := range map[string]any{
		"uploadConcurrency": float64(store.DefaultUploadConcurrency),
		"compression":       store.DefaultCompression,
		"uploadLimitMbps":   float64(store.DefaultUploadLimitMbps),
	} {
		if body[field] != want {
			t.Fatalf("default %s = %v, want %v", field, body[field], want)
		}
	}

	code, body = ts.request(t, http.MethodPut, "/api/settings", map[string]any{
		"uploadConcurrency": 8,
		"compression":       "off",
		"uploadLimitMbps":   500,
	})
	if code != http.StatusOK {
		t.Fatalf("PUT /api/settings = %d (%v)", code, body)
	}
	if body["uploadConcurrency"] != float64(8) || body["compression"] != "off" || body["uploadLimitMbps"] != float64(500) {
		t.Fatalf("PUT response = %v", body)
	}
	// Untouched fields keep their values, and the change is persisted.
	if body["serverName"] != store.DefaultServerName || body["concurrency"] != float64(store.DefaultConcurrency) {
		t.Fatalf("a partial PUT disturbed the other settings: %v", body)
	}
	_, body = ts.request(t, http.MethodGet, "/api/settings", nil)
	if body["uploadConcurrency"] != float64(8) || body["compression"] != "off" || body["uploadLimitMbps"] != float64(500) {
		t.Fatalf("settings did not persist: %v", body)
	}

	// The boundaries themselves are accepted.
	for _, ok := range []map[string]any{
		{"uploadConcurrency": store.MinUploadConcurrency},
		{"uploadConcurrency": store.MaxUploadConcurrency},
		{"uploadLimitMbps": store.MinUploadLimitMbps},
		{"uploadLimitMbps": store.MaxUploadLimitMbps},
		{"compression": store.CompressionZstd},
		{"compression": store.CompressionOff},
	} {
		if code, body := ts.request(t, http.MethodPut, "/api/settings", ok); code != http.StatusOK {
			t.Fatalf("PUT %v = %d (%v), want 200", ok, code, body)
		}
	}
}

// TestSettingsValidationRejectsBadThroughputValues keeps a mistyped setting from
// reaching the engine, with a 400 the UI can show.
func TestSettingsValidationRejectsBadThroughputValues(t *testing.T) {
	ts := newTestServer(t)

	for _, bad := range []map[string]any{
		{"uploadConcurrency": 0},
		{"uploadConcurrency": -1},
		{"uploadConcurrency": 17},
		{"uploadLimitMbps": -1},
		{"uploadLimitMbps": 10001},
		{"compression": "gzip"},
		{"compression": ""},
		{"compression": "ZSTD"},
	} {
		code, body := ts.request(t, http.MethodPut, "/api/settings", bad)
		if code != http.StatusBadRequest {
			t.Fatalf("PUT %v = %d (%v), want 400", bad, code, body)
		}
		if body["error"] == nil || body["error"] == "" {
			t.Fatalf("PUT %v returned 400 without an error message: %v", bad, body)
		}
	}

	// Nothing was persisted by the rejected requests.
	_, body := ts.request(t, http.MethodGet, "/api/settings", nil)
	if body["uploadConcurrency"] != float64(store.DefaultUploadConcurrency) ||
		body["compression"] != store.DefaultCompression ||
		body["uploadLimitMbps"] != float64(store.DefaultUploadLimitMbps) {
		t.Fatalf("a rejected PUT changed the stored settings: %v", body)
	}
}
