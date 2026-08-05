package batch

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spice-framework/spice/data"
)

const (
	maxSQLAttemptLease      = 24 * time.Hour
	maxSerializedStepsBytes = (maxMetadataBytes+3)*maxSteps + 2
	maxSQLAttemptNumber     = uint64(1<<63 - 1)
	// SQLBeginOutcomeStarted identifies a newly inserted instance.
	SQLBeginOutcomeStarted = "started"
	// SQLBeginOutcomeResumed identifies a new attempt over retained checkpoints.
	SQLBeginOutcomeResumed = "resumed"
	// SQLBeginOutcomeComplete identifies an already-complete instance.
	SQLBeginOutcomeComplete = "complete"
	// SQLBeginOutcomeRunning identifies an unexpired active attempt.
	SQLBeginOutcomeRunning = "running"
	// SQLBeginOutcomeChanged identifies incompatible ordered steps.
	SQLBeginOutcomeChanged = "changed"
	// SQLBeginOutcomeOverflow identifies an exhausted signed SQL attempt number.
	SQLBeginOutcomeOverflow = "overflow"
)

// SQLStatements supplies dialect-owned atomic statements for the batch
// persistence protocol. Statement text is trusted startup configuration,
// never request input.
type SQLStatements struct {
	Begin      string
	Checkpoint string
	Complete   string
	Fail       string
}

// SQLStoreOptions controls durable attempt ownership.
type SQLStoreOptions struct {
	// AttemptLease is the maximum time an attempt remains exclusively active
	// without a checkpoint. Steps may execute at least once when they outlive
	// this lease and another runner resumes the instance.
	AttemptLease time.Duration
	// Clock supplies current time. Nil selects time.Now.
	Clock func() time.Time
}

// SQLStore implements Store through standard database/sql contracts.
type SQLStore struct {
	executor   data.Executor
	statements SQLStatements
	lease      time.Duration
	clock      func() time.Time
}

// NewSQLStore validates and freezes one driver-neutral SQL store. Construction
// performs no database operation.
func NewSQLStore(
	executor data.Executor,
	statements SQLStatements,
	options SQLStoreOptions,
) (*SQLStore, error) {
	if nilInterface(executor) {
		return nil, errors.New("construct batch SQL store: executor is nil")
	}
	for _, statement := range []struct {
		name  string
		value string
	}{
		{name: "begin", value: statements.Begin},
		{name: "checkpoint", value: statements.Checkpoint},
		{name: "complete", value: statements.Complete},
		{name: "fail", value: statements.Fail},
	} {
		if strings.TrimSpace(statement.value) == "" {
			return nil, fmt.Errorf(
				"construct batch SQL store: %s statement is empty",
				statement.name,
			)
		}
	}
	if options.AttemptLease <= 0 ||
		options.AttemptLease > maxSQLAttemptLease {
		return nil, errors.New(
			"construct batch SQL store: attempt lease must be between 1ns and 24h",
		)
	}
	if options.Clock == nil {
		options.Clock = time.Now
	}
	return &SQLStore{
		executor:   executor,
		statements: statements,
		lease:      options.AttemptLease,
		clock:      options.Clock,
	}, nil
}

// Begin atomically inserts, resumes, or observes one persisted instance.
//
// The statement arguments are job ID, module, instance, canonical JSON step
// IDs, current UTC time, and lease expiry. It must return exactly one row with
// outcome, positive attempt number, and JSON completed step IDs.
func (store *SQLStore) Begin(
	ctx context.Context,
	request BeginRequest,
) (Attempt, error) {
	if err := validateSQLContext(ctx, store, "begin"); err != nil {
		return Attempt{}, err
	}
	if err := validateBeginRequest(request); err != nil {
		return Attempt{}, err
	}
	now, expiry, err := store.leaseBoundary()
	if err != nil {
		return Attempt{}, err
	}
	steps, err := json.Marshal(request.Steps)
	if err != nil {
		return Attempt{}, fmt.Errorf("begin batch SQL instance: encode steps: %w", err)
	}

	var outcome string
	var number int64
	var completedJSON []byte
	err = store.executor.QueryRowContext(
		ctx,
		store.statements.Begin,
		request.Definition.ID,
		request.Definition.Module,
		request.Instance,
		steps,
		now,
		expiry,
	).Scan(&outcome, &number, &completedJSON)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Attempt{}, errors.New(
				"begin batch SQL instance: statement returned no outcome",
			)
		}
		return Attempt{}, fmt.Errorf("begin batch SQL instance: %w", err)
	}
	return sqlAttempt(request, outcome, number, completedJSON)
}

func sqlAttempt(
	request BeginRequest,
	outcome string,
	number int64,
	completedJSON []byte,
) (Attempt, error) {
	switch outcome {
	case SQLBeginOutcomeRunning:
		return Attempt{}, fmt.Errorf("begin batch SQL instance: %w", ErrAlreadyRunning)
	case SQLBeginOutcomeChanged:
		return Attempt{}, fmt.Errorf("begin batch SQL instance: %w", ErrDefinitionChanged)
	case SQLBeginOutcomeOverflow:
		return Attempt{}, errors.New("begin batch SQL instance: attempt number overflow")
	case SQLBeginOutcomeStarted, SQLBeginOutcomeResumed, SQLBeginOutcomeComplete:
	default:
		return Attempt{}, errors.New("begin batch SQL instance: outcome is invalid")
	}
	if number <= 0 {
		return Attempt{}, errors.New(
			"begin batch SQL instance: attempt number must be positive",
		)
	}
	completed, err := decodeCompletedSteps(completedJSON, request.Steps)
	if err != nil {
		return Attempt{}, err
	}
	complete := outcome == SQLBeginOutcomeComplete
	if outcome == SQLBeginOutcomeStarted && len(completed) != 0 {
		return Attempt{}, errors.New(
			"begin batch SQL instance: new attempt has completed steps",
		)
	}
	if complete && len(completed) != len(request.Steps) {
		return Attempt{}, errors.New(
			"begin batch SQL instance: completed attempt has pending steps",
		)
	}
	attempt, err := NewAttempt(AttemptSpec{
		Definition:     request.Definition,
		Instance:       request.Instance,
		Number:         uint64(number),
		CompletedSteps: completed,
		Complete:       complete,
	})
	if err != nil {
		return Attempt{}, fmt.Errorf(
			"begin batch SQL instance: reconstruct attempt: %w",
			err,
		)
	}
	return attempt, nil
}

// Checkpoint atomically records the next step and renews the attempt lease.
//
// Statement arguments are job ID, module, instance, attempt number, step,
// current UTC time, and lease expiry.
func (store *SQLStore) Checkpoint(
	ctx context.Context,
	attempt Attempt,
	step string,
) error {
	if err := validateSQLContext(ctx, store, "checkpoint"); err != nil {
		return err
	}
	number, err := sqlAttemptNumber(attempt)
	if err != nil {
		return fmt.Errorf("checkpoint batch SQL attempt: %w", err)
	}
	if !validMetadata(step) {
		return errors.New("checkpoint batch SQL attempt: step is invalid")
	}
	now, expiry, err := store.leaseBoundary()
	if err != nil {
		return err
	}
	return store.execTransition(
		ctx,
		"checkpoint",
		store.statements.Checkpoint,
		attempt.definition.ID,
		attempt.definition.Module,
		attempt.instance,
		number,
		step,
		now,
		expiry,
	)
}

// Complete atomically completes the exact active attempt.
func (store *SQLStore) Complete(
	ctx context.Context,
	attempt Attempt,
) error {
	if err := validateSQLContext(ctx, store, "complete"); err != nil {
		return err
	}
	number, err := sqlAttemptNumber(attempt)
	if err != nil {
		return fmt.Errorf("complete batch SQL attempt: %w", err)
	}
	now, err := store.currentTime("complete")
	if err != nil {
		return err
	}
	return store.execTransition(
		ctx,
		"complete",
		store.statements.Complete,
		attempt.definition.ID,
		attempt.definition.Module,
		attempt.instance,
		number,
		now,
	)
}

// Fail atomically releases the exact active attempt for a later resume.
func (store *SQLStore) Fail(ctx context.Context, failure Failure) error {
	if err := validateSQLContext(ctx, store, "fail"); err != nil {
		return err
	}
	if err := validateFailure(failure); err != nil {
		return err
	}
	number, err := sqlAttemptNumber(failure.Attempt)
	if err != nil {
		return fmt.Errorf("fail batch SQL attempt: %w", err)
	}
	now, err := store.currentTime("fail")
	if err != nil {
		return err
	}
	return store.execTransition(
		ctx,
		"fail",
		store.statements.Fail,
		failure.Attempt.definition.ID,
		failure.Attempt.definition.Module,
		failure.Attempt.instance,
		number,
		failure.Step,
		string(failure.Kind),
		now,
	)
}

func (store *SQLStore) execTransition(
	ctx context.Context,
	operation string,
	statement string,
	arguments ...any,
) error {
	result, err := store.executor.ExecContext(ctx, statement, arguments...)
	if err != nil {
		return fmt.Errorf("%s batch SQL attempt: %w", operation, err)
	}
	if nilInterface(result) {
		return fmt.Errorf(
			"%s batch SQL attempt: execution returned nil result",
			operation,
		)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf(
			"%s batch SQL attempt: read affected rows: %w",
			operation,
			err,
		)
	}
	switch affected {
	case 0:
		return fmt.Errorf("%s batch SQL attempt: %w", operation, ErrStaleAttempt)
	case 1:
		return nil
	default:
		return fmt.Errorf(
			"%s batch SQL attempt: affected %d rows, want exactly 1",
			operation,
			affected,
		)
	}
}

func (store *SQLStore) leaseBoundary() (time.Time, time.Time, error) {
	now, err := store.currentTime("renew lease")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return now, now.Add(store.lease), nil
}

func (store *SQLStore) currentTime(operation string) (time.Time, error) {
	now := store.clock()
	if now.IsZero() {
		return time.Time{}, fmt.Errorf(
			"%s batch SQL attempt: clock returned zero time",
			operation,
		)
	}
	return now.UTC(), nil
}

func decodeCompletedSteps(encoded []byte, expected []string) ([]string, error) {
	if len(encoded) == 0 || len(encoded) > maxSerializedStepsBytes {
		return nil, errors.New(
			"begin batch SQL instance: completed steps are invalid",
		)
	}
	var completed []string
	if err := json.Unmarshal(encoded, &completed); err != nil {
		return nil, errors.New(
			"begin batch SQL instance: completed steps are invalid",
		)
	}
	if completed == nil || len(completed) > len(expected) {
		return nil, errors.New(
			"begin batch SQL instance: completed steps are not an exact definition prefix",
		)
	}
	for index, step := range completed {
		if step != expected[index] {
			return nil, errors.New(
				"begin batch SQL instance: completed steps are not an exact definition prefix",
			)
		}
	}
	return completed, nil
}

func sqlAttemptNumber(attempt Attempt) (int64, error) {
	if err := validateStoreAttempt(attempt); err != nil {
		return 0, err
	}
	if attempt.number > maxSQLAttemptNumber {
		return 0, errors.New("attempt number exceeds the signed SQL range")
	}
	return int64(attempt.number), nil
}

func validateSQLContext(
	ctx context.Context,
	store *SQLStore,
	operation string,
) error {
	switch {
	case store == nil || nilInterface(store.executor):
		return fmt.Errorf("%s batch SQL store: store is nil", operation)
	case ctx == nil:
		return fmt.Errorf("%s batch SQL store: context is nil", operation)
	default:
		if cause := context.Cause(ctx); cause != nil {
			return fmt.Errorf("%s batch SQL store: %w", operation, cause)
		}
		return nil
	}
}

var _ Store = (*SQLStore)(nil)
