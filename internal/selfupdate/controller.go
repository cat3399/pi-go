package selfupdate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
)

type CheckedPlan struct {
	Plan
	ID string `json:"id"`
}

// Controller keeps the verified plan on the host. Frontends pass only an opaque
// ID to apply, never executable paths, release URLs or checksums of their own.
// It contains no Agent or remote application state.
type Controller struct {
	mu        sync.Mutex
	updater   *Updater
	plan      CheckedPlan
	installed Result
}

func NewController(updater *Updater) *Controller { return &Controller{updater: updater} }

func (c *Controller) Check(ctx context.Context, options Options) (CheckedPlan, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.plan = CheckedPlan{}
	if c.installed.Updated {
		return CheckedPlan{}, errors.New("application updated; restart it before checking again")
	}
	plan, err := c.updater.Check(ctx, options)
	if err != nil {
		return CheckedPlan{}, err
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return CheckedPlan{}, err
	}
	c.plan = CheckedPlan{Plan: plan, ID: hex.EncodeToString(id[:])}
	return c.plan, nil
}

func (c *Controller) Apply(ctx context.Context, id string) (Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if id == "" || id != c.plan.ID {
		return Result{}, errors.New("update check expired; check again before installing")
	}
	result, err := c.updater.Apply(ctx, c.plan.Plan)
	if err == nil {
		c.installed = result
		c.plan = CheckedPlan{}
	}
	return result, err
}

func (c *Controller) Status() Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.installed
}

// DownloadURL exposes only an asset selected by this host's latest check.
// The Android browser owns downloading it; this does not report an installation.
func (c *Controller) DownloadURL(id string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if id == "" || id != c.plan.ID || c.plan.Surface != "mobile" {
		return "", errors.New("update check expired; check again before downloading")
	}
	return c.plan.archiveURL, nil
}
