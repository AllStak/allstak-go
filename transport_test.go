package allstak

import (
	"net/http"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name   string
		header string
		want   time.Duration
	}{
		{"integer seconds", "2", 2 * time.Second},
		{"empty falls back to backoff", "", 0},
		{"garbage falls back to backoff", "soon-ish", 0},
		{"zero is no delay", "0", 0},
		{"negative is no delay", "-5", 0},
		{"over cap clamps to 300s", "600", 300 * time.Second},
		{"exactly cap", "300", 300 * time.Second},
		// HTTP-date 30s in the future.
		{"http-date future", now.Add(30 * time.Second).Format(http.TimeFormat), 30 * time.Second},
		// HTTP-date in the past resolves to no delay.
		{"http-date past", now.Add(-1 * time.Minute).Format(http.TimeFormat), 0},
		// HTTP-date far in the future clamps.
		{"http-date over cap", now.Add(10 * time.Minute).Format(http.TimeFormat), 300 * time.Second},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseRetryAfter(tc.header, now)
			if got != tc.want {
				t.Errorf("parseRetryAfter(%q) = %v, want %v", tc.header, got, tc.want)
			}
		})
	}
}

func TestParseRetryAfterWhitespace(t *testing.T) {
	now := time.Now()
	if got := parseRetryAfter("  5  ", now); got != 5*time.Second {
		t.Errorf("parseRetryAfter with whitespace = %v, want 5s", got)
	}
}
