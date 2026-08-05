// Package migration provides deterministic, module-owned database migration
// plans with checksum drift detection and dialect-owned execution.
package migration

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"
)

const maxMetadataBytes = 512

// Spec is the inspectable input to NewPlan. Versions are application-global,
// monotonically increasing identifiers.
type Spec struct {
	Version uint64
	Module  string
	Name    string
	SQL     string
}

// Migration is one immutable normalized plan entry.
type Migration struct {
	version  uint64
	module   string
	name     string
	sql      string
	checksum string
}

// Version returns the application-global migration version.
func (migration Migration) Version() uint64 {
	return migration.version
}

// Module returns the owning application module.
func (migration Migration) Module() string {
	return migration.module
}

// Name returns the stable human-readable migration name.
func (migration Migration) Name() string {
	return migration.name
}

// SQL returns normalized executable SQL.
func (migration Migration) SQL() string {
	return migration.sql
}

// Checksum returns the lowercase SHA-256 checksum of normalized SQL.
func (migration Migration) Checksum() string {
	return migration.checksum
}

// Plan is an immutable migration sequence ordered by global version.
type Plan struct {
	migrations []Migration
}

// NewPlan validates, normalizes, checksums, and orders module-owned migrations.
func NewPlan(specs []Spec) (*Plan, error) {
	migrations := make([]Migration, 0, len(specs))
	versions := make(map[uint64]string, len(specs))
	for index, spec := range specs {
		migration, err := newMigration(spec)
		if err != nil {
			return nil, fmt.Errorf("construct migration plan: entry %d: %w", index, err)
		}
		if prior, duplicate := versions[migration.version]; duplicate {
			return nil, fmt.Errorf(
				"construct migration plan: version %d is shared by modules %q and %q",
				migration.version,
				prior,
				migration.module,
			)
		}
		versions[migration.version] = migration.module
		migrations = append(migrations, migration)
	}
	slices.SortFunc(migrations, func(left, right Migration) int {
		return cmp.Compare(left.version, right.version)
	})
	return &Plan{migrations: migrations}, nil
}

// Migrations returns a defensive copy of the deterministic sequence.
func (plan *Plan) Migrations() []Migration {
	if plan == nil {
		return []Migration{}
	}
	return append([]Migration(nil), plan.migrations...)
}

// Applied is one durable migration registry record.
type Applied struct {
	Version   uint64
	Module    string
	Name      string
	Checksum  string
	AppliedAt time.Time
}

// Session executes while a Backend holds its migration lock. Apply must
// atomically execute the SQL and persist its exact metadata.
type Session interface {
	Applied(context.Context) ([]Applied, error)
	Apply(context.Context, Migration) error
}

// Backend owns dialect-specific locking and transaction policy. RunLocked must
// invoke work exactly once while concurrent migration runners are excluded.
type Backend interface {
	RunLocked(context.Context, func(context.Context, Session) error) error
}

// Observation contains bounded metadata and no SQL text.
type Observation struct {
	Version  uint64
	Module   string
	Name     string
	Duration time.Duration
	Err      error
}

// Observer receives completed migration attempts synchronously.
type Observer func(context.Context, Observation)

// Runner reconciles and applies one immutable plan.
type Runner struct {
	backend   Backend
	observers []Observer
}

// NewRunner constructs an instance-owned migration runner.
func NewRunner(backend Backend, observers ...Observer) (*Runner, error) {
	if nilInterface(backend) {
		return nil, errors.New("construct migration runner: backend is nil")
	}
	for index, observer := range observers {
		if observer == nil {
			return nil, fmt.Errorf("construct migration runner: observer %d is nil", index)
		}
	}
	return &Runner{
		backend:   backend,
		observers: append([]Observer(nil), observers...),
	}, nil
}

// Result summarizes one locked migration run.
type Result struct {
	Current int
	Applied int
}

// Run validates the durable registry as an exact plan prefix, then applies
// pending migrations sequentially under the backend lock.
func (runner *Runner) Run(ctx context.Context, plan *Plan) (Result, error) {
	var result Result
	if ctx == nil {
		return result, errors.New("run migrations: context is nil")
	}
	if runner == nil || nilInterface(runner.backend) {
		return result, errors.New("run migrations: runner is nil")
	}
	if plan == nil {
		return result, errors.New("run migrations: plan is nil")
	}
	invocations := 0
	err := runner.backend.RunLocked(ctx, func(lockedContext context.Context, session Session) error {
		invocations++
		if invocations > 1 {
			return errors.New("run migrations: backend invoked work more than once")
		}
		if lockedContext == nil {
			return errors.New("run migrations: backend supplied nil context")
		}
		if nilInterface(session) {
			return errors.New("run migrations: backend supplied nil session")
		}
		applied, err := session.Applied(lockedContext)
		if err != nil {
			return fmt.Errorf("read applied migrations: %w", err)
		}
		if err := reconcile(plan.migrations, applied); err != nil {
			return err
		}
		result.Current = len(applied)
		for _, migration := range plan.migrations[len(applied):] {
			if cause := context.Cause(lockedContext); cause != nil {
				return fmt.Errorf("run migrations: %w", cause)
			}
			if err := runner.apply(lockedContext, session, migration); err != nil {
				return err
			}
			result.Applied++
		}
		return nil
	})
	if err != nil {
		return result, fmt.Errorf("run migrations under lock: %w", err)
	}
	if invocations != 1 {
		return result, errors.New("run migrations: backend did not invoke work")
	}
	return result, nil
}

func (runner *Runner) apply(
	ctx context.Context,
	session Session,
	migration Migration,
) error {
	started := time.Now()
	err := session.Apply(ctx, migration)
	if err != nil {
		err = fmt.Errorf(
			"apply migration %d (%s): %w",
			migration.version,
			migration.module,
			err,
		)
	}
	observation := Observation{
		Version:  migration.version,
		Module:   migration.module,
		Name:     migration.name,
		Duration: time.Since(started),
		Err:      err,
	}
	for _, observer := range runner.observers {
		observer(ctx, observation)
	}
	return err
}

func reconcile(plan []Migration, applied []Applied) error {
	if len(applied) > len(plan) {
		return fmt.Errorf(
			"reconcile migrations: registry contains %d entries but plan contains %d",
			len(applied),
			len(plan),
		)
	}
	for index, record := range applied {
		expected := plan[index]
		if record.Version != expected.version {
			return fmt.Errorf(
				"reconcile migrations: registry version %d at position %d, want %d",
				record.Version,
				index,
				expected.version,
			)
		}
		if record.Module != expected.module || record.Name != expected.name {
			return fmt.Errorf(
				"reconcile migration %d: identity changed from %s/%s to %s/%s",
				record.Version,
				record.Module,
				record.Name,
				expected.module,
				expected.name,
			)
		}
		if record.Checksum != expected.checksum {
			return fmt.Errorf("reconcile migration %d: checksum drift detected", record.Version)
		}
		if record.AppliedAt.IsZero() {
			return fmt.Errorf("reconcile migration %d: applied time is missing", record.Version)
		}
	}
	return nil
}

func newMigration(spec Spec) (Migration, error) {
	if spec.Version == 0 {
		return Migration{}, errors.New("version must be positive")
	}
	if err := validateMetadata("module", spec.Module); err != nil {
		return Migration{}, err
	}
	if err := validateMetadata("name", spec.Name); err != nil {
		return Migration{}, err
	}
	sqlText := strings.ReplaceAll(spec.SQL, "\r\n", "\n")
	if strings.ContainsRune(sqlText, '\r') {
		return Migration{}, errors.New("SQL contains an unsupported carriage return")
	}
	if strings.TrimSpace(sqlText) == "" {
		return Migration{}, errors.New("SQL is empty")
	}
	checksum := sha256.Sum256([]byte(sqlText))
	return Migration{
		version:  spec.Version,
		module:   spec.Module,
		name:     spec.Name,
		sql:      sqlText,
		checksum: hex.EncodeToString(checksum[:]),
	}, nil
}

func validateMetadata(name, value string) error {
	if value == "" ||
		strings.TrimSpace(value) != value ||
		len(value) > maxMetadataBytes {
		return fmt.Errorf(
			"%s must be between 1 and %d bytes with no surrounding space",
			name,
			maxMetadataBytes,
		)
	}
	return nil
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() { //nolint:exhaustive // Only nil-capable reflection kinds require explicit handling.
	case reflect.Chan, reflect.Func, reflect.Interface,
		reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
