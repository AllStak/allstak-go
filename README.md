# allstak-go

AllStak SDK for Go services. Captures errors, logs, inbound and outbound HTTP requests, spans, database telemetry, and cron heartbeats.

## Install

```bash
go get github.com/AllStak/allstak-go
```

## Setup

```go
package main

import (
	"context"
	"errors"
	"net/http"
	"os"

	allstak "github.com/AllStak/allstak-go"
)

func main() {
	client := allstak.New(allstak.Config{
		APIKey:      os.Getenv("ALLSTAK_API_KEY"),
		Environment: os.Getenv("APP_ENV"),
		Release:     os.Getenv("ALLSTAK_RELEASE"),
		ServiceName: "checkout-api",
	})
	defer client.Flush(context.Background())

	mux := http.NewServeMux()
	mux.HandleFunc("/checkout", func(w http.ResponseWriter, r *http.Request) {
		ctx, finish := client.StartSpan(r.Context(), "checkout.authorize")
		defer finish(nil)

		client.CaptureLog(allstak.LogPayload{
			Level:   "info",
			Message: "checkout started",
		})

		w.WriteHeader(http.StatusCreated)
	})

	client.CaptureException(context.Background(), errors.New("example error"))
	http.ListenAndServe(":3000", allstak.Middleware(client)(mux))
}
```

## Outbound HTTP

```go
httpClient := &http.Client{
	Transport: allstak.NewTransport(client, nil),
}
```

## Gin

```go
import allstakgin "github.com/AllStak/allstak-go/integrations/allstakgin"

router.Use(allstakgin.Middleware(client))
```

## Echo

```go
import allstakecho "github.com/AllStak/allstak-go/integrations/allstakecho"

e.Use(allstakecho.Middleware(client))
```

The Echo middleware recovers panics as fatal events, captures errors returned
from handlers, records the inbound request with the matched route template
(e.g. `/users/:id`), and propagates trace context to downstream/outbound calls.

## Configuration

| Field | Description |
| --- | --- |
| `APIKey` | Project API key. |
| `Host` | Optional ingest host override for self-hosted AllStak. |
| `Environment` | Deployment environment. |
| `Release` | App version or commit SHA. |
| `ServiceName` | Logical service name. |
| `RequestTimeout` | Per-request ingest timeout. |
| `MaxRetries` | Retry attempts for transient failures. |

## Privacy

The SDK redacts common sensitive headers and fields. Avoid putting secrets in custom metadata.

## Troubleshooting

- No events: confirm `ALLSTAK_API_KEY` is set and the client is reused for the process lifetime.
- Missing request correlation: keep `traceparent`, `baggage`, and `x-request-id` headers through proxies.
- Short-lived command: call `client.Flush(ctx)` before exit.

## License

MIT
