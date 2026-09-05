// Package installation prepares durable product data and the local documentation
// installed with a binary. It has no Agent, Session or transport state.
package installation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Stable kernel-locked sidecars survive crashes without a stale-lock heuristic.
func lock(ctx context.Context, path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			_ = file.Close()
			return nil, context.Cause(ctx)
		}
		acquired, err := tryLock(file)
		if err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("lock installation: %w", err)
		}
		if acquired {
			return func() { unlock(file); _ = file.Close() }, nil
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = file.Close()
			return nil, context.Cause(ctx)
		case <-timer.C:
		}
	}
}
