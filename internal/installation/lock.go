// Package installation prepares durable product data and local documentation.
package installation

import (
	"context"
	"github.com/cat3399/pi-go/internal/filelock"
)

func lock(ctx context.Context, path string) (func(), error) {
	return filelock.Acquire(ctx, path)
}
