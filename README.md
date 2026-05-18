# allstak-go

**Structured error + log capture for Go services. Zero-allocation hot path, context-aware.**

[![Go Reference](https://pkg.go.dev/badge/github.com/allstak-io/allstak-go.svg)](https://pkg.go.dev/github.com/allstak-io/allstak-go)
[![CI](https://github.com/allstak-io/allstak-go/actions/workflows/ci.yml/badge.svg)](https://github.com/allstak-io/allstak-go/actions)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

Official AllStak SDK for Go — captures errors, structured logs, inbound/outbound HTTP, SQL queries, distributed spans, and cron heartbeats.

## Dashboard

View captured events live at [app.allstak.sa](https://app.allstak.sa).

![AllStak dashboard](https://app.allstak.sa/images/dashboard-preview.png)

## Features

- `CaptureException` with automatic stack trace and error-chain unwrapping
- Structured logs with levels (`debug`, `info`, `warn`, `error`, `fatal`)
- `net/http` middleware for inbound request telemetry
- Outbound HTTP transport wrapper for egress capture
- `database/sql` query capture with statement normalization
- Distributed tracing via context-carried trace and span IDs
- Cron heartbeats via `SendHeartbeat`
- Per-stream worker goroutines — no head-of-line blocking

## Installation

```bash
go get github.com/allstak-io/allstak-go
```

## Quick Start

> Create a project at [app.allstak.sa](https://app.allstak.sa) to get your API key.

```go
package main

import (
    "context"
    "errors"
    "os"

    "github.com/allstak-io/allstak-go"
)

func main() {
    client := allstak.New(allstak.Config{
        APIKey:      os.Getenv("ALLSTAK_API_KEY"),
        Environment: "production",
        Release:     "myapp@1.0.0",
        ServiceName: "myapp-api",
    })
    defer client.Close(context.Background())

    client.CaptureException(context.Background(), errors.New("test: hello from allstak-go"))
}
```

Run the file — the test error appears in your dashboard within seconds.

## Get Your API Key

1. Sign up at [app.allstak.sa](https://app.allstak.sa)
2. Create a project
3. Copy your API key from **Project Settings → API Keys**
4. Export it as `ALLSTAK_API_KEY` or pass it to `allstak.Config{APIKey: ...}`

## Configuration

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `APIKey` | `string` | yes | — | Project API key (`ask_live_…`) |
| `Environment` | `string` | no | `production` | Deployment env |
| `Release` | `string` | no | — | Version / git SHA |
| `ServiceName` | `string` | no | binary name | Logical service identifier |
| `FlushInterval` | `time.Duration` | no | `2s` | Background flush cadence |
| `BatchSize` | `int` | no | `50` | Events per ingest request |
| `QueueCapacity` | `int` | no | `1000` | Per-stream buffer size |
| `MaxRetries` | `int` | no | `3` | Flush retry count |
| `RequestTimeout` | `time.Duration` | no | `5s` | Per-request timeout |
| `Debug` | `bool` | no | `false` | Verbose stderr logging |
| `RedactKeys` | `[]string` | no | `nil` | Extra key patterns added to the built-in deny-list (plain substrings or Go regexps starting with `(?` / `^`) |
| `CaptureRequestHeaders` | `bool` | no | `false` | Capture inbound/outbound request headers (sensitive values are redacted) |
| `CaptureResponseHeaders` | `bool` | no | `false` | Capture inbound/outbound response headers (sensitive values are redacted) |

The ingest host is not a field on `Config`; it defaults to `https://api.allstak.sa` and can be overridden with the `ALLSTAK_HOST` env var (self-hosted only).

## Privacy & Redaction

The SDK strips sensitive values from any payload it sends. The built-in deny-list (always applied) matches the following keys case-insensitively:

- Header-style: `Authorization`, `Proxy-Authorization`, `Cookie`, `Set-Cookie`, `X-API-Key`, `X-Auth-Token`, `X-Access-Token`, `X-AllStak-Key`
- Key-suffix patterns: `*token`, `*api_key` / `*api-key`, `*password`, `*passwd`, `*secret`, `*session_id`, `*csrf`

Redaction is applied to:

- `CaptureError.Metadata` and breadcrumb `Data` (recursive into nested maps and slices)
- `CaptureLog.Metadata` (recursive)
- `CaptureHTTPRequest.Metadata` (recursive)
- `CaptureSpan.Tags` (`map[string]string`)
- Captured headers when `CaptureRequestHeaders` / `CaptureResponseHeaders` are enabled

To add custom patterns:

```go
client := allstak.New(allstak.Config{
    APIKey:     "ask_live_...",
    RedactKeys: []string{"internal_id", `(?i)^x-tenant-`},
})
```

### Body capture

Request and response **bodies are never captured** by this SDK. The wire-format fields `RequestBody` / `ResponseBody` exist for parity with other SDKs but the Go middleware leaves them empty. Body capture would change timing semantics and break streaming endpoints, so it is intentionally not exposed.

### Debug logging

`Config.Debug` is **off by default**. Even when on, the SDK never logs request or header contents — only client lifecycle and counter summaries.

## Troubleshooting

- **`X-AllStak-Trace-Id` not echoed on responses** — confirm `allstak.Middleware(client)` is wrapped around your router and that nothing strips response headers downstream.
- **Events not showing in dashboard** — call `client.Flush(ctx)` before `os.Exit` / process termination. The background workers can have pending events.
- **`go get` 403 / module not found** — check `GOPROXY`; the module is published via `proxy.golang.org`. There is no replace directive required.

## Limitations

- Beta release — no live dashboard certification recorded yet (see `docs/reports/` in the platform repo).
- No automatic body capture. Pre-collect bodies in your application code if needed and pass them through `Metadata`.
- `Branch`, `CommitSha`, and `Release` are auto-detected from common CI env vars but not from `runtime/debug.ReadBuildInfo()` yet.

## Example Usage

Capture an exception:

```go
if err := chargeCard(ctx, order); err != nil {
    client.CaptureException(ctx, err)
}
```

Send a structured log:

```go
client.CaptureLog(allstak.LogPayload{
    Level:   "info",
    Message: "Order processed",
    Metadata: map[string]any{"orderId": "ORD-123"},
})
```

Send a cron heartbeat:

```go
client.SendHeartbeat(ctx, allstak.HeartbeatPayload{
    Slug:       "daily-report",
    Status:     "ok",
    DurationMs: 1234,
})
```

## Production Endpoint

Production endpoint: `https://api.allstak.sa`. Override via `ALLSTAK_HOST`:

```bash
export ALLSTAK_HOST=https://allstak.mycorp.com
```

## Fail-Open Reliability

AllStak telemetry is best-effort. Runtime capture APIs enqueue into bounded
background workers and drop old telemetry under pressure rather than blocking
the host process. If AllStak is down, slow, rate-limiting, or under
maintenance, customer requests continue normally.

- Capture APIs are non-blocking and use bounded queues.
- Inbound HTTP middleware records telemetry without waiting on AllStak ingest.
- `SendHeartbeat`, `Flush`, and `Close` are bounded. If the caller passes a
  context without a deadline, heartbeat uses an internal deadline derived from
  `RequestTimeout`.
- DNS, connection, timeout, 429, 500, and 503 failure modes are covered by
  automated fail-open tests.

## Links

- Documentation: https://docs.allstak.sa
- Dashboard: https://app.allstak.sa
- Source: https://github.com/allstak-io/allstak-go

## License

MIT © AllStak

## Production readiness

### Install

`go get github.com/allstak-io/allstak-go@v0.1.2`

### Quick Start

Use the minimal setup shown above in this README, set an AllStak API key through environment/configuration, and verify telemetry in a non-production project before enabling it for users. Do not hardcode API keys in source code.

### Configuration

Configure the API key, ingest host, environment, release, service name, sample rates, and optional capture settings explicitly for each deployment. Default production host is `https://api.allstak.sa` unless this SDK documents otherwise.

### Environment Variables

Prefer environment variables for secrets and deployment-specific values: `ALLSTAK_API_KEY`, `ALLSTAK_HOST`, `ALLSTAK_ENVIRONMENT`, `ALLSTAK_RELEASE`, and SDK-specific build/source-map tokens where applicable. Client-side frameworks must only expose public client keys using their framework-specific public env var conventions.

### Framework Compatibility

Go module targets Go 1.23. Root tests cover core behavior; Gin/GORM module compatibility should be validated in real services before stable launch.

### What Data Is Captured

Depending on the SDK and enabled integrations, AllStak can capture exceptions, logs, breadcrumbs, HTTP request metadata, traces/spans, release/environment tags, user context supplied by the application, cron/job heartbeat status, and source-map artifact metadata. Body/header capture is optional where supported and should stay disabled unless explicitly needed.

### Privacy / PII / Redaction

Do not send secrets, passwords, tokens, payment data, national IDs, or raw request/response bodies unless the SDK documentation for this package explicitly says the field is redacted and the behavior has been verified in your app. Authorization, cookie, token, password, secret, API key, and similar fields should be masked by default where capture is implemented. Add `beforeSend`/filter hooks or equivalent application-side scrubbing for domain-specific PII.

### Production Safety

The SDK must fail open: telemetry failures must not crash or materially block the host application. Keep queues bounded, retries bounded, debug logging off in production, and capture rates conservative until overhead is measured in your application. Live dashboard certification was **not verified** in the 2026-05-17 release-gate audit because live credentials were not available.

### Troubleshooting

If telemetry is missing, verify the package version, API key, ingest host, environment, release, network access to `https://api.allstak.sa`, sampling settings, framework integration order, and whether the SDK is disabled after an auth failure. For source maps, verify release/dist values and artifact upload responses.

### Release / Source Map Setup

Not applicable.

### Version Compatibility

Keep the package manifest version, runtime SDK version constant, changelog entry, git tag, and registry version aligned. Do not publish from a dirty checkout.

### Known Limitations

Available through the Go module proxy at v0.1.1. Keep module tags and changelog aligned. Live dashboard proof, performance overhead, retry-storm behavior, and full production hardening must be revalidated before claiming production-stable readiness.

### Stability Status

Current status: **beta**. This SDK is not production-stable unless a later certification report explicitly says so with live dashboard evidence.
