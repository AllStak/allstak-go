package allstak

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// wireOpts is the policy the transport applies on the wire path with the
// secure default (SendDefaultPii=false).
var wireOpts = scrubOptions{scrubValues: true, sendDefaultPii: false}

// ── F2: DB error strings and heartbeat messages must be scrubbed ────────────

func TestScrubPayloadDBQueryErrorMessage(t *testing.T) {
	batch := DBQueryBatch{Queries: []DBQueryItem{{
		NormalizedQuery: "INSERT INTO users (email) VALUES (?)",
		Status:          "error",
		ErrorMessage:    `duplicate key value violates unique constraint "users_email_key": Key (email)=(user@example.com) already exists`,
	}, {
		NormalizedQuery: "INSERT INTO payments (card) VALUES (?)",
		Status:          "error",
		ErrorMessage:    `Duplicate entry '4111111111111111' for key 'payments.card'`,
	}}}

	out, ok := scrubPayload(batch, wireOpts).(DBQueryBatch)
	if !ok {
		t.Fatalf("scrubPayload returned %T, want DBQueryBatch", out)
	}
	if strings.Contains(out.Queries[0].ErrorMessage, "user@example.com") {
		t.Fatalf("email leaked through DB error message: %q", out.Queries[0].ErrorMessage)
	}
	if !strings.Contains(out.Queries[0].ErrorMessage, redactedValue) {
		t.Fatalf("DB error message not scrubbed: %q", out.Queries[0].ErrorMessage)
	}
	if strings.Contains(out.Queries[1].ErrorMessage, "4111111111111111") {
		t.Fatalf("card number leaked through DB error message: %q", out.Queries[1].ErrorMessage)
	}
	// The original must never be mutated.
	if !strings.Contains(batch.Queries[0].ErrorMessage, "user@example.com") {
		t.Fatal("scrubPayload mutated the caller's batch")
	}
}

func TestScrubPayloadHeartbeatMessage(t *testing.T) {
	hb := HeartbeatPayload{
		Slug:    "nightly-sync",
		Status:  "failed",
		Message: "sync failed for account user@example.com (ssn 123-45-6789)",
	}
	out, ok := scrubPayload(hb, wireOpts).(HeartbeatPayload)
	if !ok {
		t.Fatalf("scrubPayload returned %T, want HeartbeatPayload", out)
	}
	if strings.Contains(out.Message, "user@example.com") || strings.Contains(out.Message, "123-45-6789") {
		t.Fatalf("PII leaked through heartbeat message: %q", out.Message)
	}

	ptrOut, ok := scrubPayload(&hb, wireOpts).(*HeartbeatPayload)
	if !ok || strings.Contains(ptrOut.Message, "user@example.com") {
		t.Fatalf("pointer heartbeat not scrubbed: %#v", ptrOut)
	}
	if !strings.Contains(hb.Message, "user@example.com") {
		t.Fatal("scrubPayload mutated the caller's payload")
	}
}

// ── F7: scrubber type coverage — []string, structs, errors, typed maps ──────

func TestScrubValueStringSlice(t *testing.T) {
	in := map[string]any{
		"recipients": []string{"user@example.com", "plain-value"},
	}
	out := scrubMapOpts(in, wireOpts)
	got, ok := out["recipients"].([]string)
	if !ok {
		t.Fatalf("recipients type = %T, want []string", out["recipients"])
	}
	if got[0] != redactedValue {
		t.Fatalf("email in []string not scrubbed: %q", got[0])
	}
	if got[1] != "plain-value" {
		t.Fatalf("non-PII []string element corrupted: %q", got[1])
	}
	if in["recipients"].([]string)[0] != "user@example.com" {
		t.Fatal("input mutated")
	}
}

func TestScrubValueStructFields(t *testing.T) {
	type account struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Plan     string `json:"plan"`
		internal string
	}
	in := map[string]any{
		"account": account{Email: "user@example.com", Password: "hunter2", Plan: "pro", internal: "x"},
	}
	out := scrubMapOpts(in, wireOpts)
	got, ok := out["account"].(map[string]any)
	if !ok {
		t.Fatalf("account type = %T, want map[string]any", out["account"])
	}
	if got["password"] != redactedValue {
		t.Fatalf("password field not key-redacted: %v", got["password"])
	}
	if got["email"] != redactedValue {
		t.Fatalf("email value not scrubbed: %v", got["email"])
	}
	if got["plan"] != "pro" {
		t.Fatalf("plan corrupted: %v", got["plan"])
	}
	if _, leaked := got["internal"]; leaked {
		t.Fatal("unexported field leaked")
	}
}

func TestScrubValueErrorValue(t *testing.T) {
	in := map[string]any{
		"cause": errors.New("lookup failed for user@example.com"),
	}
	out := scrubMapOpts(in, wireOpts)
	got, ok := out["cause"].(string)
	if !ok {
		t.Fatalf("cause type = %T, want string", out["cause"])
	}
	if strings.Contains(got, "user@example.com") {
		t.Fatalf("email leaked through error value: %q", got)
	}
}

func TestScrubValueTypedStringMap(t *testing.T) {
	type headers map[string]string
	in := map[string]any{
		"hdrs": headers{"Authorization": "Bearer abc", "X-Plain": "ok user@example.com"},
	}
	out := scrubMapOpts(in, wireOpts)
	got, ok := out["hdrs"].(map[string]any)
	if !ok {
		t.Fatalf("hdrs type = %T, want map[string]any", out["hdrs"])
	}
	if got["Authorization"] != redactedValue {
		t.Fatalf("Authorization not redacted: %v", got["Authorization"])
	}
	if s, _ := got["X-Plain"].(string); strings.Contains(s, "user@example.com") {
		t.Fatalf("email leaked through typed map value: %q", s)
	}
}

func TestScrubValueCustomMarshalersPassThrough(t *testing.T) {
	now := time.Now()
	in := map[string]any{"at": now, "count": 42, "ok": true}
	out := scrubMapOpts(in, wireOpts)
	if got, ok := out["at"].(time.Time); !ok || !got.Equal(now) {
		t.Fatalf("time.Time corrupted: %#v", out["at"])
	}
	if out["count"] != 42 || out["ok"] != true {
		t.Fatalf("primitives corrupted: %#v", out)
	}
}

func TestScrubValuePointerToStruct(t *testing.T) {
	type creds struct {
		Token string `json:"token"`
	}
	in := map[string]any{"creds": &creds{Token: "secret-token"}}
	out := scrubMapOpts(in, wireOpts)
	got, ok := out["creds"].(map[string]any)
	if !ok {
		t.Fatalf("creds type = %T, want map[string]any", out["creds"])
	}
	if got["token"] != redactedValue {
		t.Fatalf("token field not redacted: %v", got["token"])
	}
}

// ── Free-text key=value secrets + IBAN (sdk-validation regressions) ─────────

func TestKvSecretScrubbedAtStringStart(t *testing.T) {
	opts := scrubOptions{scrubValues: true}
	out := scrubString("password=hunter2 rest", opts)
	if strings.Contains(out, "hunter2") {
		t.Fatalf("password value leaked: %q", out)
	}
	if !strings.Contains(out, "password=") {
		t.Fatalf("key should be kept for debuggability: %q", out)
	}
	if out2 := scrubString("note client_secret=s3cr3t&next=1", opts); strings.Contains(out2, "s3cr3t") {
		t.Fatalf("client_secret leaked: %q", out2)
	}
	if out3 := scrubString("otp: 123456", opts); strings.Contains(out3, "123456") {
		t.Fatalf("otp leaked: %q", out3)
	}
}

func TestIbanScrubbedAlwaysEvenWithPiiOptIn(t *testing.T) {
	out := scrubString("transfer to SA4420000001234567891234 failed", scrubOptions{scrubValues: true, sendDefaultPii: true})
	if strings.Contains(out, "SA4420000001234567891234") {
		t.Fatalf("IBAN leaked with pii opt-in: %q", out)
	}
	out2 := scrubString("iban DE89370400440532013000 ok", scrubOptions{scrubValues: true})
	if strings.Contains(out2, "DE89370400440532013000") {
		t.Fatalf("IBAN leaked: %q", out2)
	}
}

func TestKvIbanNoOverscrub(t *testing.T) {
	opts := scrubOptions{scrubValues: true}
	for _, in := range []string{"order id 1234 for user", "auth flow started", "SA44 is a region code"} {
		if got := scrubString(in, opts); got != in {
			t.Fatalf("over-scrubbed %q -> %q", in, got)
		}
	}
}
