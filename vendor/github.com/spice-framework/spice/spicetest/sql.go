package spicetest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/spice-framework/spice/data"
)

// SQLFactory constructs a transaction-scoped test subject. The executor
// deliberately omits Commit and Rollback.
type SQLFactory[T any] func(context.Context, data.Executor) (T, error)

// SQLOptions controls the test transaction.
type SQLOptions struct {
	Isolation sql.IsolationLevel
	ReadOnly  bool
}

// SQLRollbackPanic reports the rare case where a factory panicked and its
// mandatory rollback also failed. Error deliberately excludes Value.
type SQLRollbackPanic struct {
	Value       any
	RollbackErr error
}

// Error describes the failed cleanup without formatting the panic value.
func (failure *SQLRollbackPanic) Error() string {
	return "SQL test factory panicked and rollback failed"
}

// Unwrap exposes the rollback failure.
func (failure *SQLRollbackPanic) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.RollbackErr
}

// SQL is one transaction-scoped data test slice. Close always rolls back.
type SQL[T any] struct {
	value     T
	executor  data.Executor
	tx        *sql.Tx
	closed    atomic.Bool
	closeOnce sync.Once
	closeErr  error
}

// NewSQL begins a test-owned transaction and constructs one subject inside it.
// Construction errors roll back before return. Factory panics are rolled back
// and re-panicked with the original value.
func NewSQL[T any](
	ctx context.Context,
	database *sql.DB,
	factory SQLFactory[T],
	options SQLOptions,
) (result *SQL[T], resultErr error) {
	if err := validateSQLSlice(ctx, database, factory, options); err != nil {
		return nil, err
	}
	tx, err := database.BeginTx(ctx, &sql.TxOptions{
		Isolation: options.Isolation,
		ReadOnly:  options.ReadOnly,
	})
	if err != nil {
		return nil, fmt.Errorf("begin SQL test slice: %w", err)
	}
	defer func() {
		panicValue := recover()
		if panicValue == nil {
			return
		}
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			panic(&SQLRollbackPanic{
				Value:       panicValue,
				RollbackErr: rollbackErr,
			})
		}
		panic(panicValue)
	}()

	value, err := factory(ctx, tx)
	if err != nil {
		rollbackErr := tx.Rollback()
		return nil, errors.Join(
			fmt.Errorf("construct SQL test subject: %w", err),
			wrapSQLSliceError("rollback failed SQL test subject", rollbackErr),
		)
	}
	return &SQL[T]{
		value:    value,
		executor: tx,
		tx:       tx,
	}, nil
}

// Value returns the transaction-scoped subject.
func (slice *SQL[T]) Value() T {
	if slice == nil {
		var zero T
		return zero
	}
	return slice.value
}

// Executor returns the restricted transaction executor.
func (slice *SQL[T]) Executor() data.Executor {
	if slice == nil {
		return nil
	}
	return slice.executor
}

// Closed reports whether rollback has begun.
func (slice *SQL[T]) Closed() bool {
	return slice == nil || slice.closed.Load()
}

// Close rolls the transaction back exactly once. It is concurrency-safe and
// returns the same rollback outcome to every caller.
func (slice *SQL[T]) Close() error {
	if slice == nil {
		return nil
	}
	slice.closeOnce.Do(func() {
		slice.closed.Store(true)
		slice.closeErr = wrapSQLSliceError(
			"rollback SQL test slice",
			slice.tx.Rollback(),
		)
	})
	return slice.closeErr
}

func validateSQLSlice[T any](
	ctx context.Context,
	database *sql.DB,
	factory SQLFactory[T],
	options SQLOptions,
) error {
	switch {
	case ctx == nil:
		return errors.New("construct SQL test slice: context is nil")
	case database == nil:
		return errors.New("construct SQL test slice: database is nil")
	case factory == nil:
		return errors.New("construct SQL test slice: factory is nil")
	default:
		if cause := context.Cause(ctx); cause != nil {
			return fmt.Errorf("construct SQL test slice: %w", cause)
		}
	}
	switch options.Isolation {
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
			"construct SQL test slice: unsupported isolation level %d",
			options.Isolation,
		)
	}
}

func wrapSQLSliceError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}
