package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// fixture is a minimal SPA tree.
func fixture() fstest.MapFS {
	return fstest.MapFS{
		"index.html":           {Data: []byte(`<!doctype html><div id="app"></div>`)},
		"assets/app-abc123.js": {Data: []byte("console.log(1)")},
		"manifest.webmanifest": {Data: []byte("{}")},
	}
}

func do(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestServesIndexAtRoot(t *testing.T) {
	h := Handler(Options{FS: fixture()})
	rec := do(t, h, http.MethodGet, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `id="app"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("index Cache-Control = %q, want no-cache", got)
	}
}

func TestSpaFallbackForClientRoutes(t *testing.T) {
	h := Handler(Options{FS: fixture()})
	// These are the SPA routes from UX-SPEC §3.
	for _, path := range []string{"/r/K7M2QX", "/r/K7M2QX/", "/settings/profile"} {
		rec := do(t, h, http.MethodGet, path)
		if rec.Code != http.StatusOK {
			t.Errorf("%s status = %d, want 200", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `id="app"`) {
			t.Errorf("%s did not return index.html", path)
		}
	}
}

func TestAssetsGetImmutableCache(t *testing.T) {
	h := Handler(Options{FS: fixture()})
	rec := do(t, h, http.MethodGet, "/assets/app-abc123.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Fatalf("asset Cache-Control = %q", got)
	}
}

func TestMissingAssetIs404NotHTML(t *testing.T) {
	h := Handler(Options{FS: fixture()})
	rec := do(t, h, http.MethodGet, "/assets/missing-xyz.js")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "<!doctype html>") {
		t.Fatal("missing asset was answered with index.html")
	}
}

// TestAPIPathsNeverFallBack is docs/PROTOCOL.md §1.6: unknown /api and /ws paths
// must be JSON 404, never the SPA.
func TestAPIPathsNeverFallBack(t *testing.T) {
	h := Handler(Options{FS: fixture()})
	for _, path := range []string{
		"/api", "/api/rooms", "/api/rooms/K7M2QX", "/api/nope",
		"/ws", "/ws/", "/metrics", "/ops", "/ops/data", "/healthz", "/readyz",
	} {
		rec := do(t, h, http.MethodGet, path)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), `id="app"`) {
			t.Errorf("%s received the SPA fallback", path)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("%s content-type = %q, want JSON", path, ct)
		}
	}
}

func TestOnlyGETAndHEAD(t *testing.T) {
	h := Handler(Options{FS: fixture()})
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := do(t, h, method, "/r/K7M2QX")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s status = %d, want 405", method, rec.Code)
		}
		if rec.Header().Get("Allow") == "" {
			t.Errorf("%s response is missing Allow", method)
		}
	}
	rec := do(t, h, http.MethodHead, "/r/K7M2QX")
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("HEAD status = %d body = %d bytes", rec.Code, rec.Body.Len())
	}
}

func TestMissingIndexRendersPlaceholder(t *testing.T) {
	// The Phase A state: a backend-only checkout with no dist.
	h := Handler(Options{FS: fstest.MapFS{}, DevHint: "cd web && npm ci && npm run build"})
	rec := do(t, h, http.MethodGet, "/")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "前端产物未找到") {
		t.Errorf("placeholder body = %s", body)
	}
	if !strings.Contains(body, "npm run build") {
		t.Error("placeholder does not show the build hint")
	}
	for _, link := range []string{"/healthz", "/readyz", "/metrics", "/ops"} {
		if !strings.Contains(body, link) {
			t.Errorf("placeholder does not link to %s", link)
		}
	}
}

func TestEmbeddedDistDoesNotPanic(t *testing.T) {
	// The committed dist only has .gitkeep: Handler must build and answer.
	h := Handler(Options{})
	rec := do(t, h, http.MethodGet, "/healthz")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	// "/" is either the placeholder or a real index depending on whether someone
	// built the frontend locally; both are fine, a panic is not.
	_ = do(t, h, http.MethodGet, "/")
}

func TestPathTraversalIsRejected(t *testing.T) {
	h := Handler(Options{FS: fixture()})
	for _, path := range []string{
		"/../index.html",
		"/assets/../../etc/passwd",
	} {
		rec := do(t, h, http.MethodGet, path)
		if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "root:") {
			t.Fatalf("%s served a file outside the embedded FS", path)
		}
	}
}

func TestDirectoryRequestDoesNotList(t *testing.T) {
	h := Handler(Options{FS: fixture()})
	rec := do(t, h, http.MethodGet, "/assets/")
	if strings.Contains(rec.Body.String(), "app-abc123.js") {
		t.Fatal("directory request leaked a file listing")
	}
}
