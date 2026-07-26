package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	spicemigration "github.com/StevenBuglione/spice/migration"
)

const (
	defaultMigrationSchema = "public"
	// defaultMigrationLockID is the signed big-endian representation of
	// "SPICEMIG". It is stable across processes and releases.
	defaultMigrationLockID       int64 = 0x53504943454d4947
	migrationCleanupTimeout            = 5 * time.Second
	maxPostgreSQLIdentifierBytes       = 63
)

const (
	acquireMigrationLockSQL = `SELECT pg_advisory_lock($1)`
	releaseMigrationLockSQL = `SELECT pg_advisory_unlock($1)`
)

// MigrationOptions configures the PostgreSQL implementation of the core
// migration backend. Schema must already exist. LockID zero selects Spice's
// stable process-independent default advisory lock.
type MigrationOptions struct {
	Schema string
	LockID int64
}

// MigrationBackend applies module-owned migration plans through one
// caller-owned PostgreSQL pool.
type MigrationBackend struct {
	database *sql.DB
	schema   string
	lockID   int64
}

var _ spicemigration.Backend = (*MigrationBackend)(nil)

// NewMigrationBackend constructs a PostgreSQL migration backend without
// connecting. The backend creates its fixed registry table on first use.
func NewMigrationBackend(
	database *sql.DB,
	options MigrationOptions,
) (*MigrationBackend, error) {
	if database == nil {
		return nil, errors.New("construct PostgreSQL migration backend: database is nil")
	}
	schema := options.Schema
	if schema == "" {
		schema = defaultMigrationSchema
	}
	if !validPostgreSQLIdentifier(schema) {
		return nil, errors.New("construct PostgreSQL migration backend: schema is invalid")
	}
	lockID := options.LockID
	if lockID == 0 {
		lockID = defaultMigrationLockID
	}
	return &MigrationBackend{
		database: database,
		schema:   schema,
		lockID:   lockID,
	}, nil
}

// RunLocked pins one pgx connection, obtains a session-level advisory lock,
// and invokes work exactly once. Unlock failure closes the physical connection
// so a leaked session lock can never return to the pool.
func (backend *MigrationBackend) RunLocked(
	ctx context.Context,
	work func(context.Context, spicemigration.Session) error,
) error {
	switch {
	case ctx == nil:
		return errors.New("run PostgreSQL migrations: context is nil")
	case backend == nil || backend.database == nil:
		return errors.New("run PostgreSQL migrations: backend is nil")
	case work == nil:
		return errors.New("run PostgreSQL migrations: work is nil")
	}

	sqlConnection, err := backend.database.Conn(ctx)
	if err != nil {
		return fmt.Errorf("run PostgreSQL migrations: acquire connection: %w", err)
	}
	rawErr := sqlConnection.Raw(func(driverConnection any) error {
		connection, ok := driverConnection.(*stdlib.Conn)
		if !ok || connection.Conn() == nil {
			return errors.New("PostgreSQL migration backend requires a pgx database")
		}
		return backend.runLocked(ctx, pgxConnection{connection: connection.Conn()}, work)
	})
	closeErr := sqlConnection.Close()
	if err := errors.Join(rawErr, closeErr); err != nil {
		return fmt.Errorf("run PostgreSQL migrations: %w", err)
	}
	return nil
}

func (backend *MigrationBackend) runLocked(
	ctx context.Context,
	connection migrationConnection,
	work func(context.Context, spicemigration.Session) error,
) (err error) {
	if execErr := connection.Exec(ctx, acquireMigrationLockSQL, backend.lockID); execErr != nil {
		return fmt.Errorf("acquire advisory lock: %w", execErr)
	}
	defer func() {
		err = errors.Join(err, backend.releaseLock(ctx, connection))
	}()

	if execErr := connection.Exec(ctx, backend.registryDDL()); execErr != nil {
		return fmt.Errorf("create migration registry: %w", execErr)
	}
	return work(ctx, &postgresMigrationSession{
		connection: connection,
		registry:   backend.registryName(),
	})
}

func (backend *MigrationBackend) releaseLock(
	ctx context.Context,
	connection migrationConnection,
) error {
	cleanupContext, cancel := context.WithTimeout(
		context.WithoutCancel(ctx),
		migrationCleanupTimeout,
	)
	defer cancel()

	unlocked, err := connection.QueryBool(
		cleanupContext,
		releaseMigrationLockSQL,
		backend.lockID,
	)
	if err == nil && unlocked {
		return nil
	}
	closeErr := connection.Close(cleanupContext)
	switch {
	case err != nil:
		return fmt.Errorf(
			"release advisory lock: %w",
			errors.Join(err, closeErr),
		)
	case closeErr != nil:
		return fmt.Errorf("release advisory lock: lock was not owned: %w", closeErr)
	default:
		return errors.New("release advisory lock: lock was not owned")
	}
}

func (backend *MigrationBackend) registryName() string {
	return `"` + backend.schema + `"."spice_schema_history"`
}

func (backend *MigrationBackend) registryDDL() string {
	return `CREATE TABLE IF NOT EXISTS ` + backend.registryName() + ` (
	version numeric(20, 0) PRIMARY KEY
		CHECK (version >= 1 AND version <= 18446744073709551615),
	module text NOT NULL
		CHECK (octet_length(module) BETWEEN 1 AND 512),
	name text NOT NULL
		CHECK (octet_length(name) BETWEEN 1 AND 512),
	checksum text NOT NULL
		CHECK (checksum ~ '^[0-9a-f]{64}$'),
	applied_at timestamp with time zone NOT NULL DEFAULT statement_timestamp()
)`
}

type postgresMigrationSession struct {
	connection migrationConnection
	registry   string
}

func (session *postgresMigrationSession) Applied(
	ctx context.Context,
) ([]spicemigration.Applied, error) {
	if ctx == nil {
		return nil, errors.New("read PostgreSQL migration registry: context is nil")
	}
	rows, err := session.connection.Query(
		ctx,
		`SELECT version::text, module, name, checksum, applied_at
FROM `+session.registry+`
ORDER BY version`,
	)
	if err != nil {
		return nil, fmt.Errorf("read PostgreSQL migration registry: %w", err)
	}
	defer rows.Close()

	applied := make([]spicemigration.Applied, 0)
	for rows.Next() {
		var (
			versionText string
			record      spicemigration.Applied
		)
		if scanErr := rows.Scan(
			&versionText,
			&record.Module,
			&record.Name,
			&record.Checksum,
			&record.AppliedAt,
		); scanErr != nil {
			return nil, fmt.Errorf("read PostgreSQL migration registry row: %w", scanErr)
		}
		version, parseErr := parseMigrationVersion(versionText)
		if parseErr != nil {
			return nil, parseErr
		}
		record.Version = version
		record.AppliedAt = record.AppliedAt.UTC()
		applied = append(applied, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read PostgreSQL migration registry rows: %w", err)
	}
	return applied, nil
}

func (session *postgresMigrationSession) Apply(
	ctx context.Context,
	entry spicemigration.Migration,
) error {
	if ctx == nil {
		return errors.New("apply PostgreSQL migration: context is nil")
	}
	if err := validateMigration(entry); err != nil {
		return err
	}
	transaction, err := session.connection.Begin(ctx)
	if err != nil {
		return fmt.Errorf("apply PostgreSQL migration: begin transaction: %w", err)
	}
	rollback := func(cause error) error {
		cleanupContext, cancel := context.WithTimeout(
			context.WithoutCancel(ctx),
			migrationCleanupTimeout,
		)
		defer cancel()
		rollbackErr := transaction.Rollback(cleanupContext)
		if errors.Is(rollbackErr, pgx.ErrTxClosed) {
			rollbackErr = nil
		}
		return errors.Join(cause, rollbackErr)
	}

	if err := transaction.ExecScript(ctx, entry.SQL()); err != nil {
		return fmt.Errorf(
			"apply PostgreSQL migration: execute SQL: %w",
			rollback(err),
		)
	}
	if err := transaction.Exec(
		ctx,
		`INSERT INTO `+session.registry+`
	(version, module, name, checksum)
VALUES ($1::numeric, $2, $3, $4)`,
		fmt.Sprintf("%d", entry.Version()),
		entry.Module(),
		entry.Name(),
		entry.Checksum(),
	); err != nil {
		return fmt.Errorf(
			"apply PostgreSQL migration: record metadata: %w",
			rollback(err),
		)
	}
	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf("apply PostgreSQL migration: commit transaction: %w", err)
	}
	return nil
}

func validateMigration(entry spicemigration.Migration) error {
	switch {
	case entry.Version() == 0:
		return errors.New("apply PostgreSQL migration: version is invalid")
	case strings.TrimSpace(entry.Module()) == "":
		return errors.New("apply PostgreSQL migration: module is invalid")
	case strings.TrimSpace(entry.Name()) == "":
		return errors.New("apply PostgreSQL migration: name is invalid")
	case strings.TrimSpace(entry.SQL()) == "":
		return errors.New("apply PostgreSQL migration: SQL is empty")
	case len(entry.Checksum()) != 64:
		return errors.New("apply PostgreSQL migration: checksum is invalid")
	}
	for _, character := range []byte(entry.Checksum()) {
		if (character < '0' || character > '9') &&
			(character < 'a' || character > 'f') {
			return errors.New("apply PostgreSQL migration: checksum is invalid")
		}
	}
	return nil
}

func validPostgreSQLIdentifier(identifier string) bool {
	if identifier == "" || len(identifier) > maxPostgreSQLIdentifierBytes {
		return false
	}
	for index, character := range []byte(identifier) {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			character == '_' ||
			(index > 0 && character >= '0' && character <= '9') {
			continue
		}
		return false
	}
	return true
}

type migrationConnection interface {
	Exec(context.Context, string, ...any) error
	Query(context.Context, string, ...any) (migrationRows, error)
	QueryBool(context.Context, string, ...any) (bool, error)
	Begin(context.Context) (migrationTransaction, error)
	Close(context.Context) error
}

type migrationRows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close()
}

type migrationTransaction interface {
	ExecScript(context.Context, string) error
	Exec(context.Context, string, ...any) error
	Commit(context.Context) error
	Rollback(context.Context) error
}

type pgxConnection struct {
	connection *pgx.Conn
}

func (connection pgxConnection) Exec(
	ctx context.Context,
	statement string,
	arguments ...any,
) error {
	_, err := connection.connection.Exec(ctx, statement, arguments...)
	return err
}

func (connection pgxConnection) Query(
	ctx context.Context,
	statement string,
	arguments ...any,
) (migrationRows, error) {
	//nolint:sqlclosecheck // postgresMigrationSession.Applied owns and closes the returned rows.
	return connection.connection.Query(ctx, statement, arguments...)
}

func (connection pgxConnection) QueryBool(
	ctx context.Context,
	statement string,
	arguments ...any,
) (bool, error) {
	var value bool
	err := connection.connection.QueryRow(ctx, statement, arguments...).Scan(&value)
	return value, err
}

func (connection pgxConnection) Begin(
	ctx context.Context,
) (migrationTransaction, error) {
	transaction, err := connection.connection.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return pgxMigrationTransaction{transaction: transaction}, nil
}

func (connection pgxConnection) Close(ctx context.Context) error {
	return connection.connection.Close(ctx)
}

type pgxMigrationTransaction struct {
	transaction pgx.Tx
}

func (transaction pgxMigrationTransaction) ExecScript(
	ctx context.Context,
	statement string,
) error {
	_, err := transaction.transaction.Exec(
		ctx,
		statement,
		pgx.QueryExecModeSimpleProtocol,
	)
	return err
}

func (transaction pgxMigrationTransaction) Exec(
	ctx context.Context,
	statement string,
	arguments ...any,
) error {
	_, err := transaction.transaction.Exec(ctx, statement, arguments...)
	return err
}

func (transaction pgxMigrationTransaction) Commit(ctx context.Context) error {
	return transaction.transaction.Commit(ctx)
}

func (transaction pgxMigrationTransaction) Rollback(ctx context.Context) error {
	return transaction.transaction.Rollback(ctx)
}

func parseMigrationVersion(value string) (uint64, error) {
	version, err := strconv.ParseUint(value, 10, 64)
	if err != nil || strconv.FormatUint(version, 10) != value || version == 0 {
		return 0, errors.New("read PostgreSQL migration registry: version is invalid")
	}
	return version, nil
}
