package batch

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
)

const maxMemoryStoreCapacity = 1_000_000

type executionKey struct {
	definition Definition
	instance   string
}

type memoryExecution struct {
	steps           []string
	attempt         uint64
	completed       int
	running         bool
	complete        bool
	lastFailureStep string
	lastFailureKind FailureKind
}

// ExecutionSnapshot is an immutable diagnostic view of one in-process
// execution. Instance identities are intentionally excluded.
type ExecutionSnapshot struct {
	Definition      Definition
	Attempt         uint64
	CompletedSteps  []string
	Running         bool
	Complete        bool
	LastFailureStep string
	LastFailureKind FailureKind
}

// MemoryStore is a concurrency-safe, capacity-bounded in-process Store.
//
// State is not durable across process restarts. Use it for development,
// tests, and jobs whose restart state does not need to survive a process.
type MemoryStore struct {
	mu         sync.Mutex
	capacity   int
	executions map[executionKey]*memoryExecution
}

// NewMemoryStore constructs an empty store with a fixed instance capacity.
func NewMemoryStore(capacity int) (*MemoryStore, error) {
	if capacity <= 0 || capacity > maxMemoryStoreCapacity {
		return nil, fmt.Errorf(
			"construct batch memory store: capacity must be between 1 and %d",
			maxMemoryStoreCapacity,
		)
	}
	return &MemoryStore{
		capacity:   capacity,
		executions: make(map[executionKey]*memoryExecution, capacity),
	}, nil
}

// Begin atomically starts or resumes one execution attempt.
func (store *MemoryStore) Begin(
	ctx context.Context,
	request BeginRequest,
) (Attempt, error) {
	if err := validateMemoryContext(ctx, store, "begin"); err != nil {
		return Attempt{}, err
	}
	if err := validateBeginRequest(request); err != nil {
		return Attempt{}, err
	}
	key := executionKey{definition: request.Definition, instance: request.Instance}

	store.mu.Lock()
	defer store.mu.Unlock()

	execution, exists := store.executions[key]
	if !exists {
		if len(store.executions) >= store.capacity {
			return Attempt{}, fmt.Errorf("begin batch instance: %w", ErrCapacity)
		}
		execution = &memoryExecution{
			steps:   slices.Clone(request.Steps),
			attempt: 1,
			running: true,
		}
		store.executions[key] = execution
		return memoryAttempt(key, execution), nil
	}
	if !slices.Equal(execution.steps, request.Steps) {
		return Attempt{}, fmt.Errorf(
			"begin batch job %q: %w",
			request.Definition.ID,
			ErrDefinitionChanged,
		)
	}
	if execution.complete {
		return memoryAttempt(key, execution), nil
	}
	if execution.running {
		return Attempt{}, fmt.Errorf(
			"begin batch job %q: %w",
			request.Definition.ID,
			ErrAlreadyRunning,
		)
	}
	if execution.attempt == ^uint64(0) {
		return Attempt{}, fmt.Errorf(
			"begin batch job %q: attempt number overflow",
			request.Definition.ID,
		)
	}
	execution.attempt++
	execution.running = true
	return memoryAttempt(key, execution), nil
}

// Checkpoint atomically records the next ordered step.
func (store *MemoryStore) Checkpoint(
	ctx context.Context,
	attempt Attempt,
	step string,
) error {
	if err := validateMemoryContext(ctx, store, "checkpoint"); err != nil {
		return err
	}
	if err := validateStoreAttempt(attempt); err != nil {
		return fmt.Errorf("checkpoint batch attempt: %w", err)
	}
	if !validMetadata(step) {
		return errors.New("checkpoint batch attempt: step is invalid")
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	execution, err := store.activeExecution(attempt)
	if err != nil {
		return fmt.Errorf("checkpoint batch attempt: %w", err)
	}
	if execution.completed >= len(execution.steps) ||
		execution.steps[execution.completed] != step {
		return errors.New(
			"checkpoint batch attempt: step is not the next ordered step",
		)
	}
	execution.completed++
	return nil
}

// Complete atomically marks a fully checkpointed attempt complete.
func (store *MemoryStore) Complete(
	ctx context.Context,
	attempt Attempt,
) error {
	if err := validateMemoryContext(ctx, store, "complete"); err != nil {
		return err
	}
	if err := validateStoreAttempt(attempt); err != nil {
		return fmt.Errorf("complete batch attempt: %w", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	execution, err := store.activeExecution(attempt)
	if err != nil {
		return fmt.Errorf("complete batch attempt: %w", err)
	}
	if execution.completed != len(execution.steps) {
		return errors.New("complete batch attempt: pending steps remain")
	}
	execution.complete = true
	execution.running = false
	execution.lastFailureStep = ""
	execution.lastFailureKind = ""
	return nil
}

// Fail atomically releases an active attempt for a later restart.
func (store *MemoryStore) Fail(ctx context.Context, failure Failure) error {
	if err := validateMemoryContext(ctx, store, "fail"); err != nil {
		return err
	}
	if err := validateFailure(failure); err != nil {
		return err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	execution, err := store.activeExecution(failure.Attempt)
	if err != nil {
		return fmt.Errorf("fail batch attempt: %w", err)
	}
	if !validFailureStep(execution, failure.Step) {
		return errors.New("fail batch attempt: step is outside the active boundary")
	}
	execution.running = false
	execution.lastFailureStep = failure.Step
	execution.lastFailureKind = failure.Kind
	return nil
}

// Snapshot returns a defensive diagnostic view when an execution exists.
func (store *MemoryStore) Snapshot(
	ctx context.Context,
	definition Definition,
	instance string,
) (ExecutionSnapshot, bool, error) {
	if err := validateMemoryContext(ctx, store, "snapshot"); err != nil {
		return ExecutionSnapshot{}, false, err
	}
	if err := validateDefinition(definition); err != nil {
		return ExecutionSnapshot{}, false, fmt.Errorf(
			"snapshot batch instance: %w",
			err,
		)
	}
	if !validMetadata(instance) {
		return ExecutionSnapshot{}, false, errors.New(
			"snapshot batch instance: instance is invalid",
		)
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	execution, exists := store.executions[executionKey{
		definition: definition,
		instance:   instance,
	}]
	if !exists {
		return ExecutionSnapshot{}, false, nil
	}
	return ExecutionSnapshot{
		Definition:      definition,
		Attempt:         execution.attempt,
		CompletedSteps:  slices.Clone(execution.steps[:execution.completed]),
		Running:         execution.running,
		Complete:        execution.complete,
		LastFailureStep: execution.lastFailureStep,
		LastFailureKind: execution.lastFailureKind,
	}, true, nil
}

// Delete removes inactive execution state and releases capacity.
func (store *MemoryStore) Delete(
	ctx context.Context,
	definition Definition,
	instance string,
) error {
	if err := validateMemoryContext(ctx, store, "delete"); err != nil {
		return err
	}
	if err := validateDefinition(definition); err != nil {
		return fmt.Errorf("delete batch instance: %w", err)
	}
	if !validMetadata(instance) {
		return errors.New("delete batch instance: instance is invalid")
	}
	key := executionKey{definition: definition, instance: instance}

	store.mu.Lock()
	defer store.mu.Unlock()

	execution, exists := store.executions[key]
	if !exists {
		return nil
	}
	if execution.running {
		return fmt.Errorf("delete batch instance: %w", ErrAlreadyRunning)
	}
	delete(store.executions, key)
	return nil
}

func (store *MemoryStore) activeExecution(
	attempt Attempt,
) (*memoryExecution, error) {
	execution, exists := store.executions[executionKey{
		definition: attempt.definition,
		instance:   attempt.instance,
	}]
	if !exists ||
		!execution.running ||
		execution.complete ||
		execution.attempt != attempt.number {
		return nil, ErrStaleAttempt
	}
	return execution, nil
}

func memoryAttempt(key executionKey, execution *memoryExecution) Attempt {
	return Attempt{
		definition:     key.definition,
		instance:       key.instance,
		number:         execution.attempt,
		completedSteps: slices.Clone(execution.steps[:execution.completed]),
		complete:       execution.complete,
	}
}

func validateBeginRequest(request BeginRequest) error {
	if err := validateDefinition(request.Definition); err != nil {
		return fmt.Errorf("begin batch instance: %w", err)
	}
	if !validMetadata(request.Instance) {
		return errors.New("begin batch instance: instance is invalid")
	}
	if len(request.Steps) == 0 || len(request.Steps) > maxSteps {
		return fmt.Errorf(
			"begin batch instance: step count must be between 1 and %d",
			maxSteps,
		)
	}
	seen := make(map[string]struct{}, len(request.Steps))
	for index, step := range request.Steps {
		if !validMetadata(step) {
			return fmt.Errorf("begin batch instance: step %d is invalid", index)
		}
		if _, duplicate := seen[step]; duplicate {
			return fmt.Errorf(
				"begin batch instance: step %q is duplicated",
				step,
			)
		}
		seen[step] = struct{}{}
	}
	return nil
}

func validateStoreAttempt(attempt Attempt) error {
	if err := validateDefinition(attempt.definition); err != nil {
		return err
	}
	if !validMetadata(attempt.instance) {
		return errors.New("instance is invalid")
	}
	if attempt.number == 0 {
		return errors.New("attempt number must be positive")
	}
	return nil
}

func validateFailure(failure Failure) error {
	if err := validateStoreAttempt(failure.Attempt); err != nil {
		return fmt.Errorf("fail batch attempt: %w", err)
	}
	if !validMetadata(failure.Step) {
		return errors.New("fail batch attempt: step is invalid")
	}
	switch failure.Kind {
	case FailureError, FailureCanceled, FailurePanic:
		return nil
	default:
		return errors.New("fail batch attempt: kind is invalid")
	}
}

func validFailureStep(execution *memoryExecution, step string) bool {
	if execution.completed < len(execution.steps) {
		return execution.steps[execution.completed] == step
	}
	return len(execution.steps) != 0 &&
		execution.steps[len(execution.steps)-1] == step
}

func validateMemoryContext(
	ctx context.Context,
	store *MemoryStore,
	operation string,
) error {
	switch {
	case store == nil:
		return fmt.Errorf("%s batch memory store: store is nil", operation)
	case ctx == nil:
		return fmt.Errorf("%s batch memory store: context is nil", operation)
	default:
		if cause := context.Cause(ctx); cause != nil {
			return fmt.Errorf(
				"%s batch memory store: %w",
				operation,
				cause,
			)
		}
		return nil
	}
}

var _ Store = (*MemoryStore)(nil)
