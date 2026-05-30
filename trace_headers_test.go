package allstak

import (
	"net/http"
	"testing"
)

func TestMergeBaggagePreservesVendorMembersAndReplacesSDKMembers(t *testing.T) {
	got := MergeBaggage("vendor=value, allstak-trace_id=old, allstak-span_id=old", "tt", "rr", "ss")
	want := "vendor=value,allstak-trace_id=tt,allstak-request_id=rr,allstak-span_id=ss"
	if got != want {
		t.Fatalf("unexpected baggage: got %q want %q", got, want)
	}
}

func TestSetTraceRequestHeadersIncludesTraceparentAndBaggage(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://example.test/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("baggage", "vendor=value")

	SetTraceRequestHeaders(req, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "req-1", "bbbbbbbbbbbbbbbb")

	if got := req.Header.Get("traceparent"); got != "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-01" {
		t.Fatalf("traceparent = %q", got)
	}
	if got := req.Header.Get("baggage"); got != "vendor=value,allstak-trace_id=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa,allstak-request_id=req-1,allstak-span_id=bbbbbbbbbbbbbbbb" {
		t.Fatalf("baggage = %q", got)
	}
	if got := req.Header.Get("AllStak-Baggage"); got != "allstak-trace_id=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa,allstak-request_id=req-1,allstak-span_id=bbbbbbbbbbbbbbbb" {
		t.Fatalf("AllStak-Baggage = %q", got)
	}
}

func TestTraceHeadersFromRequestContinuesValidTraceparent(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://example.test/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	req.Header.Set("X-AllStak-Trace-Id", "not-a-valid-trace-id")

	headers := TraceHeadersFromRequest(req)
	if headers.TraceID != "0af7651916cd43dd8448eb211c80319c" {
		t.Fatalf("TraceID = %q", headers.TraceID)
	}
	if headers.ParentSpanID != "b7ad6b7169203331" {
		t.Fatalf("ParentSpanID = %q", headers.ParentSpanID)
	}
}

func TestTraceHeadersFromRequestRejectsInvalidTraceContext(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://example.test/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("traceparent", "00-00000000000000000000000000000000-0000000000000000-01")
	req.Header.Set("X-AllStak-Trace-Id", "not-a-valid-trace-id")
	req.Header.Set("X-AllStak-Span-Id", "also-invalid")

	headers := TraceHeadersFromRequest(req)
	if !isValidTraceID(headers.TraceID) {
		t.Fatalf("TraceID not normalized W3C id: %q", headers.TraceID)
	}
	if headers.ParentSpanID != "" {
		t.Fatalf("ParentSpanID = %q, want empty", headers.ParentSpanID)
	}
}

func TestSetTraceRequestHeadersNormalizesUUIDFormIDs(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://example.test/", nil)
	if err != nil {
		t.Fatal(err)
	}

	SetTraceRequestHeaders(req,
		"7f3ac1d9-2b8e-4a6f-8c1a-000000000001",
		"req-1",
		"abcdef01-2345-6789-abcd-ef0123456789")

	if got := req.Header.Get("traceparent"); got != "00-7f3ac1d92b8e4a6f8c1a000000000001-abcdef0123456789-01" {
		t.Fatalf("traceparent = %q", got)
	}
	if got := req.Header.Get("X-AllStak-Trace-Id"); got != "7f3ac1d92b8e4a6f8c1a000000000001" {
		t.Fatalf("X-AllStak-Trace-Id = %q", got)
	}
	if got := req.Header.Get("X-AllStak-Span-Id"); got != "abcdef0123456789" {
		t.Fatalf("X-AllStak-Span-Id = %q", got)
	}
}
