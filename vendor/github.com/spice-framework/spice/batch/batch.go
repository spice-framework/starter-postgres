// Package batch provides restartable, explicitly persisted job and step
// execution for Spice applications.
package batch

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxMetadataBytes = 512
	maxSteps         = 10_000
)

var (
	// ErrPanicked identifies a contained batch step panic.
	ErrPanicked = errors.New("batch step panicked")
	// ErrAlreadyRunning identifies an active attempt for the same job instance.
	ErrAlreadyRunning = errors.New("batch instance is already running")
	// ErrStaleAttempt identifies a transition for an inactive or old attempt.
	ErrStaleAttempt = errors.New("batch attempt is stale")
	// ErrDefinitionChanged identifies a persisted instance whose ordered steps
	// differ from the current job definition.
	ErrDefinitionChanged = errors.New("batch definition changed")
	// ErrCapacity identifies an in-process store at its configured instance
	// limit.
	ErrCapacity = errors.New("batch store capacity reached")
)

// Definition identifies one module-owned batch job.
type Definition struct {
	ID     string
	Module string
}

// StepSpec is the inspectable input to NewJob.
type StepSpec struct {
	ID  string
	Run func(context.Context) error
}

// Step is one immutable job step.
type Step struct {
	id  string
	run func(context.Context) error
}

// ID returns the stable step identity.
func (step Step) ID() string {
	return step.id
}

// Job is an immutable ordered batch definition.
type Job struct {
	definition Definition
	steps      []Step
}

// NewJob validates and freezes one ordered job.
func NewJob(definition Definition, specs []StepSpec) (*Job, error) {
	if err := validateDefinition(definition); err != nil {
		return nil, err
	}
	if len(specs) == 0 || len(specs) > maxSteps {
		return nil, fmt.Errorf(
			"construct batch job %q: step count must be between 1 and %d",
			definition.ID,
			maxSteps,
		)
	}
	steps := make([]Step, 0, len(specs))
	seen := make(map[string]struct{}, len(specs))
	for index, spec := range specs {
		if !validMetadata(spec.ID) {
			return nil, fmt.Errorf(
				"construct batch job %q: step %d ID is invalid",
				definition.ID,
				index,
			)
		}
		if _, duplicate := seen[spec.ID]; duplicate {
			return nil, fmt.Errorf(
				"construct batch job %q: step ID %q is duplicated",
				definition.ID,
				spec.ID,
			)
		}
		if spec.Run == nil {
			return nil, fmt.Errorf(
				"construct batch job %q: step %q callback is nil",
				definition.ID,
				spec.ID,
			)
		}
		seen[spec.ID] = struct{}{}
		steps = append(steps, Step{id: spec.ID, run: spec.Run})
	}
	return &Job{definition: definition, steps: steps}, nil
}

// Definition returns the job identity.
func (job *Job) Definition() Definition {
	if job == nil {
		return Definition{}
	}
	return job.definition
}

// Steps returns a defensive copy of the ordered steps.
func (job *Job) Steps() []Step {
	if job == nil {
		return []Step{}
	}
	return append([]Step(nil), job.steps...)
}

// AttemptSpec is the inspectable input to NewAttempt.
type AttemptSpec struct {
	Definition     Definition
	Instance       string
	Number         uint64
	CompletedSteps []string
	Complete       bool
}

// Attempt is immutable persisted restart metadata returned by a Store.
type Attempt struct {
	definition     Definition
	instance       string
	number         uint64
	completedSteps []string
	complete       bool
}

// NewAttempt validates and freezes persisted restart metadata.
func NewAttempt(spec AttemptSpec) (Attempt, error) {
	if err := validateDefinition(spec.Definition); err != nil {
		return Attempt{}, fmt.Errorf("construct batch attempt: %w", err)
	}
	if !validMetadata(spec.Instance) {
		return Attempt{}, errors.New("construct batch attempt: instance is invalid")
	}
	if spec.Number == 0 {
		return Attempt{}, errors.New("construct batch attempt: number must be positive")
	}
	seen := make(map[string]struct{}, len(spec.CompletedSteps))
	for index, step := range spec.CompletedSteps {
		if !validMetadata(step) {
			return Attempt{}, fmt.Errorf(
				"construct batch attempt: completed step %d is invalid",
				index,
			)
		}
		if _, duplicate := seen[step]; duplicate {
			return Attempt{}, fmt.Errorf(
				"construct batch attempt: completed step %q is duplicated",
				step,
			)
		}
		seen[step] = struct{}{}
	}
	return Attempt{
		definition:     spec.Definition,
		instance:       spec.Instance,
		number:         spec.Number,
		completedSteps: append([]string(nil), spec.CompletedSteps...),
		complete:       spec.Complete,
	}, nil
}

// Definition returns the attempted job identity.
func (attempt Attempt) Definition() Definition {
	return attempt.definition
}

// Instance returns the caller-owned idempotent job instance identity.
func (attempt Attempt) Instance() string {
	return attempt.instance
}

// Number returns the one-based execution attempt.
func (attempt Attempt) Number() uint64 {
	return attempt.number
}

// CompletedSteps returns a defensive copy of the durable completed prefix.
func (attempt Attempt) CompletedSteps() []string {
	return append([]string(nil), attempt.completedSteps...)
}

// Complete reports whether the store already completed this job instance.
func (attempt Attempt) Complete() bool {
	return attempt.complete
}

// BeginRequest asks a Store to atomically begin or resume one job instance.
type BeginRequest struct {
	Definition Definition
	Instance   string
	Steps      []string
}

// FailureKind is bounded durable failure metadata.
type FailureKind string

const (
	// FailureError identifies a returned step or persistence error.
	FailureError FailureKind = "error"
	// FailureCanceled identifies caller cancellation.
	FailureCanceled FailureKind = "canceled"
	// FailurePanic identifies a contained step panic.
	FailurePanic FailureKind = "panic"
)

// Failure releases one active attempt for a later restart. It intentionally
// omits the application error and instance payload from durable metadata.
type Failure struct {
	Attempt Attempt
	Step    string
	Kind    FailureKind
}

// Store owns atomic attempt and checkpoint transitions. An implementation
// defines whether that state survives process restarts.
type Store interface {
	Begin(context.Context, BeginRequest) (Attempt, error)
	Checkpoint(context.Context, Attempt, string) error
	Complete(context.Context, Attempt) error
	Fail(context.Context, Failure) error
}

// ContextFactory creates one fresh bounded context for a failure transition
// after a step context has failed or been canceled.
type ContextFactory func() (context.Context, context.CancelFunc)

// Operation identifies one observed batch boundary.
type Operation string

const (
	// OperationStep identifies one executed step.
	OperationStep Operation = "step"
	// OperationJob identifies one completed or already-complete job attempt.
	OperationJob Operation = "job"
)

// Observation contains bounded job metadata. Instance identities and
// application values are intentionally excluded.
type Observation struct {
	Definition Definition
	Operation  Operation
	Step       string
	Attempt    uint64
	Duration   time.Duration
	Resumed    bool
	Completed  bool
	Err        error
	Panicked   bool
}

// Observer receives completed boundaries synchronously.
type Observer func(context.Context, Observation)

// Runner executes one immutable job at a time through an explicit Store.
type Runner struct {
	store          Store
	failureContext ContextFactory
	observers      []Observer
}

// NewRunner constructs an instance-owned batch runner.
func NewRunner(
	store Store,
	failureContext ContextFactory,
	observers ...Observer,
) (*Runner, error) {
	if nilInterface(store) {
		return nil, errors.New("construct batch runner: store is nil")
	}
	if failureContext == nil {
		return nil, errors.New("construct batch runner: failure context factory is nil")
	}
	for index, observer := range observers {
		if observer == nil {
			return nil, fmt.Errorf(
				"construct batch runner: observer %d is nil",
				index,
			)
		}
	}
	return &Runner{
		store:          store,
		failureContext: failureContext,
		observers:      append([]Observer(nil), observers...),
	}, nil
}

// Result summarizes one execution or restart.
type Result struct {
	Attempt         uint64
	StepsSkipped    int
	StepsCompleted  int
	Resumed         bool
	AlreadyComplete bool
	Duration        time.Duration
}

// Run atomically begins one instance, skips its durable completed prefix,
// executes remaining steps serially, and persists each successful checkpoint.
func (runner *Runner) Run(
	ctx context.Context,
	job *Job,
	instance string,
) (Result, error) {
	started := time.Now()
	var result Result
	if job == nil {
		return result, errors.New("run batch job: job is nil")
	}
	if err := validateRun(ctx, runner, job, instance); err != nil {
		return result, err
	}
	stepIDs := jobStepIDs(job.steps)
	attempt, err := runner.store.Begin(ctx, BeginRequest{
		Definition: job.definition,
		Instance:   instance,
		Steps:      append([]string(nil), stepIDs...),
	})
	if err != nil {
		return result, fmt.Errorf(
			"begin batch job %q: %w",
			job.definition.ID,
			err,
		)
	}
	if err := validateAttempt(job, instance, attempt); err != nil {
		return result, err
	}

	result.Attempt = attempt.number
	result.StepsSkipped = len(attempt.completedSteps)
	result.Resumed = result.StepsSkipped != 0
	if attempt.complete {
		result.AlreadyComplete = true
		result.Duration = time.Since(started)
		runner.observe(ctx, Observation{
			Definition: job.definition,
			Operation:  OperationJob,
			Attempt:    attempt.number,
			Duration:   result.Duration,
			Resumed:    result.Resumed,
			Completed:  true,
		})
		return result, nil
	}

	for _, step := range job.steps[result.StepsSkipped:] {
		if cause := context.Cause(ctx); cause != nil {
			err := fmt.Errorf("run batch job %q: %w", job.definition.ID, cause)
			return result, runner.fail(attempt, step.id, FailureCanceled, err)
		}
		panicked, duration, stepErr := runStep(ctx, job.definition, step)
		runner.observe(ctx, Observation{
			Definition: job.definition,
			Operation:  OperationStep,
			Step:       step.id,
			Attempt:    attempt.number,
			Duration:   duration,
			Resumed:    result.Resumed,
			Completed:  stepErr == nil,
			Err:        stepErr,
			Panicked:   panicked,
		})
		if stepErr != nil {
			kind := FailureError
			if panicked {
				kind = FailurePanic
			} else if errors.Is(stepErr, context.Canceled) ||
				errors.Is(stepErr, context.DeadlineExceeded) {
				kind = FailureCanceled
			}
			return result, runner.fail(attempt, step.id, kind, stepErr)
		}
		if err := runner.store.Checkpoint(ctx, attempt, step.id); err != nil {
			checkpointErr := fmt.Errorf(
				"checkpoint batch job %q step %q: %w",
				job.definition.ID,
				step.id,
				err,
			)
			return result, runner.fail(
				attempt,
				step.id,
				FailureError,
				checkpointErr,
			)
		}
		result.StepsCompleted++
	}
	if err := runner.store.Complete(ctx, attempt); err != nil {
		completionErr := fmt.Errorf(
			"complete batch job %q: %w",
			job.definition.ID,
			err,
		)
		return result, runner.fail(
			attempt,
			job.steps[len(job.steps)-1].id,
			FailureError,
			completionErr,
		)
	}
	result.Duration = time.Since(started)
	runner.observe(ctx, Observation{
		Definition: job.definition,
		Operation:  OperationJob,
		Attempt:    attempt.number,
		Duration:   result.Duration,
		Resumed:    result.Resumed,
		Completed:  true,
	})
	return result, nil
}

// PanicError reports a contained step panic without exposing its recovered
// value.
type PanicError struct {
	Definition Definition
	Step       string
}

// Error describes the failed step.
func (err *PanicError) Error() string {
	if err == nil {
		return ErrPanicked.Error()
	}
	return fmt.Sprintf(
		"run batch job %q step %q: %v",
		err.Definition.ID,
		err.Step,
		ErrPanicked,
	)
}

// Unwrap supports errors.Is(err, ErrPanicked).
func (err *PanicError) Unwrap() error {
	return ErrPanicked
}

func runStep(
	ctx context.Context,
	definition Definition,
	step Step,
) (panicked bool, duration time.Duration, err error) {
	started := time.Now()
	defer func() {
		duration = time.Since(started)
		if recover() != nil {
			err = &PanicError{Definition: definition, Step: step.id}
			panicked = true
		}
	}()
	err = step.run(ctx)
	if err != nil {
		err = fmt.Errorf(
			"run batch job %q step %q: %w",
			definition.ID,
			step.id,
			err,
		)
	}
	return
}

func (runner *Runner) fail(
	attempt Attempt,
	step string,
	kind FailureKind,
	cause error,
) error {
	ctx, cancel := runner.failureContext()
	if cancel == nil {
		return errors.Join(
			cause,
			errors.New("record batch failure: context cancel function is nil"),
		)
	}
	defer cancel()
	if ctx == nil {
		return errors.Join(
			cause,
			errors.New("record batch failure: context is nil"),
		)
	}
	if contextCause := context.Cause(ctx); contextCause != nil {
		return errors.Join(
			cause,
			fmt.Errorf("record batch failure: %w", contextCause),
		)
	}
	if err := runner.store.Fail(ctx, Failure{
		Attempt: attempt,
		Step:    step,
		Kind:    kind,
	}); err != nil {
		return errors.Join(
			cause,
			fmt.Errorf("record batch failure: %w", err),
		)
	}
	return cause
}

func validateRun(
	ctx context.Context,
	runner *Runner,
	job *Job,
	instance string,
) error {
	switch {
	case ctx == nil:
		return errors.New("run batch job: context is nil")
	case runner == nil || nilInterface(runner.store):
		return errors.New("run batch job: runner is nil")
	case !validMetadata(instance):
		return errors.New("run batch job: instance is invalid")
	default:
		if cause := context.Cause(ctx); cause != nil {
			return fmt.Errorf("run batch job %q: %w", job.definition.ID, cause)
		}
		return nil
	}
}

func validateAttempt(job *Job, instance string, attempt Attempt) error {
	switch {
	case attempt.definition != job.definition:
		return fmt.Errorf(
			"begin batch job %q: store returned a different definition",
			job.definition.ID,
		)
	case attempt.instance != instance:
		return fmt.Errorf(
			"begin batch job %q: store returned a different instance",
			job.definition.ID,
		)
	case attempt.number == 0:
		return fmt.Errorf(
			"begin batch job %q: store returned attempt zero",
			job.definition.ID,
		)
	case len(attempt.completedSteps) > len(job.steps):
		return fmt.Errorf(
			"begin batch job %q: store returned too many completed steps",
			job.definition.ID,
		)
	}
	for index, completed := range attempt.completedSteps {
		if completed != job.steps[index].id {
			return fmt.Errorf(
				"begin batch job %q: completed steps are not an exact job prefix",
				job.definition.ID,
			)
		}
	}
	if attempt.complete && len(attempt.completedSteps) != len(job.steps) {
		return fmt.Errorf(
			"begin batch job %q: completed attempt has pending steps",
			job.definition.ID,
		)
	}
	return nil
}

func validateDefinition(definition Definition) error {
	switch {
	case !validMetadata(definition.ID):
		return errors.New("construct batch job: job ID is invalid")
	case !validMetadata(definition.Module):
		return fmt.Errorf(
			"construct batch job %q: module is invalid",
			definition.ID,
		)
	default:
		return nil
	}
}

func jobStepIDs(steps []Step) []string {
	result := make([]string, len(steps))
	for index, step := range steps {
		result[index] = step.id
	}
	return result
}

func validMetadata(value string) bool {
	if value == "" ||
		len(value) > maxMetadataBytes ||
		strings.TrimSpace(value) != value ||
		!utf8.ValidString(value) {
		return false
	}
	for _, character := range []byte(value) {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}

func (runner *Runner) observe(ctx context.Context, observation Observation) {
	for _, observer := range runner.observers {
		observer(ctx, observation)
	}
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() { //nolint:exhaustive // Only nil-capable reflection kinds require explicit handling.
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
