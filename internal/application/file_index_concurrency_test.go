package application

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFileIndexSlowProjectDoesNotBlockCachedProject(t *testing.T) {
	service := newFileTestService(t, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	load := func(ctx context.Context, cwd string) (fileIndexListing, error) {
		if cwd == "slow" {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return fileIndexListing{}, ctx.Err()
			}
		}
		return fileIndexListing{files: []string{cwd + ".txt"}}, nil
	}
	if _, err := service.cachedFileIndex(ctx, "fast", load); err != nil {
		t.Fatal(err)
	}
	slow := make(chan error, 1)
	go func() { _, err := service.cachedFileIndex(ctx, "slow", load); slow <- err }()
	<-entered
	fast := make(chan error, 1)
	go func() { _, err := service.cachedFileIndex(ctx, "fast", load); fast <- err }()
	select {
	case err := <-fast:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("cached project waited for another project's filesystem read")
	}
	unblock()
	if err := <-slow; err != nil {
		t.Fatal(err)
	}
}

func TestFileIndexCoalescesRebuildsAndWaiterCanCancel(t *testing.T) {
	service := newFileTestService(t, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var calls atomic.Int32
	load := func(ctx context.Context, _ string) (fileIndexListing, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-release:
			return fileIndexListing{files: []string{"shared.txt"}}, nil
		case <-ctx.Done():
			return fileIndexListing{}, ctx.Err()
		}
	}
	done := make(chan error, 9)
	go func() { _, err := service.cachedFileIndex(ctx, "shared", load); done <- err }()
	<-entered
	waitCtx, stopWaiting := context.WithCancel(ctx)
	waiter := make(chan error, 1)
	go func() { _, err := service.cachedFileIndex(waitCtx, "shared", load); waiter <- err }()
	stopWaiting()
	if err := <-waiter; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter: %v", err)
	}
	for range 8 {
		go func() { _, err := service.cachedFileIndex(ctx, "shared", load); done <- err }()
	}
	unblock()
	for range 9 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("rebuilt one directory %d times", got)
	}
}

func TestFileIndexCancelledLoadCanBeRetried(t *testing.T) {
	service := newFileTestService(t, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	var calls atomic.Int32
	load := func(ctx context.Context, _ string) (fileIndexListing, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-ctx.Done()
			return fileIndexListing{}, ctx.Err()
		}
		return fileIndexListing{files: []string{"retry.txt"}}, nil
	}
	first := make(chan error, 1)
	go func() { _, err := service.cachedFileIndex(ctx, "shared", load); first <- err }()
	<-entered
	retryCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	second := make(chan error, 1)
	go func() { _, err := service.cachedFileIndex(retryCtx, "shared", load); second <- err }()
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled loader: %v", err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("load calls = %d", calls.Load())
	}
}
