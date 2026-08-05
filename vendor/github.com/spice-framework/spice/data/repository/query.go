// Package repository provides bounded, typed database/sql query primitives
// for application-owned and generated repositories.
package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"

	"github.com/spice-framework/spice/data"
)

const (
	// DefaultMaxRows is the recommended list bound for generated repositories.
	DefaultMaxRows = 1_000
	// MaxRows is the largest list bound accepted by NewQuery.
	MaxRows = 100_000
)

var (
	// ErrNotFound indicates that a required single-row query returned no rows.
	ErrNotFound = errors.New("repository query returned no rows")
	// ErrMultipleRows indicates that a single-row query returned more than one row.
	ErrMultipleRows = errors.New("repository query returned multiple rows")
	// ErrRowLimitExceeded indicates that a list query produced more than its
	// declared in-memory result bound.
	ErrRowLimitExceeded = errors.New("repository query row limit exceeded")
)

// Scanner is implemented by *sql.Row and *sql.Rows. Decoders should call Scan
// exactly once and return a newly owned value.
type Scanner interface {
	Scan(...any) error
}

// Decoder maps the current database row to T without reflection.
type Decoder[T any] func(Scanner) (T, error)

// QuerySpec describes one immutable repository query. Statement syntax and
// placeholders remain owned by the selected database dialect.
type QuerySpec[T any] struct {
	ID        string
	Module    string
	Statement string
	MaxRows   int
	Decode    Decoder[T]
}

// Query is an immutable, typed repository query safe for concurrent use when
// its decoder is safe for concurrent use.
type Query[T any] struct {
	id        string
	module    string
	statement string
	maxRows   int
	decode    Decoder[T]
}

// NewQuery validates and freezes a repository query definition.
func NewQuery[T any](spec QuerySpec[T]) (*Query[T], error) {
	switch {
	case spec.ID == "":
		return nil, errors.New("construct repository query: ID is required")
	case spec.Module == "":
		return nil, fmt.Errorf("construct repository query %q: module is required", spec.ID)
	case spec.Statement == "":
		return nil, fmt.Errorf("construct repository query %q: statement is required", spec.ID)
	case spec.MaxRows <= 0:
		return nil, fmt.Errorf("construct repository query %q: max rows must be positive", spec.ID)
	case spec.MaxRows > MaxRows:
		return nil, fmt.Errorf(
			"construct repository query %q: max rows %d exceeds %d",
			spec.ID,
			spec.MaxRows,
			MaxRows,
		)
	case spec.Decode == nil:
		return nil, fmt.Errorf("construct repository query %q: decoder is nil", spec.ID)
	default:
		return &Query[T]{
			id:        spec.ID,
			module:    spec.Module,
			statement: spec.Statement,
			maxRows:   spec.MaxRows,
			decode:    spec.Decode,
		}, nil
	}
}

// ID returns the stable compiler- or application-owned operation identity.
func (query *Query[T]) ID() string {
	if query == nil {
		return ""
	}
	return query.id
}

// Module returns the owning application module identity.
func (query *Query[T]) Module() string {
	if query == nil {
		return ""
	}
	return query.module
}

// List returns rows in driver order. It rejects the result before decoding any
// row beyond MaxRows. Callers should also bound work in SQL so the database
// does not produce an unnecessarily large result.
func (query *Query[T]) List(
	ctx context.Context,
	executor data.Executor,
	args ...any,
) ([]T, error) {
	rows, err := query.open(ctx, executor, args)
	if err != nil {
		return nil, err
	}

	items := make([]T, 0, min(query.maxRows, 64))
	for rows.Next() {
		if len(items) == query.maxRows {
			return nil, query.finish(
				rows,
				fmt.Errorf("%w: maximum is %d", ErrRowLimitExceeded, query.maxRows),
			)
		}
		item, decodeErr := query.decode(rows)
		if decodeErr != nil {
			return nil, query.finish(
				rows,
				fmt.Errorf("decode row %d: %w", len(items), decodeErr),
			)
		}
		items = append(items, item)
	}
	if err := query.finish(rows, nil); err != nil {
		return nil, err
	}
	return items, nil
}

// One returns exactly one row. Zero and multiple rows are errors identifiable
// with errors.Is.
func (query *Query[T]) One(
	ctx context.Context,
	executor data.Executor,
	args ...any,
) (T, error) {
	value, found, err := query.single(ctx, executor, args)
	if err != nil {
		return value, err
	}
	if !found {
		return value, query.wrap(ErrNotFound)
	}
	return value, nil
}

// Optional returns found=false when no row exists and rejects multiple rows.
func (query *Query[T]) Optional(
	ctx context.Context,
	executor data.Executor,
	args ...any,
) (value T, found bool, err error) {
	return query.single(ctx, executor, args)
}

func (query *Query[T]) single(
	ctx context.Context,
	executor data.Executor,
	args []any,
) (value T, found bool, resultErr error) {
	rows, err := query.open(ctx, executor, args)
	if err != nil {
		return value, false, err
	}
	if !rows.Next() {
		if finishErr := query.finish(rows, nil); finishErr != nil {
			return value, false, finishErr
		}
		return value, false, nil
	}

	decoded, err := query.decode(rows)
	if err != nil {
		return value, false, query.finish(rows, fmt.Errorf("decode row 0: %w", err))
	}
	if rows.Next() {
		return value, false, query.finish(rows, ErrMultipleRows)
	}
	if err := query.finish(rows, nil); err != nil {
		return value, false, err
	}
	return decoded, true, nil
}

func (query *Query[T]) open(
	ctx context.Context,
	executor data.Executor,
	args []any,
) (*sql.Rows, error) {
	switch {
	case ctx == nil:
		return nil, errors.New("execute repository query: context is nil")
	case query == nil || query.decode == nil:
		return nil, errors.New("execute repository query: query is nil")
	case nilExecutor(executor):
		return nil, query.wrap(errors.New("executor is nil"))
	default:
		rows, err := executor.QueryContext(ctx, query.statement, args...)
		if err != nil {
			return nil, query.wrap(err)
		}
		return rows, nil
	}
}

func (query *Query[T]) finish(rows *sql.Rows, primary error) error {
	rowsErr := rows.Err()
	closeErr := rows.Close()
	combined := errors.Join(primary, rowsErr, closeErr)
	if combined == nil {
		return nil
	}
	return query.wrap(combined)
}

func (query *Query[T]) wrap(err error) error {
	return fmt.Errorf("execute repository query %q: %w", query.id, err)
}

func nilExecutor(executor data.Executor) bool {
	if executor == nil {
		return true
	}
	value := reflect.ValueOf(executor)
	return (value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer) && value.IsNil()
}
