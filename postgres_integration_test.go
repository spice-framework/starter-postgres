//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/StevenBuglione/spice/data"
	"github.com/StevenBuglione/spice/data/repository"
	"github.com/StevenBuglione/spice/migration"
	"github.com/StevenBuglione/spice/starter/postgres"
)

const (
	integrationMigrationSchema = "spice_migration_integration"
	integrationMigrationLockID = int64(0x53504943454d4954)
)

func TestPostgreSQLTransactionAndRepositoryIntegration(t *testing.T) {
	connectionURL := os.Getenv("SPICE_POSTGRES_TEST_URL")
	if connectionURL == "" {
		t.Fatal("SPICE_POSTGRES_TEST_URL is required for integration tests")
	}
	database, err := postgres.Open(postgres.Options{
		URL:                connectionURL,
		MaxOpenConnections: 4,
		MaxIdleConnections: 4,
		AllowInsecure:      true,
	})
	if err != nil {
		t.Fatalf("open PostgreSQL database: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := database.Close(); closeErr != nil {
			t.Errorf("close database: %v", closeErr)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := postgres.Ping(ctx, database); err != nil {
		t.Fatalf("ping PostgreSQL database: %v", err)
	}
	if _, err := database.ExecContext(
		ctx,
		`CREATE TEMPORARY TABLE spice_orders (id bigint PRIMARY KEY, quantity bigint NOT NULL)`,
	); err != nil {
		t.Fatalf("create test table: %v", err)
	}

	manager, err := data.NewManager(database)
	if err != nil {
		t.Fatalf("construct transaction manager: %v", err)
	}
	err = manager.Within(ctx, data.Definition{
		ID:        "orders.create",
		Module:    "example.com/shop/orders",
		Isolation: sql.LevelSerializable,
	}, func(ctx context.Context, executor data.Executor) error {
		_, execErr := executor.ExecContext(
			ctx,
			`INSERT INTO spice_orders (id, quantity) VALUES ($1, $2)`,
			int64(41),
			int64(3),
		)
		return execErr
	})
	if err != nil {
		t.Fatalf("insert order: %v", err)
	}

	findOrder, err := repository.NewQuery(repository.QuerySpec[int64]{
		ID:        "orders.findQuantity",
		Module:    "example.com/shop/orders",
		Statement: `SELECT quantity FROM spice_orders WHERE id = $1`,
		MaxRows:   1,
		Decode: func(scanner repository.Scanner) (int64, error) {
			var quantity int64
			scanErr := scanner.Scan(&quantity)
			return quantity, scanErr
		},
	})
	if err != nil {
		t.Fatalf("construct repository query: %v", err)
	}
	quantity, err := findOrder.One(ctx, database, int64(41))
	if err != nil {
		t.Fatalf("find order: %v", err)
	}
	if quantity != 3 {
		t.Fatalf("unexpected quantity: %d", quantity)
	}

	testPostgreSQLMigrations(t, ctx, database)
}

func testPostgreSQLMigrations(t *testing.T, ctx context.Context, database *sql.DB) {
	t.Helper()
	if _, err := database.ExecContext(
		ctx,
		`DROP SCHEMA IF EXISTS "`+integrationMigrationSchema+`" CASCADE`,
	); err != nil {
		t.Fatalf("drop prior migration schema: %v", err)
	}
	if _, err := database.ExecContext(
		ctx,
		`CREATE SCHEMA "`+integrationMigrationSchema+`"`,
	); err != nil {
		t.Fatalf("create migration schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, cleanupErr := database.ExecContext(
			cleanupContext,
			`DROP SCHEMA IF EXISTS "`+integrationMigrationSchema+`" CASCADE`,
		); cleanupErr != nil {
			t.Errorf("drop migration schema: %v", cleanupErr)
		}
	})

	backend, err := postgres.NewMigrationBackend(database, postgres.MigrationOptions{
		Schema: integrationMigrationSchema,
		LockID: integrationMigrationLockID,
	})
	if err != nil {
		t.Fatalf("construct migration backend: %v", err)
	}
	plan, err := migration.NewPlan([]migration.Spec{{
		Version: 202607260001,
		Module:  "example.com/shop/orders",
		Name:    "create integration orders",
		SQL: `CREATE TABLE "` + integrationMigrationSchema + `"."orders" (
	id bigint PRIMARY KEY
);
INSERT INTO "` + integrationMigrationSchema + `"."orders" (id) VALUES (41);`,
	}})
	if err != nil {
		t.Fatalf("construct migration plan: %v", err)
	}
	runner, err := migration.NewRunner(backend)
	if err != nil {
		t.Fatalf("construct migration runner: %v", err)
	}

	start := make(chan struct{})
	outcomes := make(chan migrationOutcome, 2)
	for range 2 {
		go func() {
			<-start
			result, runErr := runner.Run(ctx, plan)
			outcomes <- migrationOutcome{result: result, err: runErr}
		}()
	}
	close(start)
	var current, applied int
	for range 2 {
		outcome := <-outcomes
		if outcome.err != nil {
			t.Fatalf("run concurrent migrations: %v", outcome.err)
		}
		current += outcome.result.Current
		applied += outcome.result.Applied
	}
	if current != 1 || applied != 1 {
		t.Fatalf("concurrent results: current=%d applied=%d", current, applied)
	}

	var orderCount int
	if err := database.QueryRowContext(
		ctx,
		`SELECT count(*) FROM "`+integrationMigrationSchema+`"."orders"`,
	).Scan(&orderCount); err != nil {
		t.Fatalf("read migrated orders: %v", err)
	}
	if orderCount != 1 {
		t.Fatalf("migrated order count = %d", orderCount)
	}

	failingPlan, err := migration.NewPlan([]migration.Spec{
		{
			Version: 202607260001,
			Module:  "example.com/shop/orders",
			Name:    "create integration orders",
			SQL: `CREATE TABLE "` + integrationMigrationSchema + `"."orders" (
	id bigint PRIMARY KEY
);
INSERT INTO "` + integrationMigrationSchema + `"."orders" (id) VALUES (41);`,
		},
		{
			Version: 202607260002,
			Module:  "example.com/shop/inventory",
			Name:    "prove rollback",
			SQL: `CREATE TABLE "` + integrationMigrationSchema + `"."rollback_probe" (id bigint);
SELECT missing_column FROM "` + integrationMigrationSchema + `"."rollback_probe";`,
		},
	})
	if err != nil {
		t.Fatalf("construct failing migration plan: %v", err)
	}
	if _, err := runner.Run(ctx, failingPlan); err == nil {
		t.Fatal("failing migration unexpectedly succeeded")
	}
	var rollbackTable *string
	if err := database.QueryRowContext(
		ctx,
		`SELECT to_regclass($1)::text`,
		integrationMigrationSchema+".rollback_probe",
	).Scan(&rollbackTable); err != nil {
		t.Fatalf("inspect rollback table: %v", err)
	}
	if rollbackTable != nil {
		t.Fatalf("failed migration retained table %q", *rollbackTable)
	}
}

func TestPostgreSQLMigrationLockWaitHonorsCancellation(t *testing.T) {
	connectionURL := os.Getenv("SPICE_POSTGRES_TEST_URL")
	if connectionURL == "" {
		t.Fatal("SPICE_POSTGRES_TEST_URL is required for integration tests")
	}
	database, err := postgres.Open(postgres.Options{
		URL:                connectionURL,
		MaxOpenConnections: 2,
		MaxIdleConnections: 2,
		AllowInsecure:      true,
	})
	if err != nil {
		t.Fatalf("open PostgreSQL database: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := database.Close(); closeErr != nil {
			t.Errorf("close database: %v", closeErr)
		}
	})

	lockContext, cancelLock := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelLock()
	lockConnection, err := database.Conn(lockContext)
	if err != nil {
		t.Fatalf("acquire lock connection: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := lockConnection.Close(); closeErr != nil {
			t.Errorf("close lock connection: %v", closeErr)
		}
	})
	if _, err := lockConnection.ExecContext(
		lockContext,
		`SELECT pg_advisory_lock($1)`,
		integrationMigrationLockID,
	); err != nil {
		t.Fatalf("hold advisory lock: %v", err)
	}
	t.Cleanup(func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, unlockErr := lockConnection.ExecContext(
			cleanupContext,
			`SELECT pg_advisory_unlock($1)`,
			integrationMigrationLockID,
		); unlockErr != nil {
			t.Errorf("release advisory lock: %v", unlockErr)
		}
	})

	backend, err := postgres.NewMigrationBackend(database, postgres.MigrationOptions{
		LockID: integrationMigrationLockID,
	})
	if err != nil {
		t.Fatalf("construct migration backend: %v", err)
	}
	runner, err := migration.NewRunner(backend)
	if err != nil {
		t.Fatalf("construct migration runner: %v", err)
	}
	plan, err := migration.NewPlan(nil)
	if err != nil {
		t.Fatalf("construct empty plan: %v", err)
	}
	waitContext, cancelWait := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelWait()
	if _, err := runner.Run(waitContext, plan); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock wait error = %v", err)
	}
}

type migrationOutcome struct {
	result migration.Result
	err    error
}
