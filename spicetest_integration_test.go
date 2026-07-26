//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/StevenBuglione/spice/data"
	"github.com/StevenBuglione/spice/spicetest"
	"github.com/StevenBuglione/spice/starter/postgres"
)

const integrationTestSliceTable = "spice_data_slice_integration"

func TestPostgreSQLDataTestSliceAlwaysRollsBack(t *testing.T) {
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

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := postgres.Ping(ctx, database); err != nil {
		t.Fatalf("ping PostgreSQL database: %v", err)
	}
	if _, err := database.ExecContext(
		ctx,
		`DROP TABLE IF EXISTS "`+integrationTestSliceTable+`"`,
	); err != nil {
		t.Fatalf("drop prior test-slice table: %v", err)
	}
	if _, err := database.ExecContext(
		ctx,
		`CREATE TABLE "`+integrationTestSliceTable+`" (id bigint PRIMARY KEY)`,
	); err != nil {
		t.Fatalf("create test-slice table: %v", err)
	}
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(
			context.Background(),
			10*time.Second,
		)
		defer cleanupCancel()
		if _, cleanupErr := database.ExecContext(
			cleanupContext,
			`DROP TABLE IF EXISTS "`+integrationTestSliceTable+`"`,
		); cleanupErr != nil {
			t.Errorf("drop test-slice table: %v", cleanupErr)
		}
	})

	slice, err := spicetest.NewSQL(
		ctx,
		database,
		func(ctx context.Context, executor data.Executor) (int, error) {
			if _, execErr := executor.ExecContext(
				ctx,
				`INSERT INTO "`+integrationTestSliceTable+`" (id) VALUES ($1)`,
				int64(41),
			); execErr != nil {
				return 0, execErr
			}
			var count int
			if queryErr := executor.QueryRowContext(
				ctx,
				`SELECT count(*) FROM "`+integrationTestSliceTable+`"`,
			).Scan(&count); queryErr != nil {
				return 0, queryErr
			}
			return count, nil
		},
		spicetest.SQLOptions{Isolation: sql.LevelSerializable},
	)
	if err != nil {
		t.Fatalf("construct PostgreSQL data test slice: %v", err)
	}
	if slice.Value() != 1 {
		t.Fatalf("transactional row count = %d, want 1", slice.Value())
	}
	if err := slice.Close(); err != nil {
		t.Fatalf("close PostgreSQL data test slice: %v", err)
	}

	var persisted int
	if err := database.QueryRowContext(
		ctx,
		`SELECT count(*) FROM "`+integrationTestSliceTable+`"`,
	).Scan(&persisted); err != nil {
		t.Fatalf("read persisted rows: %v", err)
	}
	if persisted != 0 {
		t.Fatalf("persisted rows = %d, want rollback", persisted)
	}
}
