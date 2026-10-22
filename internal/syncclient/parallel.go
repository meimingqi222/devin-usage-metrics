package syncclient

import (
	"context"
	"sync"

	"github.com/meimingqi222/devin-usage-metrics/internal/syncdiag"
)

// transferLimit bounds object transfers across all concurrent calls, so
// devices imported in parallel share one budget. Blocks average a few KiB, so
// latency, not bandwidth, sets the pace.
const transferLimit = 8

var transferSlots = make(chan struct{}, transferLimit)

// Bound both network concurrency and simultaneous decoding memory. A failed
// transfer cancels siblings; callers must not expose partially assembled data.
func parallelTransfers(ctx context.Context, count int, fn func(context.Context, int) error) (result error) {
	progress := syncdiag.NewProgress(ctx, count)
	defer func() { progress.Finish(result) }()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	var once sync.Once
	var first error
	jobs := make(chan int)
	for range min(count, transferLimit) {
		wg.Go(func() {
			for index := range jobs {
				if ctx.Err() != nil {
					return
				}
				select {
				case transferSlots <- struct{}{}:
				case <-ctx.Done():
					return
				}
				err := fn(ctx, index)
				<-transferSlots
				if err != nil {
					once.Do(func() { first = err; cancel() })
					return
				}
				progress.Completed()
			}
		})
	}
send:
	for index := 0; index < count; index++ {
		select {
		case jobs <- index:
		case <-ctx.Done():
			break send
		}
	}
	close(jobs)
	wg.Wait()
	if first != nil {
		return first
	}
	return ctx.Err()
}
