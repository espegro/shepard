package gateway

import (
	"net/http"
	"strings"
	"testing"
)

func TestLoggedHeadersRedactSecrets(t *testing.T) {
	headers := http.Header{
		"Authorization": []string{"Bearer secret"},
		"Cookie":        []string{"session=secret"},
		"X-Trace":       []string{"trace-id"},
	}
	logged := loggedHeaders(headers)
	if logged["Authorization"] != "[REDACTED]" || logged["Cookie"] != "[REDACTED]" {
		t.Fatalf("sensitive headers were not redacted: %+v", logged)
	}
	if logged["X-Trace"] != "trace-id" {
		t.Fatalf("non-sensitive header changed: %+v", logged)
	}
}

func TestBoundedLogBody(t *testing.T) {
	body, truncated := boundedLogBody([]byte("abcdef"), 3)
	if body != "abc" || !truncated {
		t.Fatalf("unexpected bounded body: %q truncated=%v", body, truncated)
	}
	if _, truncated := boundedLogBody([]byte("abc"), 3); truncated {
		t.Fatal("exactly sized body should not be marked truncated")
	}
	if strings.TrimSpace(body) == "" {
		t.Fatal("body unexpectedly empty")
	}
}
