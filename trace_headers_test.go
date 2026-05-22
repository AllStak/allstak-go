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
