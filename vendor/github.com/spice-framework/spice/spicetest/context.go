package spicetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/spice-framework/spice/lifecycle"
)

// Application is the lifecycle surface implemented by every generated Spice
// application. The generic Context preserves the concrete application type so
// tests can use generated APIs such as Components without reflection or
// string-based bean lookup.
type Application interface {
	Start(context.Context) error
	Stop(context.Context) error
	State() lifecycle.State
}

// Factory constructs one concrete generated application.
type Factory[A Application] func(context.Context) (A, error)

// ContextOptions controls bounded startup and shutdown for an application test
// context. SkipStart keeps the application constructed for focused slices that
// must not run production lifecycle hooks.
type ContextOptions struct {
	StartupTimeout  time.Duration
	ShutdownTimeout time.Duration
	SkipStart       bool
}

// Context owns one concrete generated application for the duration of a test.
// It starts lifecycle hooks by default and stops the application exactly once.
type Context[A Application] struct {
	application     A
	shutdownTimeout time.Duration
	closeOnce       sync.Once
	closeErr        error
}

// NewContext constructs and, unless SkipStart is set, starts one generated
// application. Factory failures are joined with best-effort cleanup when the
// factory returned a constructed application.
func NewContext[A Application](
	ctx context.Context,
	factory Factory[A],
	options ContextOptions,
) (*Context[A], error) {
	normalized, err := normalizeContextOptions(options)
	if err != nil {
		return nil, err
	}
	switch {
	case ctx == nil:
		return nil, errors.New("construct application test context: context is nil")
	case factory == nil:
		return nil, errors.New("construct application test context: factory is nil")
	default:
		if cause := context.Cause(ctx); cause != nil {
			return nil, fmt.Errorf("construct application test context: %w", cause)
		}
	}

	application, err := factory(ctx)
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("construct application test context: %w", err),
			stopReturnedApplication(application, normalized.ShutdownTimeout),
		)
	}
	state := application.State()
	if state != lifecycle.StateConstructed && state != lifecycle.StateReady {
		return nil, fmt.Errorf(
			"construct application test context: application state %s is not usable",
			state,
		)
	}
	testContext := &Context[A]{
		application:     application,
		shutdownTimeout: normalized.ShutdownTimeout,
	}
	if normalized.SkipStart || state == lifecycle.StateReady {
		return testContext, nil
	}

	startupContext, cancel := context.WithTimeout(ctx, normalized.StartupTimeout)
	defer cancel()
	if err := application.Start(startupContext); err != nil {
		return nil, fmt.Errorf("start application test context: %w", err)
	}
	if state := application.State(); state != lifecycle.StateReady {
		return nil, errors.Join(
			fmt.Errorf(
				"start application test context: application state %s is not ready",
				state,
			),
			stopReturnedApplication(application, normalized.ShutdownTimeout),
		)
	}
	return testContext, nil
}

// Application returns the concrete generated application.
func (testContext *Context[A]) Application() A {
	if testContext == nil {
		var zero A
		return zero
	}
	return testContext.application
}

// State returns the generated application's lifecycle state.
func (testContext *Context[A]) State() lifecycle.State {
	if testContext == nil {
		return lifecycle.StateInvalid
	}
	return testContext.application.State()
}

// Close stops the generated application with a fresh bounded background
// context. It is safe for concurrent and repeated calls.
func (testContext *Context[A]) Close() error {
	if testContext == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(
		context.Background(),
		testContext.shutdownTimeout,
	)
	defer cancel()
	return testContext.CloseContext(ctx)
}

// CloseContext stops the generated application exactly once. The configured
// shutdown timeout remains an upper bound on the caller-owned context.
func (testContext *Context[A]) CloseContext(ctx context.Context) error {
	if testContext == nil {
		return nil
	}
	if ctx == nil {
		return errors.New("close application test context: context is nil")
	}
	testContext.closeOnce.Do(func() {
		shutdownContext, cancel := context.WithTimeout(
			ctx,
			testContext.shutdownTimeout,
		)
		defer cancel()
		if err := testContext.application.Stop(shutdownContext); err != nil {
			testContext.closeErr = fmt.Errorf(
				"close application test context: %w",
				err,
			)
		}
	})
	return testContext.closeErr
}

func normalizeContextOptions(options ContextOptions) (ContextOptions, error) {
	if options.StartupTimeout == 0 {
		options.StartupTimeout = defaultTestTimeout
	}
	if options.ShutdownTimeout == 0 {
		options.ShutdownTimeout = defaultTestTimeout
	}
	if options.StartupTimeout < 0 || options.StartupTimeout > maxTestTimeout {
		return ContextOptions{}, errors.New(
			"construct application test context: startup timeout must be between 1ns and 1m",
		)
	}
	if options.ShutdownTimeout < 0 || options.ShutdownTimeout > maxTestTimeout {
		return ContextOptions{}, errors.New(
			"construct application test context: shutdown timeout must be between 1ns and 1m",
		)
	}
	return options, nil
}

func stopReturnedApplication[A Application](
	application A,
	timeout time.Duration,
) error {
	state := application.State()
	if state != lifecycle.StateConstructed && state != lifecycle.StateReady {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := application.Stop(ctx); err != nil {
		return fmt.Errorf("stop returned application: %w", err)
	}
	return nil
}
