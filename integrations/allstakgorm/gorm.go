// Package allstakgorm instruments gorm.io/gorm with AllStak database
// telemetry. Every query executed through an instrumented *gorm.DB is
// captured as a DBQueryItem (normalized SQL, duration, status, rows
// affected) and correlated with the active trace via the request context.
//
// Usage:
//
//	db, _ := gorm.Open(sqlite.Open("app.db"), &gorm.Config{})
//	allstakgorm.Instrument(db, client)
//
// After Instrument is called, any subsequent `db.Create(...)`,
// `db.Where(...).Find(...)`, etc. emits a DB event. Multi-statement
// operations (like auto-migrations) emit one event per SQL statement.
package allstakgorm

import (
	"time"

	allstak "github.com/AllStak/allstak-go"
	"gorm.io/gorm"
)

const (
	// pluginName is the internal identifier GORM uses to install/remove
	// plugins. We re-use the same constant so tests can toggle us on and
	// off cleanly.
	pluginName = "allstak:gorm"

	// callbackKey stamps the start time on each GORM statement context
	// so the After callback can compute duration.
	callbackKey = "allstak:start"
)

// plugin implements gorm.Plugin so Instrument can call db.Use(plugin{...}).
type plugin struct {
	client       *allstak.Client
	databaseName string
	databaseType string
}

// Instrument registers AllStak callbacks on the given *gorm.DB. Calling
// it more than once on the same connection is safe — GORM will replace
// existing callbacks at the same names.
//
// databaseType should be one of "sqlite", "postgres", "mysql", etc.
// If empty it is inferred from the underlying Dialector.Name().
func Instrument(db *gorm.DB, client *allstak.Client, opts ...Option) error {
	p := &plugin{client: client}
	for _, o := range opts {
		o(p)
	}
	if p.databaseType == "" && db.Dialector != nil {
		p.databaseType = db.Dialector.Name()
	}
	return db.Use(p)
}

// Option is a functional config for the GORM plugin.
type Option func(*plugin)

// WithDatabaseName attaches a database name tag to emitted events.
// Useful when one process talks to multiple logical databases.
func WithDatabaseName(name string) Option {
	return func(p *plugin) { p.databaseName = name }
}

// WithDatabaseType overrides the database type label. Only needed if the
// auto-detection from the Dialector is wrong or unavailable.
func WithDatabaseType(dbType string) Option {
	return func(p *plugin) { p.databaseType = dbType }
}

// Name implements gorm.Plugin.
func (p *plugin) Name() string { return pluginName }

// Initialize implements gorm.Plugin. It attaches before/after callbacks
// to every hook that can emit SQL: Create, Query, Update, Delete, Row,
// and Raw. before records the start time; after builds the event.
func (p *plugin) Initialize(db *gorm.DB) error {
	// We register via a slice of closures rather than copy-pasting 12
	// nearly-identical lines. Any registration error aborts the install.
	for _, register := range []func() error{
		func() error {
			return db.Callback().Create().Before("gorm:create").Register("allstak:before_create", p.before)
		},
		func() error {
			return db.Callback().Create().After("gorm:create").Register("allstak:after_create", p.after)
		},
		func() error {
			return db.Callback().Query().Before("gorm:query").Register("allstak:before_query", p.before)
		},
		func() error {
			return db.Callback().Query().After("gorm:query").Register("allstak:after_query", p.after)
		},
		func() error {
			return db.Callback().Update().Before("gorm:update").Register("allstak:before_update", p.before)
		},
		func() error {
			return db.Callback().Update().After("gorm:update").Register("allstak:after_update", p.after)
		},
		func() error {
			return db.Callback().Delete().Before("gorm:delete").Register("allstak:before_delete", p.before)
		},
		func() error {
			return db.Callback().Delete().After("gorm:delete").Register("allstak:after_delete", p.after)
		},
		func() error {
			return db.Callback().Row().Before("gorm:row").Register("allstak:before_row", p.before)
		},
		func() error {
			return db.Callback().Row().After("gorm:row").Register("allstak:after_row", p.after)
		},
		func() error {
			return db.Callback().Raw().Before("gorm:raw").Register("allstak:before_raw", p.before)
		},
		func() error {
			return db.Callback().Raw().After("gorm:raw").Register("allstak:after_raw", p.after)
		},
	} {
		if err := register(); err != nil {
			return err
		}
	}
	return nil
}

// before records the query start timestamp on the statement.
// GORM always initializes Statement.Settings so no nil guard is needed.
func (p *plugin) before(db *gorm.DB) {
	if db.Statement == nil {
		return
	}
	db.Statement.Settings.Store(callbackKey, time.Now())
}

// after computes the duration, builds a DBQueryItem, and captures it.
func (p *plugin) after(db *gorm.DB) {
	if db.Statement == nil {
		return
	}
	startV, ok := db.Statement.Settings.Load(callbackKey)
	if !ok {
		// The before callback didn't fire for this statement — emit a
		// best-effort event with 0 duration rather than losing the SQL.
		startV = time.Now()
	}
	start, ok := startV.(time.Time)
	if !ok {
		return
	}
	duration := time.Since(start)

	// Capture the raw rendered SQL for normalization. We do not preserve
	// bound values — NormalizeSQL will replace them with "?" anyway, and
	// keeping them out of telemetry is the right default for PII.
	sql := db.Statement.SQL.String()
	if sql == "" {
		return
	}
	normalized := allstak.NormalizeSQL(sql)

	status := "success"
	errMsg := ""
	if db.Error != nil {
		status = "error"
		errMsg = db.Error.Error()
	}

	item := allstak.DBQueryItem{
		NormalizedQuery: normalized,
		QueryHash:       allstak.HashSQL(normalized),
		QueryType:       allstak.ClassifySQL(normalized),
		DurationMs:      duration.Milliseconds(),
		TimestampMillis: start.UnixMilli(),
		Status:          status,
		ErrorMessage:    errMsg,
		DatabaseName:    p.databaseName,
		DatabaseType:    p.databaseType,
		RowsAffected:    int(db.Statement.RowsAffected),
	}

	// Correlate with the active trace if the statement was built from a
	// context-bearing query (e.g. db.WithContext(ctx).Find(...)), and drop a
	// breadcrumb on the request-scoped trail so an error captured later in the
	// same request shows the query (normalized — no bound values) that ran
	// just before it.
	if db.Statement.Context != nil {
		if tid, sid := allstak.TraceFromContext(db.Statement.Context); tid != "" {
			item.TraceID = tid
			item.SpanID = sid
		}
		allstak.AddDBBreadcrumb(db.Statement.Context, item.QueryType, item.NormalizedQuery, item.Status, item.DurationMs, item.RowsAffected)
	}

	p.client.CaptureDBQuery(item)
}
