package resource

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestTrustLeaseContentionWaitsForOwnerAndObeysContext(t *testing.T) {
	agent := t.TempDir()
	owner, err := NewTrustStore(agent)
	if err != nil {
		t.Fatal(err)
	}
	owner.poll = 5 * time.Millisecond
	lease, err := owner.acquirePersistentLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	waiter, err := NewTrustStore(agent)
	if err != nil {
		t.Fatal(err)
	}
	waiter.poll = 5 * time.Millisecond
	completed := make(chan error, 1)
	go func() { completed <- waiter.Set(context.Background(), filepath.Join(agent, "project"), true) }()
	// This exceeds the old fixed 10 x 20ms retry budget. A live lease should
	// keep the contender waiting instead of producing a load-dependent error.
	time.Sleep(250 * time.Millisecond)
	select {
	case err := <-completed:
		t.Fatalf("contender returned while lease was live: %v", err)
	default:
	}
	if err := lease.release(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-completed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("contender did not acquire released lease")
	}

	lease, err = owner.acquirePersistentLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.release()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := waiter.Set(ctx, filepath.Join(agent, "other"), true); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline wait error = %v", err)
	}
}

func TestTrustLeaseRecoversStaleOwner(t *testing.T) {
	agent := t.TempDir()
	store, err := NewTrustStore(agent)
	if err != nil {
		t.Fatal(err)
	}
	store.poll = 2 * time.Millisecond
	store.leaseTTL = 20 * time.Millisecond
	store.heartbeat = 5 * time.Millisecond
	lockPath := store.Path() + ".lock"
	if err := os.Mkdir(lockPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeExclusivePrivateFile(filepath.Join(lockPath, trustLockOwnerFile), []byte("abandoned")); err != nil {
		t.Fatal(err)
	}
	if err := writeExclusivePrivateFile(filepath.Join(lockPath, trustLockHeartbeatFile), []byte("abandoned")); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Second)
	if err := os.Chtimes(filepath.Join(lockPath, trustLockHeartbeatFile), old, old); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(context.Background(), filepath.Join(agent, "project"), true); err != nil {
		t.Fatal(err)
	}
	if trusted, known, err := store.Get(context.Background(), filepath.Join(agent, "project")); err != nil || !known || !trusted {
		t.Fatalf("recovered decision = (%v, %v, %v)", trusted, known, err)
	}
}

func TestTrustLeaseRecoversStaleLegacyShapesWithoutRefreshingThem(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{
			name: "empty-proper-lockfile-directory",
			setup: func(t *testing.T, lockPath string) {
				t.Helper()
				if err := os.Mkdir(lockPath, 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "owner-only-without-heartbeat",
			setup: func(t *testing.T, lockPath string) {
				t.Helper()
				if err := os.Mkdir(lockPath, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := writeExclusivePrivateFile(filepath.Join(lockPath, trustLockOwnerFile), []byte("abandoned-owner")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "previous-transition-format",
			setup: func(t *testing.T, lockPath string) {
				t.Helper()
				if err := os.Mkdir(lockPath, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := writeExclusivePrivateFile(filepath.Join(lockPath, trustLockTransition), []byte("old-claim")); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			agent := t.TempDir()
			store, err := NewTrustStore(agent)
			if err != nil {
				t.Fatal(err)
			}
			store.poll = 2 * time.Millisecond
			store.leaseTTL = 20 * time.Millisecond
			store.heartbeat = 5 * time.Millisecond
			lockPath := store.Path() + ".lock"
			test.setup(t, lockPath)
			old := time.Now().Add(-time.Second)
			if err := os.Chtimes(lockPath, old, old); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(lockPath)
			if err != nil {
				t.Fatal(err)
			}
			stale, _, err := store.leaseIsStale(lockPath)
			if err != nil || !stale {
				t.Fatalf("first observation = stale %t, err %v", stale, err)
			}
			after, err := os.Stat(lockPath)
			if err != nil {
				t.Fatal(err)
			}
			if !after.ModTime().Equal(before.ModTime()) {
				t.Fatalf("read-only stale observation refreshed directory: before %v after %v", before.ModTime(), after.ModTime())
			}
			project := filepath.Join(agent, "project")
			if err := store.Set(context.Background(), project, true); err != nil {
				t.Fatal(err)
			}
			if trusted, known, err := store.Get(context.Background(), project); err != nil || !known || !trusted {
				t.Fatalf("recovered legacy decision = (%v, %v, %v)", trusted, known, err)
			}
		})
	}
}

func TestTrustLeaseRetiredClaimFromCrashedRecoveryNeverBlocksActivePath(t *testing.T) {
	agent := t.TempDir()
	store, err := NewTrustStore(agent)
	if err != nil {
		t.Fatal(err)
	}
	store.poll = 2 * time.Millisecond
	store.leaseTTL = 20 * time.Millisecond
	store.heartbeat = 5 * time.Millisecond
	crashedClaim := store.Path() + ".lock.stale-crashed-claimant"
	if err := os.Mkdir(crashedClaim, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeExclusivePrivateFile(filepath.Join(crashedClaim, "unknown-legacy-entry"), []byte("keeps retired claim non-empty")); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(context.Background(), filepath.Join(agent, "project"), true); err != nil {
		t.Fatalf("retired sibling blocked active lock: %v", err)
	}
	if _, err := os.Stat(crashedClaim); err != nil {
		t.Fatalf("acquisition unexpectedly depended on cleaning retired claim: %v", err)
	}
}

func TestTrustLeaseHeartbeatPreventsRecoveryOfOldDirectory(t *testing.T) {
	agent := t.TempDir()
	owner, err := NewTrustStore(agent)
	if err != nil {
		t.Fatal(err)
	}
	owner.poll = 2 * time.Millisecond
	owner.leaseTTL = 200 * time.Millisecond
	owner.heartbeat = 5 * time.Millisecond
	lease, err := owner.acquirePersistentLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.release()
	old := time.Now().Add(-time.Second)
	if err := os.Chtimes(lease.path, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(lease.path, trustLockOwnerFile), old, old); err != nil {
		t.Fatal(err)
	}

	waiter, err := NewTrustStore(agent)
	if err != nil {
		t.Fatal(err)
	}
	waiter.poll = 2 * time.Millisecond
	waiter.leaseTTL = owner.leaseTTL
	waiter.heartbeat = owner.heartbeat
	ctx, cancel := context.WithTimeout(context.Background(), 320*time.Millisecond)
	defer cancel()
	if _, err := waiter.acquirePersistentLock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("live heartbeat was stolen: %v", err)
	}
	if err := lease.ensureOwned(); err != nil {
		t.Fatalf("owner lost live lease: %v", err)
	}
}

func TestTrustLeaseReleaseCannotRemoveReplacementToken(t *testing.T) {
	store, err := NewTrustStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.acquirePersistentLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	displaced := lease.path + ".displaced"
	if err := os.Rename(lease.path, displaced); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(lease.path, 0o700); err != nil {
		t.Fatal(err)
	}
	const replacement = "replacement-owner"
	if err := writeExclusivePrivateFile(filepath.Join(lease.path, trustLockOwnerFile), []byte(replacement)); err != nil {
		t.Fatal(err)
	}
	if err := writeExclusivePrivateFile(filepath.Join(lease.path, trustLockHeartbeatFile), []byte(replacement)); err != nil {
		t.Fatal(err)
	}
	if err := lease.release(); !errors.Is(err, ErrTrustStore) {
		t.Fatalf("replacement release error = %v", err)
	}
	owner, err := os.ReadFile(filepath.Join(lease.path, trustLockOwnerFile))
	if err != nil || string(owner) != replacement {
		t.Fatalf("replacement lease was removed: owner=%q, err=%v", owner, err)
	}
	cleanupTrustLeaseDirectory(displaced)
	cleanupTrustLeaseDirectory(lease.path)
}

func TestTrustLeaseHeartbeatRetriesTransientRefreshErrors(t *testing.T) {
	store, err := NewTrustStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store.leaseTTL = 300 * time.Millisecond
	store.heartbeat = 10 * time.Millisecond
	var attempts atomic.Int32
	store.chtimes = func(path string, atime, mtime time.Time) error {
		if attempts.Add(1) <= 2 {
			return errors.New("transient chtimes failure")
		}
		return os.Chtimes(path, atime, mtime)
	}
	lease, err := store.acquirePersistentLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	waitForTrustLease(t, time.Second, func() bool { return attempts.Load() >= 3 })
	if err := lease.ensureOwned(); err != nil {
		t.Fatalf("transient refresh permanently compromised lease: %v", err)
	}
	if err := lease.release(); err != nil {
		t.Fatalf("recovered lease release = %v", err)
	}
}

func TestTrustLeaseDeletedHeartbeatIrreversiblyRejectsCommit(t *testing.T) {
	store, err := NewTrustStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store.leaseTTL = 100 * time.Millisecond
	store.heartbeat = 5 * time.Millisecond
	lease, err := store.acquirePersistentLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(lease.path, trustLockHeartbeatFile)); err != nil {
		t.Fatal(err)
	}
	waitForTrustLease(t, time.Second, func() bool { return lease.compromiseError() != nil })
	if err := store.atomic([]byte("{}\n"), lease); !errors.Is(err, ErrTrustStore) {
		t.Fatalf("commit with deleted heartbeat = %v", err)
	}
	if _, err := os.Stat(store.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("compromised commit published trust file: %v", err)
	}
	if err := lease.release(); !errors.Is(err, ErrTrustStore) {
		t.Fatalf("compromised release error = %v", err)
	}
	if _, err := os.Stat(lease.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned compromised lock was not retired: %v", err)
	}
}

func TestTrustLeasePersistentRefreshErrorExpiresAndRejectsCommit(t *testing.T) {
	store, err := NewTrustStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store.leaseTTL = 50 * time.Millisecond
	store.heartbeat = 5 * time.Millisecond
	store.chtimes = func(string, time.Time, time.Time) error { return errors.New("persistent chtimes failure") }
	lease, err := store.acquirePersistentLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	waitForTrustLease(t, time.Second, func() bool { return lease.compromiseError() != nil })
	if err := store.atomic([]byte("{}\n"), lease); !errors.Is(err, ErrTrustStore) {
		t.Fatalf("commit after heartbeat expiry = %v", err)
	}
	if err := lease.release(); !errors.Is(err, ErrTrustStore) {
		t.Fatalf("expired lease release error = %v", err)
	}
}

func TestTrustLeaseReleaseReportsCleanupFailureAfterRetiringActivePath(t *testing.T) {
	store, err := NewTrustStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.acquirePersistentLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := writeExclusivePrivateFile(filepath.Join(lease.path, "unknown-entry"), []byte("preserve")); err != nil {
		t.Fatal(err)
	}
	if err := lease.release(); !errors.Is(err, ErrTrustStore) {
		t.Fatalf("cleanup failure was swallowed: %v", err)
	}
	if _, err := os.Stat(lease.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("active path remained blocked after cleanup error: %v", err)
	}
}

func waitForTrustLease(t *testing.T, timeout time.Duration, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for lease state")
		}
		time.Sleep(time.Millisecond)
	}
}
