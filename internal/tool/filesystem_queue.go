package tool

import (
	"context"
	"sync"
)

type mutationQueue struct {
	mu           sync.Mutex
	tails        map[string]chan struct{}
	pendingNodes int
	settling     int
}

func newMutationQueue() *mutationQueue { return &mutationQueue{tails: make(map[string]chan struct{})} }

// with serializes only aliases of the same target. Cancellation while queued
// does not start the operation; cancellation after it starts is the operation's
// responsibility so a write cannot outlive its lock.
func (q *mutationQueue) with(ctx context.Context, key string, fn func() error) error {
	q.mu.Lock()
	previous := q.tails[key]
	current := make(chan struct{})
	q.tails[key] = current
	q.pendingNodes++
	q.mu.Unlock()
	defer q.release(key, current)
	if previous != nil {
		select {
		case <-previous:
		case <-ctx.Done():
			// This cancelled node is still the predecessor observed by any
			// operation registered after it. Settle synchronously behind its own
			// predecessor so cancellation leaves neither a relay goroutine nor a
			// residual barrier after the caller returns.
			q.changeSettling(1)
			<-previous
			q.changeSettling(-1)
			return context.Cause(ctx)
		}
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	return fn()
}

func (q *mutationQueue) release(key string, current chan struct{}) {
	q.mu.Lock()
	if q.tails[key] == current {
		delete(q.tails, key)
	}
	q.pendingNodes--
	close(current)
	q.mu.Unlock()
}

func (q *mutationQueue) changeSettling(delta int) {
	q.mu.Lock()
	q.settling += delta
	q.mu.Unlock()
}

// pendingState is a deterministic lifecycle seam for package tests. It reports
// every registered node, not only the per-key tail retained in the map.
func (q *mutationQueue) pendingState() (nodes, keys, settling int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.pendingNodes, len(q.tails), q.settling
}
