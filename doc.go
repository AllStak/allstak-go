// Package allstak is the official Go SDK for AllStak, an all-in-one
// observability platform that captures errors, logs, HTTP requests,
// database queries, traces, and cron jobs from your Go services.
//
// # Quick start
//
//	client := allstak.New(allstak.Config{
//	    APIKey:      "ask_live_xxx",
//	    Environment: "production",
//	    Release:     "v1.2.3",
//	    ServiceName: "billing-api",
//	})
//	defer client.Close(context.Background())
//
//	handler := allstak.Middleware(client)(myHandler)
//	http.ListenAndServe(":8080", handler)
//
// # Integrations
//
//   - [Middleware] — net/http and Chi inbound HTTP capture.
//   - [NewTransport] — outbound http.Client capture.
//   - Nested module github.com/AllStak/allstak-go/integrations/allstakgorm
//     for GORM database instrumentation.
//   - Nested module github.com/AllStak/allstak-go/integrations/allstakgin
//     for Gin middleware.
//   - Package github.com/AllStak/allstak-go/integrations/allstakcron for
//     robfig/cron-compatible job wrappers.
//
// # Thread safety
//
// [Client] is safe for concurrent use. Create one at program start and
// reuse it for the process lifetime. Always call [Client.Close] or
// [Client.Flush] before exit so buffered events are not lost.
//
// # Host configuration
//
// The SDK targets a static production ingest host by default. For
// self-hosted deployments or local validation, set the ALLSTAK_HOST
// environment variable. There is deliberately no Host field on
// [Config] — customers should never have to know which URL their
// events go to.
package allstak
