# allstak-go

**Structured error + log capture for Go services. Zero-allocation hot path, context-aware.**

[![Go Reference](https://pkg.go.dev/badge/github.com/AllStak/allstak-go.svg)](https://pkg.go.dev/github.com/AllStak/allstak-go)
[![CI](https://github.com/AllStak/allstak-go/actions/workflows/ci.yml/badge.svg)](https://github.com/AllStak/allstak-go/actions)
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

## What You Get

Once integrated, every event flows to your AllStak dashboard:

- **Errors** — stack traces, error chains, release + environment tags
- **Logs** — structured logs with search and filters
- **HTTP** — inbound and outbound request timing, status codes, failed calls
- **Database** — `database/sql` query capture with statement normalization
- **Traces** — distributed spans with context propagation
- **Cron monitors** — scheduled job success/failure tracking
- **Alerts** — email and webhook notifications on regressions

## ⚠️ Module Path Migration Notice

> **Previous module path:** `github.com/allstak-io/allstak-go`
> **Current canonical path:** `github.com/AllStak/allstak-go`

If you were using the old module path, update your code:

**1. Update `go.mod`:**
```bash
go get github.com/AllStak/allstak-go@latest
```

**2. Update all import paths:**
```bash
# Find all files using the old path
grep -r "allstak-io/allstak-go" .

# Replace globally (Linux/macOS)
find . -name "*.go" | xargs sed -i 's|github.com/allstak-io/allstak-go|github.com/AllStak/allstak-go|g'
```

**3. Run `go mod tidy`** to clean up your `go.sum`.

The old repository at `github.com/allstak-io/allstak-go` is archived and read-only.
## Installation

```bash
go get github.com/AllStak/allstak-go
```

## Quick Start

> Create a project at [app.allstak.sa](https://app.allstak.sa) to get your API key.

```go
package main

import (
    "context"
    "errors"
    "os"

    "github.com/AllStak/allstak-go"
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

The ingest host is not a field on `Config`; it defaults to `https://api.allstak.sa` and can be overridden with the `ALLSTAK_HOST` env var (self-hosted only).

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

## Links

- Documentation: https://docs.allstak.sa
- Dashboard: https://app.allstak.sa
- Source: https://github.com/AllStak/allstak-go

## License

MIT © AllStak