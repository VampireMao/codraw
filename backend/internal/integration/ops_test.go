package integration

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/codraw/codraw/backend/internal/config"
)

// TestOpsTokenHandling covers the /ops and /ops/data credential rules:
//
//   - only enforced when METRICS_TOKEN is set
//   - accepted as a bearer header or a ?token= query parameter (a browser
//     navigation cannot set a header)
//   - the token is never echoed into the rendered page
//   - the access log masks it
func TestOpsTokenHandling(t *testing.T) {
	const token = "s3cret-ops-token-value"

	t.Run("no token configured means open", func(t *testing.T) {
		s := newStack(t, nil)
		for _, path := range []string{"/ops", "/ops/data"} {
			res, err := http.Get(s.baseURL + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			_ = res.Body.Close()
			if res.StatusCode != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200 when no token is configured", path, res.StatusCode)
			}
		}
	})

	t.Run("token configured rejects unauthenticated access", func(t *testing.T) {
		s := newStack(t, func(c *config.Config) { c.Metrics.Token = token })
		for _, path := range []string{"/ops", "/ops/data"} {
			res, err := http.Get(s.baseURL + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			_ = res.Body.Close()
			if res.StatusCode != http.StatusUnauthorized {
				t.Fatalf("GET %s without a token = %d, want 401", path, res.StatusCode)
			}
		}
	})

	t.Run("query token is accepted", func(t *testing.T) {
		s := newStack(t, func(c *config.Config) { c.Metrics.Token = token })
		for _, path := range []string{"/ops?token=" + token, "/ops/data?token=" + token} {
			res, err := http.Get(s.baseURL + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			_ = res.Body.Close()
			if res.StatusCode != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200", path, res.StatusCode)
			}
		}
	})

	t.Run("bearer token is accepted", func(t *testing.T) {
		s := newStack(t, func(c *config.Config) { c.Metrics.Token = token })
		req, _ := http.NewRequest(http.MethodGet, s.baseURL+"/ops/data", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /ops/data: %v", err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("bearer /ops/data = %d, want 200", res.StatusCode)
		}
	})

	t.Run("wrong token is rejected", func(t *testing.T) {
		s := newStack(t, func(c *config.Config) { c.Metrics.Token = token })
		res, err := http.Get(s.baseURL + "/ops?token=wrong")
		if err != nil {
			t.Fatalf("GET /ops: %v", err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("wrong token = %d, want 401", res.StatusCode)
		}
	})

	t.Run("token is never rendered into the page", func(t *testing.T) {
		s := newStack(t, func(c *config.Config) { c.Metrics.Token = token })
		res, err := http.Get(s.baseURL + "/ops?token=" + token)
		if err != nil {
			t.Fatalf("GET /ops: %v", err)
		}
		body, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if strings.Contains(string(body), token) {
			t.Fatal("the /ops HTML echoed the token; it must be read from the URL by the client only")
		}
		// The panel must still know how to authenticate its own polling request.
		if !strings.Contains(string(body), "token") {
			t.Error("/ops page does not reference the token query parameter, so its polling would 401")
		}
	})
}

// TestOpsTokenIsRedactedFromAccessLog asserts the token never reaches a log sink,
// since /ops is commonly opened as a plain browser URL.
func TestOpsTokenIsRedactedFromAccessLog(t *testing.T) {
	const token = "log-secret-token-123"

	var buf strings.Builder
	s := newStackWithLogWriter(t, func(c *config.Config) { c.Metrics.Token = token }, &buf)

	res, err := http.Get(s.baseURL + "/ops/data?token=" + token)
	if err != nil {
		t.Fatalf("GET /ops/data: %v", err)
	}
	_ = res.Body.Close()

	out := buf.String()
	if strings.Contains(out, token) {
		t.Fatalf("access log leaked the ops token:\n%s", out)
	}
	if !strings.Contains(out, "token=***") {
		t.Fatalf("access log did not mask the token parameter:\n%s", out)
	}
}
