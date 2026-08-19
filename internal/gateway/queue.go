package gateway

import (
	"context"
	"sync"
	"time"
)

// requestQueue bounds the number of requests waiting for admission. It does
// not replace the configured rate/concurrency limits; it only gives briefly
// overloaded providers a small amount of backpressure.
type requestQueue struct {
	mu      sync.Mutex
	waiting int
	max     int
}

func newRequestQueue(max int) *requestQueue { return &requestQueue{max: max} }

func (q *requestQueue) depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.waiting
}

func (q *requestQueue) setMax(max int) {
	q.mu.Lock()
	q.max = max
	q.mu.Unlock()
}

func (q *requestQueue) enter() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.max <= 0 || q.waiting >= q.max {
		return false
	}
	q.waiting++
	return true
}

func (q *requestQueue) leave() {
	q.mu.Lock()
	if q.waiting > 0 {
		q.waiting--
	}
	q.mu.Unlock()
}

func (q *requestQueue) wait(ctx context.Context, timeout time.Duration, try func() (func(), bool)) (func(), bool, bool) {
	if release, ok := try(); ok {
		return release, true, false
	}
	if !q.enter() {
		return nil, false, false
	}
	defer q.leave()

	waitCtx := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		waitCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-waitCtx.Done():
			return nil, false, true
		case <-ticker.C:
			if release, ok := try(); ok {
				return release, true, true
			}
		}
	}
}
