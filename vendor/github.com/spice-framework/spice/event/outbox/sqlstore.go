package outbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/spice-framework/spice/data"
)

// SQLStatements supplies dialect-owned, fixed SQL for the outbox protocol.
// Statement text is trusted startup configuration, never request input.
type SQLStatements struct {
	Insert   string
	Claim    string
	Complete string
	Release  string
}

// SQLStore implements Store using standard database/sql contracts.
type SQLStore struct {
	executor   data.Executor
	statements SQLStatements
}

// NewSQLStore validates and freezes one driver-neutral SQL store. Construction
// performs no database operation.
func NewSQLStore(
	executor data.Executor,
	statements SQLStatements,
) (*SQLStore, error) {
	if executor == nil || nilInterface(executor) {
		return nil, errors.New("construct SQL outbox store: executor is nil")
	}
	for _, statement := range []struct {
		name  string
		value string
	}{
		{name: "insert", value: statements.Insert},
		{name: "claim", value: statements.Claim},
		{name: "complete", value: statements.Complete},
		{name: "release", value: statements.Release},
	} {
		if strings.TrimSpace(statement.value) == "" {
			return nil, fmt.Errorf("construct SQL outbox store: %s statement is empty", statement.name)
		}
	}
	return &SQLStore{executor: executor, statements: statements}, nil
}

// Enqueue inserts one message through the supplied application transaction.
// Arguments are ID, topic, module, content type, payload, and occurrence time.
func (store *SQLStore) Enqueue(
	ctx context.Context,
	executor data.Executor,
	message Message,
) error {
	if ctx == nil {
		return errors.New("enqueue SQL outbox message: context is nil")
	}
	if store == nil || strings.TrimSpace(store.statements.Insert) == "" {
		return errors.New("enqueue SQL outbox message: store is nil")
	}
	if executor == nil || nilInterface(executor) {
		return errors.New("enqueue SQL outbox message: executor is nil")
	}
	if err := validateMessage(message); err != nil {
		return fmt.Errorf("enqueue SQL outbox message: %w", err)
	}
	return execExactlyOne(
		ctx,
		executor,
		"enqueue SQL outbox message",
		store.statements.Insert,
		message.id,
		message.topic,
		message.module,
		message.contentType,
		append([]byte(nil), message.payload...),
		message.occurredAt,
	)
}

// Claim atomically leases messages through the configured statement. Arguments
// are owner, current time, lease expiry, and limit. Rows must return ID, topic,
// module, content type, payload, occurrence time, receipt, and attempt.
func (store *SQLStore) Claim(
	ctx context.Context,
	request ClaimRequest,
) (deliveries []Delivery, resultErr error) {
	if ctx == nil {
		return nil, errors.New("claim SQL outbox messages: context is nil")
	}
	if store == nil || nilInterface(store.executor) {
		return nil, errors.New("claim SQL outbox messages: store is nil")
	}
	if err := validateClaimRequest(request); err != nil {
		return nil, err
	}
	now := request.Now.UTC()
	rows, err := store.executor.QueryContext(
		ctx,
		store.statements.Claim,
		request.Owner,
		now,
		now.Add(request.Lease),
		request.Limit,
	)
	if err != nil {
		return nil, fmt.Errorf("claim SQL outbox messages: %w", err)
	}
	if rows == nil {
		return nil, errors.New("claim SQL outbox messages: query returned nil rows")
	}
	defer func() {
		resultErr = errors.Join(resultErr, closeRows(rows))
	}()
	for rows.Next() {
		delivery, scanErr := scanDelivery(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		deliveries = append(deliveries, delivery)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim SQL outbox messages: read rows: %w", err)
	}
	if err := validateDeliveries(deliveries, request.Limit); err != nil {
		return nil, err
	}
	return deliveries, nil
}

// Complete removes or marks one published lease using owner and receipt.
func (store *SQLStore) Complete(ctx context.Context, completion Completion) error {
	if ctx == nil {
		return errors.New("complete SQL outbox message: context is nil")
	}
	if store == nil || nilInterface(store.executor) {
		return errors.New("complete SQL outbox message: store is nil")
	}
	if err := validateOwnerReceipt("complete", completion.Owner, completion.Receipt); err != nil {
		return err
	}
	return execExactlyOne(
		ctx,
		store.executor,
		"complete SQL outbox message",
		store.statements.Complete,
		completion.Owner,
		completion.Receipt,
	)
}

// Release clears one failed lease and sets its next availability.
func (store *SQLStore) Release(ctx context.Context, release Release) error {
	if ctx == nil {
		return errors.New("release SQL outbox message: context is nil")
	}
	if store == nil || nilInterface(store.executor) {
		return errors.New("release SQL outbox message: store is nil")
	}
	if err := validateOwnerReceipt("release", release.Owner, release.Receipt); err != nil {
		return err
	}
	if release.AvailableAt.IsZero() {
		return errors.New("release SQL outbox message: availability time is required")
	}
	return execExactlyOne(
		ctx,
		store.executor,
		"release SQL outbox message",
		store.statements.Release,
		release.Owner,
		release.Receipt,
		release.AvailableAt.UTC(),
	)
}

func scanDelivery(rows *sql.Rows) (Delivery, error) {
	var spec MessageSpec
	var receipt string
	var attempt int
	if err := rows.Scan(
		&spec.ID,
		&spec.Topic,
		&spec.Module,
		&spec.ContentType,
		&spec.Payload,
		&spec.OccurredAt,
		&receipt,
		&attempt,
	); err != nil {
		return Delivery{}, fmt.Errorf("claim SQL outbox messages: scan row: %w", err)
	}
	message, err := NewMessage(spec)
	if err != nil {
		return Delivery{}, fmt.Errorf("claim SQL outbox messages: reconstruct message: %w", err)
	}
	delivery, err := NewDelivery(message, receipt, attempt)
	if err != nil {
		return Delivery{}, fmt.Errorf("claim SQL outbox messages: reconstruct delivery: %w", err)
	}
	return delivery, nil
}

func validateClaimRequest(request ClaimRequest) error {
	if err := validateMetadata("owner", request.Owner); err != nil {
		return fmt.Errorf("claim SQL outbox messages: %w", err)
	}
	if request.Now.IsZero() {
		return errors.New("claim SQL outbox messages: current time is required")
	}
	if request.Lease <= 0 || request.Lease > maxFailureDelay {
		return errors.New("claim SQL outbox messages: lease must be between 1ns and 24h")
	}
	if request.Limit < 1 || request.Limit > 1000 {
		return errors.New("claim SQL outbox messages: limit must be between 1 and 1000")
	}
	return nil
}

func validateOwnerReceipt(operation, owner, receipt string) error {
	if err := validateMetadata("owner", owner); err != nil {
		return fmt.Errorf("%s SQL outbox message: %w", operation, err)
	}
	if err := validateMetadata("receipt", receipt); err != nil {
		return fmt.Errorf("%s SQL outbox message: %w", operation, err)
	}
	return nil
}

func execExactlyOne(
	ctx context.Context,
	executor data.Executor,
	operation string,
	statement string,
	arguments ...any,
) error {
	if executor == nil || nilInterface(executor) {
		return fmt.Errorf("%s: executor is nil", operation)
	}
	result, err := executor.ExecContext(ctx, statement, arguments...)
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	if nilInterface(result) {
		return fmt.Errorf("%s: execution returned nil result", operation)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: read affected rows: %w", operation, err)
	}
	if affected != 1 {
		return fmt.Errorf("%s: affected %d rows, want exactly 1", operation, affected)
	}
	return nil
}

func closeRows(rows *sql.Rows) error {
	if err := rows.Close(); err != nil {
		return fmt.Errorf("claim SQL outbox messages: close rows: %w", err)
	}
	return nil
}

var _ Store = (*SQLStore)(nil)
