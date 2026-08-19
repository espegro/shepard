package gateway

import (
	"context"
	"testing"
	"time"
)

func TestRequestQueueWaitsForAdmission(t *testing.T) {
	q := newRequestQueue(1)
	attempts := 0
	release, ok, queued := q.wait(context.Background(), time.Second, func() (func(), bool) {
		attempts++
		if attempts == 1 {
			return nil, false
		}
		return func() {}, true
	})
	if !ok || !queued || release == nil {
		t.Fatal("request did not pass through queue")
	}
	release()
}

func TestRequestQueueTimesOut(t *testing.T) {
	q := newRequestQueue(1)
	_, ok, queued := q.wait(context.Background(), 5*time.Millisecond, func() (func(), bool) {
		return nil, false
	})
	if ok || !queued {
		t.Fatal("expected queued request to time out")
	}
}
