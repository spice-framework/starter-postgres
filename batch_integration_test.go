//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spice-framework/spice/batch"
	"github.com/spice-framework/spice/data"
	postgres "github.com/spice-framework/starter-postgres"
)

const integrationBatchSchema = "spice_batch_integration"

func TestPostgreSQLBatchRestartLeaseAndConcurrency(t *testing.T) {
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
	if _, err := database.ExecContext(
		ctx,
		`DROP SCHEMA IF EXISTS "`+integrationBatchSchema+`" CASCADE`,
	); err != nil {
		t.Fatalf("drop prior batch schema: %v", err)
	}
	if _, err := database.ExecContext(
		ctx,
		`CREATE SCHEMA "`+integrationBatchSchema+`"`,
	); err != nil {
		t.Fatalf("create batch schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(
			context.Background(),
			10*time.Second,
		)
		defer cleanupCancel()
		if _, cleanupErr := database.ExecContext(
			cleanupContext,
			`DROP SCHEMA IF EXISTS "`+integrationBatchSchema+`" CASCADE`,
		); cleanupErr != nil {
			t.Errorf("drop batch schema: %v", cleanupErr)
		}
	})

	var clockNanos atomic.Int64
	started := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	clockNanos.Store(started.UnixNano())
	clock := func() time.Time {
		return time.Unix(0, clockNanos.Load()).UTC()
	}
	options := postgres.BatchOptions{
		Schema:       integrationBatchSchema,
		AttemptLease: time.Minute,
		Clock:        clock,
	}
	schemaSQL, err := postgres.BatchSchemaSQL(options)
	if err != nil {
		t.Fatalf("construct batch schema SQL: %v", err)
	}
	if _, err := database.ExecContext(ctx, schemaSQL); err != nil {
		t.Fatalf("create batch table: %v", err)
	}
	store, err := postgres.NewBatchStore(database, options)
	if err != nil {
		t.Fatalf("construct PostgreSQL batch store: %v", err)
	}

	testPostgreSQLBatchRestart(t, ctx, database, store, options)
	testPostgreSQLBatchConcurrency(t, ctx, store)
	testPostgreSQLBatchLease(t, ctx, database, store, options, &clockNanos)
}

func testPostgreSQLBatchRestart(
	t *testing.T,
	ctx context.Context,
	database data.Executor,
	store batch.Store,
	options postgres.BatchOptions,
) {
	t.Helper()
	var extractRuns atomic.Int32
	var loadRuns atomic.Int32
	job, err := batch.NewJob(
		batch.Definition{
			ID:     "orders.import",
			Module: "example.com/shop/orders",
		},
		[]batch.StepSpec{
			{ID: "extract", Run: func(context.Context) error {
				extractRuns.Add(1)
				return nil
			}},
			{ID: "load", Run: func(context.Context) error {
				if loadRuns.Add(1) == 1 {
					return errors.New("transient load failure")
				}
				return nil
			}},
		},
	)
	if err != nil {
		t.Fatalf("construct batch job: %v", err)
	}
	runner, err := batch.NewRunner(store, integrationFailureContext)
	if err != nil {
		t.Fatalf("construct batch runner: %v", err)
	}
	first, firstErr := runner.Run(ctx, job, "orders-2026-07-26")
	if firstErr == nil || first.Attempt != 1 || first.StepsCompleted != 1 {
		t.Fatalf("first run = %#v, error %v", first, firstErr)
	}
	second, secondErr := runner.Run(ctx, job, "orders-2026-07-26")
	if secondErr != nil ||
		second.Attempt != 2 ||
		second.StepsSkipped != 1 ||
		second.StepsCompleted != 1 {
		t.Fatalf("second run = %#v, error %v", second, secondErr)
	}
	if extractRuns.Load() != 1 || loadRuns.Load() != 2 {
		t.Fatalf(
			"step runs: extract=%d load=%d",
			extractRuns.Load(),
			loadRuns.Load(),
		)
	}

	reopened, err := postgres.NewBatchStore(database, options)
	if err != nil {
		t.Fatalf("reconstruct PostgreSQL batch store: %v", err)
	}
	reopenedRunner, err := batch.NewRunner(reopened, integrationFailureContext)
	if err != nil {
		t.Fatalf("construct reopened runner: %v", err)
	}
	third, thirdErr := reopenedRunner.Run(ctx, job, "orders-2026-07-26")
	if thirdErr != nil || !third.AlreadyComplete || third.Attempt != 2 {
		t.Fatalf("third run = %#v, error %v", third, thirdErr)
	}
}

func testPostgreSQLBatchConcurrency(
	t *testing.T,
	ctx context.Context,
	store batch.Store,
) {
	t.Helper()
	request := integrationBeginRequest("concurrent")
	start := make(chan struct{})
	outcomes := make(chan beginOutcome, 2)
	for range 2 {
		go func() {
			<-start
			attempt, err := store.Begin(ctx, request)
			outcomes <- beginOutcome{attempt: attempt, err: err}
		}()
	}
	close(start)
	var winner batch.Attempt
	var successes, running int
	for range 2 {
		outcome := <-outcomes
		switch {
		case outcome.err == nil:
			successes++
			winner = outcome.attempt
		case errors.Is(outcome.err, batch.ErrAlreadyRunning):
			running++
		default:
			t.Fatalf("concurrent Begin() error = %v", outcome.err)
		}
	}
	if successes != 1 || running != 1 {
		t.Fatalf("concurrent Begin(): successes=%d running=%d", successes, running)
	}
	if err := store.Fail(ctx, batch.Failure{
		Attempt: winner,
		Step:    "extract",
		Kind:    batch.FailureCanceled,
	}); err != nil {
		t.Fatalf("release concurrent attempt: %v", err)
	}

	changed := integrationBeginRequest("concurrent")
	changed.Steps = []string{"extract", "publish"}
	if _, err := store.Begin(
		ctx,
		changed,
	); !errors.Is(err, batch.ErrDefinitionChanged) {
		t.Fatalf("definition drift error = %v", err)
	}
}

func testPostgreSQLBatchLease(
	t *testing.T,
	ctx context.Context,
	database data.Executor,
	firstStore batch.Store,
	options postgres.BatchOptions,
	clockNanos *atomic.Int64,
) {
	t.Helper()
	request := integrationBeginRequest("lease")
	first, err := firstStore.Begin(ctx, request)
	if err != nil {
		t.Fatalf("begin leased attempt: %v", err)
	}
	clockNanos.Add(int64(2 * time.Minute))
	secondStore, err := postgres.NewBatchStore(database, options)
	if err != nil {
		t.Fatalf("construct lease takeover store: %v", err)
	}
	second, err := secondStore.Begin(ctx, request)
	if err != nil {
		t.Fatalf("take over expired lease: %v", err)
	}
	if second.Number() != 2 {
		t.Fatalf("takeover attempt = %d, want 2", second.Number())
	}
	if err := firstStore.Checkpoint(
		ctx,
		first,
		"extract",
	); !errors.Is(err, batch.ErrStaleAttempt) {
		t.Fatalf("old checkpoint error = %v, want ErrStaleAttempt", err)
	}
	if err := secondStore.Fail(ctx, batch.Failure{
		Attempt: second,
		Step:    "extract",
		Kind:    batch.FailureError,
	}); err != nil {
		t.Fatalf("release takeover attempt: %v", err)
	}
}

type beginOutcome struct {
	attempt batch.Attempt
	err     error
}

func integrationBeginRequest(instance string) batch.BeginRequest {
	return batch.BeginRequest{
		Definition: batch.Definition{
			ID:     "orders.import",
			Module: "example.com/shop/orders",
		},
		Instance: instance,
		Steps:    []string{"extract", "load"},
	}
}

func integrationFailureContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}
