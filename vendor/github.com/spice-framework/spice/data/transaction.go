// Package data provides standard-library-first SQL transaction contracts for
// generated Spice applications and application-owned repositories.
package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"
)

// ErrPanicked identifies an observed transaction callback panic. Within rolls
// the transaction back, reports this error to observers, and re-panics with the
// original value.
var ErrPanicked = errors.New("transaction callback panicked")

// Executor is the common database/sql surface implemented by *sql.DB and
// *sql.Tx. Repositories can depend on this interface and remain usable inside
// and outside a managed transaction.
type Executor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	PrepareContext(context.Context, string) (*sql.Stmt, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// Definition is compiler-owned metadata for one transaction boundary.
type Definition struct {
	ID        string
	Module    string
	Isolation sql.IsolationLevel
	ReadOnly  bool
}

// Result describes one completed transaction for an Observer. Err is the
// begin, callback, rollback, or commit failure. Panicked is true when Within
// rolled back because the callback panicked.
type Result struct {
	Definition Definition
	Duration   time.Duration
	Err        error
	Panicked   bool
}

// Observer receives transaction begin/end information on the calling
// goroutine. Implementations must not panic or block indefinitely.
type Observer interface {
	BeginTransaction(context.Context, Definition) (context.Context, func(Result))
}

// Work is application logic executed inside a managed transaction. The
// Executor deliberately omits Commit and Rollback so ownership stays with the
// manager.
type Work func(context.Context, Executor) error

// Manager executes transaction callbacks against one caller-owned database.
type Manager struct {
	db        *sql.DB
	observers []Observer
}

// NewManager constructs a transaction manager. It makes no connection and
// does not mutate database pool settings.
func NewManager(db *sql.DB, observers ...Observer) (*Manager, error) {
	if db == nil {
		return nil, errors.New("construct transaction manager: database is nil")
	}
	for index, observer := range observers {
		if nilObserver(observer) {
			return nil, fmt.Errorf("construct transaction manager: observer %d is nil", index)
		}
	}
	return &Manager{
		db:        db,
		observers: append([]Observer(nil), observers...),
	}, nil
}

// Within begins a transaction, invokes work, and commits only when work
// succeeds. Callback errors and panics cause rollback. A rollback failure is
// joined with the callback failure; a panic is re-raised after observation.
func (manager *Manager) Within(
	ctx context.Context,
	definition Definition,
	work Work,
) (resultErr error) {
	if ctx == nil {
		return errors.New("execute transaction: context is nil")
	}
	if manager == nil || manager.db == nil {
		return errors.New("execute transaction: manager is nil")
	}
	if err := validateDefinition(definition); err != nil {
		return err
	}
	if work == nil {
		return fmt.Errorf("execute transaction %q: callback is nil", definition.ID)
	}

	observedContext, finish := manager.beginObservation(ctx, definition)
	started := time.Now()
	tx, err := manager.db.BeginTx(observedContext, &sql.TxOptions{
		Isolation: definition.Isolation,
		ReadOnly:  definition.ReadOnly,
	})
	if err != nil {
		resultErr = fmt.Errorf("begin transaction %q: %w", definition.ID, err)
		finish(Result{Definition: definition, Duration: time.Since(started), Err: resultErr})
		return resultErr
	}

	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}
		rollbackErr := rollback(tx, definition.ID)
		observedErr := errors.Join(ErrPanicked, rollbackErr)
		finish(Result{
			Definition: definition,
			Duration:   time.Since(started),
			Err:        observedErr,
			Panicked:   true,
		})
		panic(recovered)
	}()

	if err := work(observedContext, tx); err != nil {
		workErr := fmt.Errorf("execute transaction %q: %w", definition.ID, err)
		resultErr = errors.Join(workErr, rollback(tx, definition.ID))
		finish(Result{Definition: definition, Duration: time.Since(started), Err: resultErr})
		return resultErr
	}
	if err := tx.Commit(); err != nil {
		resultErr = fmt.Errorf("commit transaction %q: %w", definition.ID, err)
		finish(Result{Definition: definition, Duration: time.Since(started), Err: resultErr})
		return resultErr
	}
	finish(Result{Definition: definition, Duration: time.Since(started)})
	return nil
}

func validateDefinition(definition Definition) error {
	if definition.ID == "" {
		return errors.New("execute transaction: boundary ID is required")
	}
	if definition.Module == "" {
		return fmt.Errorf("execute transaction %q: module is required", definition.ID)
	}
	switch definition.Isolation {
	case sql.LevelDefault,
		sql.LevelReadUncommitted,
		sql.LevelReadCommitted,
		sql.LevelWriteCommitted,
		sql.LevelRepeatableRead,
		sql.LevelSnapshot,
		sql.LevelSerializable,
		sql.LevelLinearizable:
		return nil
	default:
		return fmt.Errorf(
			"execute transaction %q: unsupported isolation level %d",
			definition.ID,
			definition.Isolation,
		)
	}
}

func rollback(tx *sql.Tx, id string) error {
	if err := tx.Rollback(); err != nil {
		return fmt.Errorf("rollback transaction %q: %w", id, err)
	}
	return nil
}

func (manager *Manager) beginObservation(
	ctx context.Context,
	definition Definition,
) (context.Context, func(Result)) {
	finishers := make([]func(Result), 0, len(manager.observers))
	observedContext := beginObservers(ctx, definition, manager.observers, &finishers)
	return observedContext, func(result Result) {
		for _, finish := range slices.Backward(finishers) {
			finish(result)
		}
	}
}

func beginObservers(
	ctx context.Context,
	definition Definition,
	observers []Observer,
	finishers *[]func(Result),
) context.Context {
	if len(observers) == 0 {
		return ctx
	}
	next, finish := observers[0].BeginTransaction(ctx, definition)
	if next == nil {
		next = ctx
	}
	if finish != nil {
		*finishers = append(*finishers, finish)
	}
	return beginObservers(next, definition, observers[1:], finishers)
}

func nilObserver(observer Observer) bool {
	if observer == nil {
		return true
	}
	value := reflect.ValueOf(observer)
	return (value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer) && value.IsNil()
}

var (
	_ Executor = (*sql.DB)(nil)
	_ Executor = (*sql.Tx)(nil)
)
