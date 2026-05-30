package allstak

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestHTTPTransportCompressionTinyPayload(t *testing.T) {
	var gotEncoding string
	var gotPayload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEncoding = r.Header.Get("Content-Encoding")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if err := json.Unmarshal(body, &gotPayload); err != nil {
			t.Fatalf("unmarshal body: %v", err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	transport := newHTTPTransport(srv.URL, "ask_test", time.Second, 0, false, scrubOptions{scrubValues: true})
	if err := transport.send(context.Background(), pathLogs, map[string]any{"message": "hi"}); err != nil {
		t.Fatalf("send failed: %v", err)
	}
	if gotEncoding != "" {
		t.Fatalf("Content-Encoding = %q, want empty", gotEncoding)
	}
	if gotPayload["message"] != "hi" {
		t.Fatalf("payload message = %v, want hi", gotPayload["message"])
	}
	diag := transport.diagnostics()
	if diag.Uncompressed != 1 || diag.Compressed != 0 || diag.BytesSaved != 0 {
		t.Fatalf("diagnostics = %+v, want one uncompressed payload", diag)
	}
}

func TestHTTPTransportCompressionLargePayload(t *testing.T) {
	var gotEncoding string
	var gotPayload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEncoding = r.Header.Get("Content-Encoding")
		reader, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Fatalf("gzip reader: %v", err)
		}
		defer reader.Close()
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("read gzip body: %v", err)
		}
		if err := json.Unmarshal(body, &gotPayload); err != nil {
			t.Fatalf("unmarshal body: %v", err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	message := strings.Repeat("x", 8000)
	transport := newHTTPTransport(srv.URL, "ask_test", time.Second, 0, false, scrubOptions{scrubValues: true})
	if err := transport.send(context.Background(), pathErrors, map[string]any{"message": message}); err != nil {
		t.Fatalf("send failed: %v", err)
	}
	if gotEncoding != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", gotEncoding)
	}
	if gotPayload["message"] != message {
		t.Fatalf("payload message mismatch")
	}
	diag := transport.diagnostics()
	if diag.Compressed != 1 || diag.Uncompressed != 0 || diag.BytesSaved <= 0 {
		t.Fatalf("diagnostics = %+v, want one compressed payload", diag)
	}
}
