// Package allstakchi provides an AllStak middleware for the Chi router.
//
// Chi uses stdlib http.Handler throughout, so this is a trivial wrapper
// around the base allstak.Middleware. It exists as its own package so
// customers can `import _ "github.com/AllStak/allstak-go/integrations/allstakchi"`
// without forcing everyone else to know about Chi.
package allstakchi

import (
	"net/http"

	allstak "github.com/AllStak/allstak-go"
)

// Middleware returns a Chi-compatible middleware that captures inbound
// HTTP requests, enriches the context with trace/user info, and recovers
// panics. It is a direct re-export of allstak.Middleware and is provided
// here purely for discoverability.
//
// Usage:
//
//	r := chi.NewRouter()
//	r.Use(allstakchi.Middleware(client))
func Middleware(client *allstak.Client) func(http.Handler) http.Handler {
	return allstak.Middleware(client)
}
