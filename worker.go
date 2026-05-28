package allstak

import (
	"context"
	"time"
)

// runSingleWorker drains a typed channel and posts each item individually
// to the given ingest path. Used for streams where the backend expects one
// event per POST (errors, logs).
//
// It is implemented as a free function (not a Client method) because Go
// does not support type parameters on methods. The Client argument gives
// us access to counters, config, and transport without introducing a new
// dependency shape.
func runSingleWorker[T any](c *Client, ch <-chan *T, path string) {
	defer c.wg.Done()
	for item := range ch {
		ctx, cancel := context.WithTimeout(context.Background(), c.cfg.RequestTimeout*2)
		if err := c.transport.send(ctx, path, item); err != nil {
			c.failed.Add(1)
			c.debugf("send %s failed: %v", path, err)
			// Outage/restart durability: persist the scrubbed payload so the
			// next init can replay it. Permanent (4xx) failures are not worth
			// keeping — drop them. Fail-open.
			c.persistOnFailure(path, item, err)
		} else {
			c.sent.Add(1)
		}
		cancel()
	}
}

// runBatchWorker accumulates items from a typed channel and flushes either
// when the batch fills up or when the flush interval elapses. The wrap
// callback converts the collected slice into whatever struct shape the
// backend expects (HTTPRequestBatch, DBQueryBatch, SpanBatch).
//
// When the channel is closed, any pending items are flushed before the
// worker exits so Close() is lossless for in-memory events.
func runBatchWorker[T any](c *Client, ch <-chan *T, path string, wrap func([]*T) any) {
	defer c.wg.Done()

	batch := make([]*T, 0, c.cfg.BatchSize)
	ticker := time.NewTicker(c.cfg.FlushInterval)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		// Copy the slice so resetting `batch` doesn't race the in-flight
		// send. Cheap because the slice holds pointers.
		snapshot := make([]*T, len(batch))
		copy(snapshot, batch)
		batch = batch[:0]

		ctx, cancel := context.WithTimeout(context.Background(), c.cfg.RequestTimeout*2)
		defer cancel()
		wrapped := wrap(snapshot)
		if err := c.transport.send(ctx, path, wrapped); err != nil {
			c.failed.Add(int64(len(snapshot)))
			c.debugf("batch send %s failed (%d items): %v", path, len(snapshot), err)
			// Persist the whole scrubbed batch so the next init can replay it.
			c.persistOnFailure(path, wrapped, err)
			return
		}
		c.sent.Add(int64(len(snapshot)))
	}

	for {
		select {
		case item, ok := <-ch:
			if !ok {
				// Channel closed — final flush and exit.
				flush()
				return
			}
			batch = append(batch, item)
			if len(batch) >= c.cfg.BatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}
