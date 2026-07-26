//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/StevenBuglione/spice/data"
	"github.com/StevenBuglione/spice/data/repository"
	"github.com/StevenBuglione/spice/starter/postgres"
)

func TestPostgreSQLTransactionAndRepositoryIntegration(t *testing.T) {
	connectionURL := os.Getenv("SPICE_POSTGRES_TEST_URL")
	if connectionURL == "" {
		t.Fatal("SPICE_POSTGRES_TEST_URL is required for integration tests")
	}
	database, err := postgres.Open(postgres.Options{
		URL:                connectionURL,
		MaxOpenConnections: 1,
		MaxIdleConnections: 1,
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
}
