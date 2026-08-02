package tool

import (
	"context"
	"sync"
)

type mutationQueue struct {
	mu    sync.Mutex
	tails map[string]chan struct{}
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
	q.mu.Unlock()
	if previous != nil {
		select {
		case <-previous:
		case <-ctx.Done():
			q.release(key, current)
			return context.Cause(ctx)
		}
	}
	defer q.release(key, current)
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
	close(current)
	q.mu.Unlock()
}
