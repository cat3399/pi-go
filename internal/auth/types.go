package auth

import (
	"context"
	"runtime"
	"strings"
	"unicode"
	"unicode/utf8"
)

const DefaultMaxFileBytes int64 = 4 << 20

// Credential is the supported API-key projection. OAuth is intentionally not
// represented as a usable credential in v0.1, although opaque OAuth entries
// are retained exactly when another provider is written.
type Credential struct {
	Type string
	Key  string
	Env  map[string]string
}

type Info struct {
	ProviderID string
	Type       string
}

type Options struct {
	Path         string
	MaxFileBytes int64
	LockPoll     func(context.Context) error
}

// Store serializes all mutations of one auth.json. Separate Store instances
// are also serialized by the on-disk lock directory.
type Store struct {
	path         string
	maxFileBytes int64
	lockPoll     func(context.Context) error
	local        chan struct{}
	platform     string
	// beforeRename is a package-private fault-injection seam. Production leaves
	// it nil; tests use it to prove a failed replacement preserves old bytes.
	beforeRename func() error
}

func NewStore(options Options) (*Store, error) {
	if strings.TrimSpace(options.Path) == "" || !utf8.ValidString(options.Path) {
		return nil, failure(KindInvalid, "open store", "", nil)
	}
	max := options.MaxFileBytes
	if max == 0 {
		max = DefaultMaxFileBytes
	}
	if max < 2 || options.Path == "." {
		return nil, failure(KindInvalid, "open store", "", nil)
	}
	local := make(chan struct{}, 1)
	local <- struct{}{}
	return &Store{
		path: options.Path, maxFileBytes: max, lockPoll: options.LockPoll,
		local: local, platform: runtime.GOOS,
	}, nil
}

func (s *Store) Path() string { return s.path }

func (s *Store) acquireLocal(ctx context.Context, operation, provider string) (func(), error) {
	if cause := context.Cause(ctx); cause != nil {
		return nil, failure(KindCancelled, operation, provider, cause)
	}
	select {
	case <-ctx.Done():
		return nil, failure(KindCancelled, operation, provider, context.Cause(ctx))
	case <-s.local:
		return func() { s.local <- struct{}{} }, nil
	}
}

func validProviderID(provider string) bool {
	return utf8.ValidString(provider) && strings.TrimSpace(provider) == provider && provider != "" &&
		!strings.ContainsFunc(provider, unicode.IsControl)
}

func validAPIKey(key string) bool {
	return utf8.ValidString(key) && strings.TrimSpace(key) != "" && !strings.ContainsFunc(key, unicode.IsControl)
}

func cloneEnv(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	result := make(map[string]string, len(source))
	for name, value := range source {
		result[name] = value
	}
	return result
}
