package allstak

// Diagnostics is a privacy-safe SDK health snapshot. It contains counters and
// queue sizes only; it never includes telemetry payloads, headers, user fields,
// breadcrumbs, request bodies, or error messages.
type Diagnostics struct {
	EventsCaptured          int64  `json:"eventsCaptured"`
	EventsSent              int64  `json:"eventsSent"`
	EventsFailed            int64  `json:"eventsFailed"`
	EventsDropped           int64  `json:"eventsDropped"`
	EventsPersisted         int64  `json:"eventsPersisted"`
	EventsReplayed          int64  `json:"eventsReplayed"`
	QueueSize               int    `json:"queueSize"`
	RetryAttempts           int64  `json:"retryAttempts"`
	RateLimitedCount        int64  `json:"rateLimitedCount"`
	CompressedPayloads      int64  `json:"compressedPayloads"`
	UncompressedPayloads    int64  `json:"uncompressedPayloads"`
	CompressionBytesSaved   int64  `json:"compressionBytesSaved"`
	SanitizerRedactionCount *int64 `json:"sanitizerRedactionCount,omitempty"`
	ActiveTraceCount        int    `json:"activeTraceCount"`
	ActiveSpanCount         int    `json:"activeSpanCount"`
	BreadcrumbCount         int    `json:"breadcrumbCount"`
	SessionRecoveryCount    int64  `json:"sessionRecoveryCount"`
	Disabled                bool   `json:"disabled"`
}

type transportDiagnostics struct {
	RetryAttempts    int64
	RateLimitedCount int64
	Compressed       int64
	Uncompressed     int64
	BytesSaved       int64
	Disabled         bool
}

type diagnosticTransport interface {
	diagnostics() transportDiagnostics
}
