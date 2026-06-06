package allstak

// This file is the single source of truth for wire-format payloads sent to
// the AllStak backend. Field names and JSON tags mirror the Java/Kotlin
// backend DTOs exactly — do not drift. If you change one of these types,
// grep the backend `modules/*/dto/*IngestRequest.java` files and verify.

// ── Errors ────────────────────────────────────────────────────────────────

// ErrorPayload is the body of POST /ingest/v1/errors. One event per request.
type ErrorPayload struct {
	ExceptionClass string         `json:"exceptionClass"`
	Message        string         `json:"message"`
	StackTrace     []string       `json:"stackTrace,omitempty"`
	Level          string         `json:"level,omitempty"`
	Environment    string         `json:"environment,omitempty"`
	Release        string         `json:"release,omitempty"`
	SessionID      string         `json:"sessionId,omitempty"`
	User           *UserContext   `json:"user,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
	TraceID        string         `json:"traceId,omitempty"`
	SpanID         string         `json:"spanId,omitempty"`
	ParentSpanID   string         `json:"parentSpanId,omitempty"`
	RequestID      string         `json:"requestId,omitempty"`
	RequestContext *ReqContext    `json:"requestContext,omitempty"`
	Breadcrumbs    []Breadcrumb   `json:"breadcrumbs,omitempty"`
	Mechanism      string         `json:"mechanism,omitempty"`
	Handled        *bool          `json:"handled,omitempty"`
	// Phase 2 — v2 ingest contract.
	SDKName    string  `json:"sdkName,omitempty"`
	SDKVersion string  `json:"sdkVersion,omitempty"`
	Platform   string  `json:"platform,omitempty"`
	Dist       string  `json:"dist,omitempty"`
	Frames     []Frame `json:"frames,omitempty"`
}

// Frame mirrors the backend ErrorIngestRequest.Frame. Populated from
// runtime.CallersFrames so the dashboard can show structured call sites.
type Frame struct {
	Filename string `json:"filename,omitempty"`
	AbsPath  string `json:"absPath,omitempty"`
	Function string `json:"function,omitempty"`
	Lineno   int    `json:"lineno,omitempty"`
	Colno    int    `json:"colno,omitempty"`
	InApp    bool   `json:"inApp,omitempty"`
	Platform string `json:"platform,omitempty"`
	DebugID  string `json:"debugId,omitempty"`
}

// UserContext identifies the authenticated user associated with an event.
// All fields are optional.
type UserContext struct {
	ID    string `json:"id,omitempty"`
	Email string `json:"email,omitempty"`
	IP    string `json:"ip,omitempty"`
}

// ReqContext captures inbound HTTP request metadata for error correlation.
type ReqContext struct {
	Method     string `json:"method,omitempty"`
	Path       string `json:"path,omitempty"`
	Host       string `json:"host,omitempty"`
	StatusCode int    `json:"statusCode,omitempty"`
	UserAgent  string `json:"userAgent,omitempty"`
}

// Breadcrumb is a lightweight event leading up to an error. namespace-compatible.
type Breadcrumb struct {
	Timestamp string         `json:"timestamp,omitempty"`
	Type      string         `json:"type,omitempty"`
	Category  string         `json:"category,omitempty"`
	Message   string         `json:"message,omitempty"`
	Level     string         `json:"level,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
}

// ── Logs ──────────────────────────────────────────────────────────────────

// LogPayload is the body of POST /ingest/v1/logs. One event per request.
type LogPayload struct {
	Level       string         `json:"level"`
	Message     string         `json:"message"`
	Service     string         `json:"service,omitempty"`
	Environment string         `json:"environment,omitempty"`
	TraceID     string         `json:"traceId,omitempty"`
	SpanID      string         `json:"spanId,omitempty"`
	RequestID   string         `json:"requestId,omitempty"`
	UserID      string         `json:"userId,omitempty"`
	ErrorID     string         `json:"errorId,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// ── HTTP requests ─────────────────────────────────────────────────────────

// HTTPRequestBatch is the body of POST /ingest/v1/http-requests.
// ProjectID is left empty — the API key header identifies the project.
type HTTPRequestBatch struct {
	Requests []HTTPRequestItem `json:"requests"`
}

// HTTPRequestItem is one inbound or outbound HTTP event.
type HTTPRequestItem struct {
	TraceID          string `json:"traceId,omitempty"`
	RequestID        string `json:"requestId,omitempty"`
	Direction        string `json:"direction"` // "inbound" | "outbound"
	Method           string `json:"method"`
	Host             string `json:"host"`
	Path             string `json:"path"`
	StatusCode       int    `json:"statusCode"`
	DurationMs       int    `json:"durationMs"`
	RequestSize      int    `json:"requestSize"`
	ResponseSize     int    `json:"responseSize"`
	UserID           string `json:"userId,omitempty"`
	ErrorFingerprint string `json:"errorFingerprint,omitempty"`
	Timestamp        string `json:"timestamp"` // ISO-8601
	SpanID           string `json:"spanId,omitempty"`
	ParentSpanID     string `json:"parentSpanId,omitempty"`
	RequestHeaders   string `json:"requestHeaders,omitempty"`
	ResponseHeaders  string `json:"responseHeaders,omitempty"`
	RequestBody      string `json:"requestBody,omitempty"`
	ResponseBody     string `json:"responseBody,omitempty"`
	Environment      string `json:"environment,omitempty"`
	Release          string `json:"release,omitempty"`
	// Release-tracking metadata. Backend stores the rest inside metadata
	// today; once dedicated columns land, the ingester reads them out.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// ── Database queries ──────────────────────────────────────────────────────

// DBQueryBatch is the body of POST /ingest/v1/db.
type DBQueryBatch struct {
	Queries []DBQueryItem `json:"queries"`
}

// DBQueryItem is a single database query event.
type DBQueryItem struct {
	NormalizedQuery string `json:"normalizedQuery"`
	QueryHash       string `json:"queryHash"`
	QueryType       string `json:"queryType"`
	DurationMs      int64  `json:"durationMs"`
	TimestampMillis int64  `json:"timestampMillis"`
	Status          string `json:"status"` // "success" | "error"
	ErrorMessage    string `json:"errorMessage,omitempty"`
	DatabaseName    string `json:"databaseName,omitempty"`
	DatabaseType    string `json:"databaseType,omitempty"`
	Service         string `json:"service,omitempty"`
	Environment     string `json:"environment,omitempty"`
	TraceID         string `json:"traceId,omitempty"`
	SpanID          string `json:"spanId,omitempty"`
	RowsAffected    int    `json:"rowsAffected"`
}

// ── Tracing spans ─────────────────────────────────────────────────────────

// SpanBatch is the body of POST /ingest/v1/spans.
type SpanBatch struct {
	Spans []SpanItem `json:"spans"`
}

// SpanItem is one span in a distributed trace.
type SpanItem struct {
	TraceID         string            `json:"traceId"`
	SpanID          string            `json:"spanId"`
	ParentSpanID    string            `json:"parentSpanId,omitempty"`
	Operation       string            `json:"operation"`
	Description     string            `json:"description,omitempty"`
	Status          string            `json:"status,omitempty"`
	DurationMs      int64             `json:"durationMs"`
	StartTimeMillis int64             `json:"startTimeMillis"`
	EndTimeMillis   int64             `json:"endTimeMillis"`
	Service         string            `json:"service,omitempty"`
	Environment     string            `json:"environment,omitempty"`
	Tags            map[string]string `json:"tags,omitempty"`
	Data            string            `json:"data,omitempty"`
}

// ── Cron heartbeat ────────────────────────────────────────────────────────

// HeartbeatPayload is the body of POST /ingest/v1/heartbeat. The backend
// auto-creates a monitor on first ping for unknown slugs.
type HeartbeatPayload struct {
	Slug       string `json:"slug"`
	Status     string `json:"status"` // "success" | "failed"
	DurationMs int    `json:"durationMs"`
	Message    string `json:"message,omitempty"`
}
