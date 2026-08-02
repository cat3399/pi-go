package app

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
)

type signalSpec struct {
	signal   os.Signal
	exitCode int
}

type signalController struct {
	cancel context.CancelCauseFunc
	input  chan os.Signal
	stop   chan struct{}
	done   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	caught *signalSpec
}

func startSignalController(parent context.Context) (context.Context, *signalController) {
	ctx, cancel := context.WithCancelCause(parent)
	controller := &signalController{
		cancel: cancel,
		input:  make(chan os.Signal, 1),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	specs := platformSignalSpecs()
	values := make([]os.Signal, len(specs))
	for index, spec := range specs {
		values[index] = spec.signal
	}
	signal.Notify(controller.input, values...)
	go func() {
		defer close(controller.done)
		select {
		case received := <-controller.input:
			controller.record(received, specs)
		case <-controller.stop:
		}
	}()
	return ctx, controller
}

func (c *signalController) record(received os.Signal, specs []signalSpec) {
	for index := range specs {
		if received == specs[index].signal {
			shouldCancel := false
			c.mu.Lock()
			if c.caught == nil {
				captured := specs[index]
				c.caught = &captured
				shouldCancel = true
			}
			c.mu.Unlock()
			if shouldCancel {
				c.cancel(fmt.Errorf("application received %s", received))
			}
			return
		}
	}
}

func (c *signalController) stopAndExitCode() (int, bool) {
	if c == nil {
		return 0, false
	}
	specs := platformSignalSpecs()
	c.once.Do(func() {
		signal.Stop(c.input)
		select {
		case received := <-c.input:
			c.record(received, specs)
		default:
		}
		close(c.stop)
		<-c.done
		c.cancel(context.Canceled)
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.caught == nil {
		return 0, false
	}
	return c.caught.exitCode, true
}
