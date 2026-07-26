//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/StevenBuglione/spice/event/outbox"
	"github.com/StevenBuglione/spice/starter/postgres"
)

const integrationOutboxSchema = "spice_outbox_integration"

func TestPostgreSQLOutboxTransactionsLeasesAndConcurrency(t *testing.T) {
	connectionURL := os.Getenv("SPICE_POSTGRES_TEST_URL")
	if connectionURL == "" {
		t.Fatal("SPICE_POSTGRES_TEST_URL is required for integration tests")
	}
	database, err := postgres.Open(postgres.Options{
		URL:                connectionURL,
		MaxOpenConnections: 8,
		MaxIdleConnections: 8,
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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := postgres.Ping(ctx, database); err != nil {
		t.Fatalf("ping PostgreSQL database: %v", err)
	}
	resetPostgreSQLOutboxSchema(t, ctx, database)

	options := postgres.OutboxOptions{Schema: integrationOutboxSchema}
	schemaSQL, err := postgres.OutboxSchemaSQL(options)
	if err != nil {
		t.Fatalf("construct outbox schema SQL: %v", err)
	}
	if _, err := database.ExecContext(ctx, schemaSQL); err != nil {
		t.Fatalf("create outbox table: %v", err)
	}
	store, err := postgres.NewOutboxStore(database, options)
	if err != nil {
		t.Fatalf("construct PostgreSQL outbox store: %v", err)
	}

	now := time.Date(2026, time.July, 26, 14, 0, 0, 0, time.UTC)
	testPostgreSQLOutboxTransaction(t, ctx, database, store, now)
	testPostgreSQLOutboxLeases(t, ctx, database, store, now)
	testPostgreSQLOutboxConcurrency(t, ctx, database, store, now)
}

func resetPostgreSQLOutboxSchema(
	t *testing.T,
	ctx context.Context,
	database *sql.DB,
) {
	t.Helper()
	if _, err := database.ExecContext(
		ctx,
		`DROP SCHEMA IF EXISTS "`+integrationOutboxSchema+`" CASCADE`,
	); err != nil {
		t.Fatalf("drop prior outbox schema: %v", err)
	}
	if _, err := database.ExecContext(
		ctx,
		`CREATE SCHEMA "`+integrationOutboxSchema+`"`,
	); err != nil {
		t.Fatalf("create outbox schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(
			context.Background(),
			10*time.Second,
		)
		defer cleanupCancel()
		if _, cleanupErr := database.ExecContext(
			cleanupContext,
			`DROP SCHEMA IF EXISTS "`+integrationOutboxSchema+`" CASCADE`,
		); cleanupErr != nil {
			t.Errorf("drop outbox schema: %v", cleanupErr)
		}
	})
}

func testPostgreSQLOutboxTransaction(
	t *testing.T,
	ctx context.Context,
	database *sql.DB,
	store *outbox.SQLStore,
	now time.Time,
) {
	t.Helper()
	transaction, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin rollback transaction: %v", err)
	}
	if err := store.Enqueue(
		ctx,
		transaction,
		integrationOutboxMessage(t, "rolled-back", now),
	); err != nil {
		t.Fatalf("enqueue rolled-back message: %v", err)
	}
	if err := transaction.Rollback(); err != nil {
		t.Fatalf("rollback outbox transaction: %v", err)
	}

	deliveries, err := store.Claim(ctx, outbox.ClaimRequest{
		Owner: "rollback-probe",
		Now:   now,
		Lease: time.Minute,
		Limit: 1,
	})
	if err != nil {
		t.Fatalf("claim rolled-back message: %v", err)
	}
	if len(deliveries) != 0 {
		t.Fatalf("claimed rolled-back messages = %d", len(deliveries))
	}
}

func testPostgreSQLOutboxLeases(
	t *testing.T,
	ctx context.Context,
	database *sql.DB,
	store *outbox.SQLStore,
	now time.Time,
) {
	t.Helper()
	transaction, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin enqueue transaction: %v", err)
	}
	for _, spec := range []struct {
		id         string
		occurredAt time.Time
	}{
		{id: "event-b", occurredAt: now},
		{id: "event-a", occurredAt: now},
		{id: "event-c", occurredAt: now.Add(time.Second)},
	} {
		if err := store.Enqueue(
			ctx,
			transaction,
			integrationOutboxMessage(t, spec.id, spec.occurredAt),
		); err != nil {
			_ = transaction.Rollback()
			t.Fatalf("enqueue %s: %v", spec.id, err)
		}
	}
	if err := transaction.Commit(); err != nil {
		t.Fatalf("commit outbox messages: %v", err)
	}

	first, err := store.Claim(ctx, outbox.ClaimRequest{
		Owner: "worker-one",
		Now:   now.Add(time.Second),
		Lease: time.Minute,
		Limit: 2,
	})
	if err != nil {
		t.Fatalf("claim first outbox batch: %v", err)
	}
	if ids := outboxDeliveryIDs(first); !slices.Equal(
		ids,
		[]string{"event-a", "event-b"},
	) {
		t.Fatalf("first claim IDs = %v", ids)
	}
	firstReceipt := first[0].Receipt()
	if first[0].Attempt() != 1 || first[1].Attempt() != 1 {
		t.Fatalf(
			"first attempts = %d, %d",
			first[0].Attempt(),
			first[1].Attempt(),
		)
	}
	if err := store.Release(ctx, outbox.Release{
		Owner:       "worker-one",
		Receipt:     firstReceipt,
		AvailableAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("release event-a: %v", err)
	}
	if err := store.Complete(ctx, outbox.Completion{
		Owner:   "worker-one",
		Receipt: first[1].Receipt(),
	}); err != nil {
		t.Fatalf("complete event-b: %v", err)
	}

	second, err := store.Claim(ctx, outbox.ClaimRequest{
		Owner: "worker-two",
		Now:   now.Add(2 * time.Second),
		Lease: time.Minute,
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("claim second outbox batch: %v", err)
	}
	if ids := outboxDeliveryIDs(second); !slices.Equal(ids, []string{"event-c"}) {
		t.Fatalf("second claim IDs = %v", ids)
	}
	if err := store.Complete(ctx, outbox.Completion{
		Owner:   "worker-two",
		Receipt: second[0].Receipt(),
	}); err != nil {
		t.Fatalf("complete event-c: %v", err)
	}

	retry, err := store.Claim(ctx, outbox.ClaimRequest{
		Owner: "worker-two",
		Now:   now.Add(time.Hour),
		Lease: time.Minute,
		Limit: 1,
	})
	if err != nil {
		t.Fatalf("claim released outbox message: %v", err)
	}
	if len(retry) != 1 ||
		retry[0].Message().ID() != "event-a" ||
		retry[0].Attempt() != 2 ||
		retry[0].Receipt() == firstReceipt {
		t.Fatalf("retried delivery = %#v", retry)
	}
	if err := store.Complete(ctx, outbox.Completion{
		Owner:   "worker-one",
		Receipt: firstReceipt,
	}); err == nil {
		t.Fatal("stale completion unexpectedly succeeded")
	}
	if err := store.Complete(ctx, outbox.Completion{
		Owner:   "worker-two",
		Receipt: retry[0].Receipt(),
	}); err != nil {
		t.Fatalf("complete retried event-a: %v", err)
	}
}

func testPostgreSQLOutboxConcurrency(
	t *testing.T,
	ctx context.Context,
	database *sql.DB,
	store *outbox.SQLStore,
	now time.Time,
) {
	t.Helper()
	message := integrationOutboxMessage(t, "event-concurrent", now)
	if err := store.Enqueue(ctx, database, message); err != nil {
		t.Fatalf("enqueue concurrent message: %v", err)
	}

	start := make(chan struct{})
	results := make(chan outboxClaimResult, 2)
	var wait sync.WaitGroup
	for index := range 2 {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			<-start
			deliveries, claimErr := store.Claim(ctx, outbox.ClaimRequest{
				Owner: "concurrent-worker-" + string(rune('a'+worker)),
				Now:   now.Add(2 * time.Hour),
				Lease: time.Minute,
				Limit: 1,
			})
			results <- outboxClaimResult{deliveries: deliveries, err: claimErr}
		}(index)
	}
	close(start)
	wait.Wait()
	close(results)

	var claimed []outbox.Delivery
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent claim: %v", result.err)
		}
		claimed = append(claimed, result.deliveries...)
	}
	if len(claimed) != 1 ||
		claimed[0].Message().ID() != message.ID() {
		t.Fatalf("concurrently claimed deliveries = %#v", claimed)
	}
}

func integrationOutboxMessage(
	t *testing.T,
	id string,
	occurredAt time.Time,
) outbox.Message {
	t.Helper()
	message, err := outbox.NewMessage(outbox.MessageSpec{
		ID:          id,
		Topic:       "orders.OrderPlaced",
		Module:      "example.com/shop/orders",
		ContentType: "application/json",
		Payload:     []byte(`{"order_id":"` + id + `"}`),
		OccurredAt:  occurredAt,
	})
	if err != nil {
		t.Fatalf("construct outbox message %s: %v", id, err)
	}
	return message
}

func outboxDeliveryIDs(deliveries []outbox.Delivery) []string {
	ids := make([]string, len(deliveries))
	for index, delivery := range deliveries {
		ids[index] = delivery.Message().ID()
	}
	return ids
}

type outboxClaimResult struct {
	deliveries []outbox.Delivery
	err        error
}
