package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	spicemigration "github.com/spice-framework/spice/migration"
)

var errMigrationTest = errors.New("migration test failure")

func TestPostgreSQLMigrationBackendAppliesScriptsAtomicallyAndReconciles(t *testing.T) {
	t.Parallel()

	plan, err := spicemigration.NewPlan([]spicemigration.Spec{{
		Version: 18446744073709551615,
		Module:  "example.com/shop/orders",
		Name:    "create orders",
		SQL:     "CREATE TABLE orders (id bigint);\nINSERT INTO orders VALUES (1);",
	}})
	if err != nil {
		t.Fatalf("construct plan: %v", err)
	}
	connection := newFakeMigrationConnection()
	backend := &MigrationBackend{schema: "tenant_a", lockID: 42}
	runner, err := spicemigration.NewRunner(migrationBackendFunc(func(
		ctx context.Context,
		work func(context.Context, spicemigration.Session) error,
	) error {
		return backend.runLocked(ctx, connection, work)
	}))
	if err != nil {
		t.Fatalf("construct runner: %v", err)
	}

	result, err := runner.Run(context.Background(), plan)
	if err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	if result != (spicemigration.Result{Applied: 1}) {
		t.Fatalf("first result = %#v", result)
	}
	wantEvents := []string{
		"lock:42",
		"registry:tenant_a",
		"read",
		"begin",
		"script:CREATE TABLE orders (id bigint);\nINSERT INTO orders VALUES (1);",
		"record:18446744073709551615",
		"commit",
		"unlock:42",
	}
	if got := strings.Join(connection.events, "\n"); got != strings.Join(wantEvents, "\n") {
		t.Fatalf("events:\n%s", got)
	}
	if len(connection.applied) != 1 ||
		connection.applied[0].Version != 18446744073709551615 ||
		connection.applied[0].Module != "example.com/shop/orders" ||
		connection.applied[0].Name != "create orders" {
		t.Fatalf("applied = %#v", connection.applied)
	}

	connection.events = nil
	result, err = runner.Run(context.Background(), plan)
	if err != nil {
		t.Fatalf("reconcile migrations: %v", err)
	}
	if result != (spicemigration.Result{Current: 1}) {
		t.Fatalf("second result = %#v", result)
	}
	if got := strings.Join(connection.events, ","); got !=
		"lock:42,registry:tenant_a,read,unlock:42" {
		t.Fatalf("reconciliation events = %q", got)
	}
	session := &postgresMigrationSession{
		connection: connection,
		registry:   backend.registryName(),
	}
	records, err := session.Applied(context.Background())
	if err != nil {
		t.Fatalf("read normalized registry: %v", err)
	}
	if len(records) != 1 || records[0].AppliedAt.Location() != time.UTC {
		t.Fatalf("normalized registry = %#v", records)
	}
}

func TestPostgreSQLMigrationBackendRollsBackFailures(t *testing.T) {
	t.Parallel()

	plan, err := spicemigration.NewPlan([]spicemigration.Spec{{
		Version: 1,
		Module:  "orders",
		Name:    "create orders",
		SQL:     "SELECT 1;",
	}})
	if err != nil {
		t.Fatalf("construct plan: %v", err)
	}
	tests := []struct {
		name      string
		configure func(*fakeMigrationConnection)
		wantEvent string
	}{
		{
			name: "script",
			configure: func(connection *fakeMigrationConnection) {
				connection.scriptErr = errMigrationTest
			},
			wantEvent: "script:SELECT 1;",
		},
		{
			name: "record",
			configure: func(connection *fakeMigrationConnection) {
				connection.recordErr = errMigrationTest
			},
			wantEvent: "record:1",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			connection := newFakeMigrationConnection()
			test.configure(connection)
			backend := &MigrationBackend{schema: "public", lockID: 7}
			runner, runnerErr := spicemigration.NewRunner(migrationBackendFunc(func(
				ctx context.Context,
				work func(context.Context, spicemigration.Session) error,
			) error {
				return backend.runLocked(ctx, connection, work)
			}))
			if runnerErr != nil {
				t.Fatalf("construct runner: %v", runnerErr)
			}
			if _, runErr := runner.Run(context.Background(), plan); !errors.Is(runErr, errMigrationTest) {
				t.Fatalf("run error = %v", runErr)
			}
			events := strings.Join(connection.events, ",")
			if !strings.Contains(events, test.wantEvent) ||
				!strings.Contains(events, "rollback") ||
				!strings.HasSuffix(events, "unlock:7") ||
				len(connection.applied) != 0 {
				t.Fatalf("events = %q, applied = %#v", events, connection.applied)
			}
		})
	}
}

func TestPostgreSQLMigrationBackendLockAndRegistryFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		configure      func(*fakeMigrationConnection)
		workErr        error
		wantClosed     bool
		wantUnlockCall bool
		wantWorkCalls  int
	}{
		{
			name: "acquire",
			configure: func(connection *fakeMigrationConnection) {
				connection.lockErr = errMigrationTest
			},
		},
		{
			name: "registry",
			configure: func(connection *fakeMigrationConnection) {
				connection.registryErr = errMigrationTest
			},
			wantUnlockCall: true,
		},
		{
			name:           "work",
			configure:      func(*fakeMigrationConnection) {},
			workErr:        errMigrationTest,
			wantUnlockCall: true,
			wantWorkCalls:  1,
		},
		{
			name: "unlock error",
			configure: func(connection *fakeMigrationConnection) {
				connection.unlockErr = errMigrationTest
			},
			workErr:        errMigrationTest,
			wantClosed:     true,
			wantUnlockCall: true,
			wantWorkCalls:  1,
		},
		{
			name: "lock not owned",
			configure: func(connection *fakeMigrationConnection) {
				connection.unlocked = false
			},
			wantClosed:     true,
			wantUnlockCall: true,
			wantWorkCalls:  1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			connection := newFakeMigrationConnection()
			test.configure(connection)
			backend := &MigrationBackend{schema: "public", lockID: 9}
			workCalls := 0
			err := backend.runLocked(
				context.Background(),
				connection,
				func(context.Context, spicemigration.Session) error {
					workCalls++
					return test.workErr
				},
			)
			if err == nil {
				t.Fatal("runLocked() error = nil")
			}
			if test.workErr != nil && !errors.Is(err, test.workErr) {
				t.Fatalf("runLocked() error = %v", err)
			}
			if test.name != "lock not owned" && !errors.Is(err, errMigrationTest) {
				t.Fatalf("runLocked() error = %v", err)
			}
			if workCalls != test.wantWorkCalls ||
				connection.closed != test.wantClosed ||
				(connection.unlockCalls > 0) != test.wantUnlockCall {
				t.Fatalf(
					"work=%d closed=%t unlockCalls=%d events=%#v",
					workCalls,
					connection.closed,
					connection.unlockCalls,
					connection.events,
				)
			}
		})
	}
}

func TestPostgreSQLMigrationSessionRejectsInvalidRegistryAndMigration(t *testing.T) {
	t.Parallel()

	connection := newFakeMigrationConnection()
	connection.applied = []spicemigration.Applied{{
		Version:   1,
		Module:    "orders",
		Name:      "orders",
		Checksum:  strings.Repeat("0", 64),
		AppliedAt: time.Now(),
	}}
	connection.versionText = "01"
	session := &postgresMigrationSession{
		connection: connection,
		registry:   `"public"."spice_schema_history"`,
	}
	if _, err := session.Applied(context.Background()); err == nil {
		t.Fatal("Applied() accepted a non-canonical version")
	}
	if _, err := session.Applied(nilMigrationContext()); err == nil {
		t.Fatal("Applied() accepted a nil context")
	}
	if err := session.Apply(context.Background(), spicemigration.Migration{}); err == nil {
		t.Fatal("Apply() accepted a zero migration")
	}
	if err := session.Apply(nilMigrationContext(), spicemigration.Migration{}); err == nil {
		t.Fatal("Apply() accepted a nil context")
	}
	if len(connection.events) != 1 {
		t.Fatalf("unexpected database work: %#v", connection.events)
	}
}

func TestPostgreSQLMigrationBackendConfiguration(t *testing.T) {
	t.Parallel()

	if _, err := NewMigrationBackend(nil, MigrationOptions{}); err == nil {
		t.Fatal("NewMigrationBackend(nil) error = nil")
	}
	database, err := Open(Options{
		URL:           "postgres://spice:secret@127.0.0.1:1/spice?sslmode=disable",
		AllowInsecure: true,
	})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := database.Close(); closeErr != nil {
			t.Errorf("close database: %v", closeErr)
		}
	})
	backend, err := NewMigrationBackend(database, MigrationOptions{})
	if err != nil {
		t.Fatalf("construct backend: %v", err)
	}
	if backend.schema != defaultMigrationSchema ||
		backend.lockID != defaultMigrationLockID ||
		backend.registryName() != `"public"."spice_schema_history"` {
		t.Fatalf("backend = %#v", backend)
	}
	explicit, err := NewMigrationBackend(database, MigrationOptions{
		Schema: "Orders_2026",
		LockID: -42,
	})
	if err != nil {
		t.Fatalf("construct explicit backend: %v", err)
	}
	if explicit.schema != "Orders_2026" || explicit.lockID != -42 {
		t.Fatalf("explicit backend = %#v", explicit)
	}

	for _, schema := range []string{
		"9orders",
		"orders-schema",
		`orders"`,
		strings.Repeat("x", maxPostgreSQLIdentifierBytes+1),
	} {
		if _, err := NewMigrationBackend(database, MigrationOptions{Schema: schema}); err == nil {
			t.Fatalf("NewMigrationBackend(schema %q) error = nil", schema)
		}
	}
	if err := backend.RunLocked(nilMigrationContext(), func(
		context.Context,
		spicemigration.Session,
	) error {
		return nil
	}); err == nil {
		t.Fatal("RunLocked(nil context) error = nil")
	}
	if err := backend.RunLocked(context.Background(), nil); err == nil {
		t.Fatal("RunLocked(nil work) error = nil")
	}
	if err := (*MigrationBackend)(nil).RunLocked(context.Background(), func(
		context.Context,
		spicemigration.Session,
	) error {
		return nil
	}); err == nil {
		t.Fatal("nil RunLocked() error = nil")
	}
}

type migrationBackendFunc func(
	context.Context,
	func(context.Context, spicemigration.Session) error,
) error

func (backend migrationBackendFunc) RunLocked(
	ctx context.Context,
	work func(context.Context, spicemigration.Session) error,
) error {
	return backend(ctx, work)
}

type fakeMigrationConnection struct {
	events       []string
	applied      []spicemigration.Applied
	pending      *spicemigration.Applied
	versionText  string
	lockErr      error
	registryErr  error
	queryErr     error
	rowsErr      error
	beginErr     error
	scriptErr    error
	recordErr    error
	commitErr    error
	rollbackErr  error
	unlockErr    error
	closeErr     error
	unlocked     bool
	unlockCalls  int
	closed       bool
	activeTx     bool
	rollbackSeen bool
}

func newFakeMigrationConnection() *fakeMigrationConnection {
	return &fakeMigrationConnection{unlocked: true}
}

func (connection *fakeMigrationConnection) Exec(
	_ context.Context,
	statement string,
	arguments ...any,
) error {
	switch {
	case statement == acquireMigrationLockSQL:
		if len(arguments) != 1 {
			return fmt.Errorf("lock arguments = %d, want 1", len(arguments))
		}
		connection.events = append(connection.events, fmt.Sprintf("lock:%v", arguments[0]))
		return connection.lockErr
	case strings.HasPrefix(statement, "CREATE TABLE"):
		schema := "public"
		if strings.Contains(statement, `"tenant_a"`) {
			schema = "tenant_a"
		}
		connection.events = append(connection.events, "registry:"+schema)
		return connection.registryErr
	default:
		return fmt.Errorf("unexpected statement: %s", statement)
	}
}

func (connection *fakeMigrationConnection) Query(
	context.Context,
	string,
	...any,
) (migrationRows, error) {
	connection.events = append(connection.events, "read")
	if connection.queryErr != nil {
		return nil, connection.queryErr
	}
	return &fakeMigrationRows{connection: connection}, nil
}

func (connection *fakeMigrationConnection) QueryBool(
	_ context.Context,
	statement string,
	arguments ...any,
) (bool, error) {
	if statement != releaseMigrationLockSQL {
		return false, errors.New("unexpected boolean query")
	}
	if len(arguments) != 1 {
		return false, fmt.Errorf("unlock arguments = %d, want 1", len(arguments))
	}
	connection.unlockCalls++
	connection.events = append(connection.events, fmt.Sprintf("unlock:%v", arguments[0]))
	return connection.unlocked, connection.unlockErr
}

func (connection *fakeMigrationConnection) Begin(
	context.Context,
) (migrationTransaction, error) {
	connection.events = append(connection.events, "begin")
	if connection.beginErr != nil {
		return nil, connection.beginErr
	}
	connection.activeTx = true
	return &fakeMigrationTransaction{connection: connection}, nil
}

func (connection *fakeMigrationConnection) Close(context.Context) error {
	connection.closed = true
	connection.events = append(connection.events, "close")
	return connection.closeErr
}

type fakeMigrationRows struct {
	connection *fakeMigrationConnection
	index      int
}

func (rows *fakeMigrationRows) Next() bool {
	return rows.index < len(rows.connection.applied)
}

func (rows *fakeMigrationRows) Scan(destinations ...any) error {
	if len(destinations) != 5 {
		return fmt.Errorf("scan destinations = %d, want 5", len(destinations))
	}
	versionDestination, versionOK := destinations[0].(*string)
	moduleDestination, moduleOK := destinations[1].(*string)
	nameDestination, nameOK := destinations[2].(*string)
	checksumDestination, checksumOK := destinations[3].(*string)
	appliedAtDestination, appliedAtOK := destinations[4].(*time.Time)
	if !versionOK || !moduleOK || !nameOK || !checksumOK || !appliedAtOK {
		return errors.New("scan destination type is invalid")
	}
	record := rows.connection.applied[rows.index]
	version := fmt.Sprintf("%d", record.Version)
	if rows.connection.versionText != "" {
		version = rows.connection.versionText
	}
	*versionDestination = version
	*moduleDestination = record.Module
	*nameDestination = record.Name
	*checksumDestination = record.Checksum
	*appliedAtDestination = record.AppliedAt
	rows.index++
	return nil
}

func (rows *fakeMigrationRows) Err() error {
	return rows.connection.rowsErr
}

func (*fakeMigrationRows) Close() {}

type fakeMigrationTransaction struct {
	connection *fakeMigrationConnection
}

func (transaction *fakeMigrationTransaction) ExecScript(
	_ context.Context,
	statement string,
) error {
	transaction.connection.events = append(transaction.connection.events, "script:"+statement)
	return transaction.connection.scriptErr
}

func (transaction *fakeMigrationTransaction) Exec(
	_ context.Context,
	_ string,
	arguments ...any,
) error {
	if len(arguments) != 4 {
		return fmt.Errorf("record arguments = %d, want 4", len(arguments))
	}
	transaction.connection.events = append(
		transaction.connection.events,
		fmt.Sprintf("record:%v", arguments[0]),
	)
	if transaction.connection.recordErr != nil {
		return transaction.connection.recordErr
	}
	versionText, versionOK := arguments[0].(string)
	module, moduleOK := arguments[1].(string)
	name, nameOK := arguments[2].(string)
	checksum, checksumOK := arguments[3].(string)
	if !versionOK || !moduleOK || !nameOK || !checksumOK {
		return errors.New("record argument type is invalid")
	}
	version, err := parseMigrationVersion(versionText)
	if err != nil {
		return err
	}
	transaction.connection.pending = &spicemigration.Applied{
		Version:   version,
		Module:    module,
		Name:      name,
		Checksum:  checksum,
		AppliedAt: time.Date(2026, time.July, 25, 16, 0, 0, 0, time.FixedZone("test", 3600)),
	}
	return nil
}

func (transaction *fakeMigrationTransaction) Commit(context.Context) error {
	transaction.connection.events = append(transaction.connection.events, "commit")
	if transaction.connection.commitErr != nil {
		return transaction.connection.commitErr
	}
	transaction.connection.applied = append(
		transaction.connection.applied,
		*transaction.connection.pending,
	)
	transaction.connection.pending = nil
	transaction.connection.activeTx = false
	return nil
}

func (transaction *fakeMigrationTransaction) Rollback(context.Context) error {
	transaction.connection.events = append(transaction.connection.events, "rollback")
	transaction.connection.pending = nil
	transaction.connection.activeTx = false
	transaction.connection.rollbackSeen = true
	return transaction.connection.rollbackErr
}

func nilMigrationContext() context.Context {
	return nil
}
