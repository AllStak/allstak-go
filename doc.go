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
// # Zero-config start
//
// [InitFromEnv] builds a client purely from ALLSTAK_* environment variables and
// installs it as the package default, so the package-level helpers
// ([Go], [CaptureException], [HTTPClient], [Close]) work with no wiring:
//
//	func main() {
//	    allstak.InitFromEnv()
//	    defer allstak.Close(context.Background())
//	}
//
// # Automatic breadcrumbs
//
// After the middleware is registered, the inbound/outbound HTTP layers, the
// GORM callback, and the structured-log helpers automatically record a
// request-scoped breadcrumb trail. Any error captured during the request
// carries that trail with no per-call code. Use [Client.AddBreadcrumb] to add
// custom crumbs and [WithBreadcrumbs] to accrue a trail on a context you manage.
//
// # Goroutine safety
//
// [Client.SafeGo] (and package-level [Go]) run a function in a goroutine with a
// deferred panic guard that captures the panic as a fatal error and re-panics,
// so background-goroutine crashes the inbound middleware can never see are still
// reported. Wrap worker pools and errgroup goroutines with it.
//
// # Integrations
//
//   - [Middleware] — net/http and Chi inbound HTTP capture.
//   - [NewTransport] / [Client.HTTPClient] — outbound http.Client capture.
//   - Nested module github.com/AllStak/allstak-go/integrations/allstakgorm
//     for GORM database instrumentation.
//   - Nested module github.com/AllStak/allstak-go/integrations/allstakgin
//     for Gin middleware.
//   - Nested module github.com/AllStak/allstak-go/integrations/allstakecho
//     for Echo middleware.
//   - Package github.com/AllStak/allstak-go/integrations/allstakcron for
//     robfig/cron-compatible job wrappers.
//   - Logging bridges: github.com/AllStak/allstak-go/integrations/allstakslog
//     (log/slog), .../allstakzap (Uber zap), and .../allstaklogrus (logrus) —
//     ship structured logs and promote error-level records with an attached
//     error to the Errors stream.
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
