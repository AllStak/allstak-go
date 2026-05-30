package allstakgorm

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	allstak "github.com/AllStak/allstak-go"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type orderRow struct {
	ID   int `gorm:"primaryKey"`
	Name string
}

func TestInstrumentCapturesTraceCorrelatedQueries(t *testing.T) {
	// The transport callback runs in the SDK background batch-worker
	// goroutine, so the captured slice must be guarded against the test
	// goroutine that reads it after Flush.
	var (
		mu      sync.Mutex
		batches []allstak.DBQueryBatch
	)
	client := allstak.NewWithTransport(allstak.Config{
		APIKey:        "ask_test",
		FlushInterval: time.Millisecond,
		BatchSize:     1,
	}, allstak.TransportFunc(func(_ context.Context, path string, payload any) error {
		if path == "/ingest/v1/db" {
			mu.Lock()
			batches = append(batches, payload.(allstak.DBQueryBatch))
			mu.Unlock()
		}
		return nil
	}))
	defer client.Close(context.Background())

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := Instrument(db, client, WithDatabaseName("testdb")); err != nil {
		t.Fatal(err)
	}

	traceID := strings.Repeat("a", 32)
	ctx := allstak.WithContextSpan(context.Background(), traceID, "bbbbbbbbbbbbbbbb", "")
	if err := db.WithContext(ctx).AutoMigrate(&orderRow{}); err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(ctx).Create(&orderRow{Name: "first"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := client.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(batches) == 0 {
		t.Fatal("expected captured db query batch")
	}
	found := false
	for _, batch := range batches {
		for _, query := range batch.Queries {
			if query.TraceID != "" {
				found = true
				if query.TraceID != traceID {
					t.Fatalf("trace id mismatch: %q", query.TraceID)
				}
				if query.SpanID != "bbbbbbbbbbbbbbbb" {
					t.Fatalf("span id mismatch: %q", query.SpanID)
				}
				if query.DatabaseName != "testdb" {
					t.Fatalf("database name mismatch: %q", query.DatabaseName)
				}
			}
		}
	}
	if !found {
		t.Fatalf("expected at least one trace-correlated query, got %#v", batches)
	}
}

func TestInstrumentEmitsDBBreadcrumbOnCapturedError(t *testing.T) {
	var (
		mu   sync.Mutex
		errs []allstak.ErrorPayload
	)
	client := allstak.NewWithTransport(allstak.Config{
		APIKey:        "ask_test",
		FlushInterval: time.Millisecond,
		BatchSize:     1,
	}, allstak.TransportFunc(func(_ context.Context, path string, payload any) error {
		if path == "/ingest/v1/errors" {
			if p, ok := payload.(*allstak.ErrorPayload); ok {
				mu.Lock()
				errs = append(errs, *p)
				mu.Unlock()
			}
		}
		return nil
	}))
	defer client.Close(context.Background())

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := Instrument(db, client); err != nil {
		t.Fatal(err)
	}

	// Install a request-scoped breadcrumb buffer so the GORM after-callback
	// records a crumb, then run a query and capture an error on the SAME ctx.
	ctx := allstak.WithBreadcrumbs(context.Background())
	if err := db.WithContext(ctx).AutoMigrate(&orderRow{}); err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(ctx).Create(&orderRow{Name: "x"}).Error; err != nil {
		t.Fatal(err)
	}
	client.CaptureException(ctx, errTest("downstream failure"))

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(errs)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(errs) == 0 {
		t.Fatal("no error captured")
	}
	foundDB := false
	for _, bc := range errs[0].Breadcrumbs {
		if bc.Type == "query" && bc.Category == "db.query" {
			foundDB = true
		}
	}
	if !foundDB {
		t.Fatalf("expected a db.query breadcrumb on the captured error, got %#v", errs[0].Breadcrumbs)
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }
