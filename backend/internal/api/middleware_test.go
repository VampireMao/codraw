package api

import (
	"strings"
	"testing"
)

func TestRedactQueryMasksCredentials(t *testing.T) {
	raw := "room=ABC123&cid=cid_secret&name=name_secret&key=key_secret&rt=rt_secret&st=st_secret&after=42"
	got := redactQuery(raw)
	for _, secret := range []string{"cid_secret", "name_secret", "key_secret", "rt_secret", "st_secret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("redactQuery leaked %q in %q", secret, got)
		}
	}
	for _, masked := range []string{"cid=***", "name=***", "key=***", "rt=***", "st=***"} {
		if !strings.Contains(got, masked) {
			t.Fatalf("redactQuery did not mask %q: %q", masked, got)
		}
	}
	if !strings.Contains(got, "after=42") {
		t.Fatalf("redactQuery over-redacted a harmless parameter: %q", got)
	}
}
