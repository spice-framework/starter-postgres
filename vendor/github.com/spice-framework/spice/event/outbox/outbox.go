// Package outbox provides transport-neutral durable event publication
// contracts and an explicit at-least-once dispatcher.
package outbox

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"mime"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/spice-framework/spice/data"
)

const (
	maxPayloadBytes  = 1 << 20
	maxMetadataBytes = 512
	maxFailureDelay  = 24 * time.Hour
)

// ErrPublisherPanicked identifies an observed publisher panic. RunOnce reports
// it and then re-panics with the original value.
var ErrPublisherPanicked = errors.New("outbox publisher panicked")

// MessageSpec is the inspectable input to NewMessage.
type MessageSpec struct {
	ID          string
	Topic       string
	Module      string
	ContentType string
	Payload     []byte
	OccurredAt  time.Time
}

// Message is an immutable serialized event prepared for durable storage.
type Message struct {
	id          string
	topic       string
	module      string
	contentType string
	payload     []byte
	occurredAt  time.Time
}

// NewMessage validates and freezes one serialized event. IDs are caller-owned
// idempotency keys and must be unique within the application outbox.
func NewMessage(spec MessageSpec) (Message, error) {
	if err := validateMessageIdentity(spec.ID, spec.Topic, spec.Module); err != nil {
		return Message{}, err
	}
	mediaType, parameters, err := mime.ParseMediaType(spec.ContentType)
	typeName, subtype, separated := strings.Cut(mediaType, "/")
	if err != nil || !separated || typeName == "" || subtype == "" {
		return Message{}, errors.New("construct outbox message: content type is invalid")
	}
	contentType := mime.FormatMediaType(mediaType, parameters)
	if len(contentType) > maxMetadataBytes {
		return Message{}, errors.New("construct outbox message: content type is too long")
	}
	if len(spec.Payload) == 0 || len(spec.Payload) > maxPayloadBytes {
		return Message{}, fmt.Errorf(
			"construct outbox message: payload must be between 1 and %d bytes",
			maxPayloadBytes,
		)
	}
	if spec.OccurredAt.IsZero() {
		return Message{}, errors.New("construct outbox message: occurrence time is required")
	}
	return Message{
		id:          spec.ID,
		topic:       spec.Topic,
		module:      spec.Module,
		contentType: contentType,
		payload:     append([]byte(nil), spec.Payload...),
		occurredAt:  spec.OccurredAt.UTC(),
	}, nil
}

// ID returns the caller-owned idempotency key.
func (message Message) ID() string {
	return message.id
}

// Topic returns the stable event contract identity.
func (message Message) Topic() string {
	return message.topic
}

// Module returns the publishing module identity.
func (message Message) Module() string {
	return message.module
}

// ContentType returns the normalized payload media type.
func (message Message) ContentType() string {
	return message.contentType
}

// Payload returns a defensive copy of the serialized event.
func (message Message) Payload() []byte {
	return append([]byte(nil), message.payload...)
}

// OccurredAt returns the UTC event occurrence time.
func (message Message) OccurredAt() time.Time {
	return message.occurredAt
}

// ClaimRequest asks a store to atomically lease the oldest available messages.
type ClaimRequest struct {
	Owner string
	Now   time.Time
	Lease time.Duration
	Limit int
}

// Completion identifies one lease that was published successfully.
type Completion struct {
	Owner   string
	Receipt string
}

// Release makes one failed delivery available after an explicit delay.
type Release struct {
	Owner       string
	Receipt     string
	AvailableAt time.Time
}

// Store owns durable persistence and lease transitions. Enqueue must use the
// supplied executor so application state and its event can commit atomically.
type Store interface {
	Enqueue(context.Context, data.Executor, Message) error
	Claim(context.Context, ClaimRequest) ([]Delivery, error)
	Complete(context.Context, Completion) error
	Release(context.Context, Release) error
}

// Delivery is an immutable leased message returned by a Store.
type Delivery struct {
	message Message
	receipt string
	attempt int
}

// NewDelivery validates and freezes one store lease.
func NewDelivery(message Message, receipt string, attempt int) (Delivery, error) {
	if err := validateMessage(message); err != nil {
		return Delivery{}, err
	}
	if err := validateMetadata("receipt", receipt); err != nil {
		return Delivery{}, err
	}
	if attempt < 1 {
		return Delivery{}, errors.New("construct outbox delivery: attempt must be positive")
	}
	return Delivery{message: message, receipt: receipt, attempt: attempt}, nil
}

// Message returns the immutable leased event.
func (delivery Delivery) Message() Message {
	return delivery.message
}

// Receipt returns the store-owned opaque lease receipt.
func (delivery Delivery) Receipt() string {
	return delivery.receipt
}

// Attempt returns the one-based delivery attempt.
func (delivery Delivery) Attempt() int {
	return delivery.attempt
}

// Publisher sends one message to an external transport. Implementations must
// use Message.ID as the downstream idempotency key.
type Publisher interface {
	Publish(context.Context, Message) error
}

// FailureDelay computes when a failed lease becomes available again.
type FailureDelay func(Delivery) time.Duration

// Observation contains bounded metadata and no payload or lease receipt.
type Observation struct {
	Topic     string
	Module    string
	Attempt   int
	Duration  time.Duration
	Published bool
	Completed bool
	Released  bool
	Err       error
	Panicked  bool
}

// Observer receives completed delivery attempts synchronously.
type Observer func(context.Context, Observation)

// Options configures one instance-owned dispatcher.
type Options struct {
	Owner        string
	BatchSize    int
	Lease        time.Duration
	Clock        func() time.Time
	FailureDelay FailureDelay
}

// Dispatcher claims, publishes, and completes durable messages.
type Dispatcher struct {
	store     Store
	publisher Publisher
	options   Options
	observers []Observer
}

// Result summarizes one bounded dispatch pass.
type Result struct {
	Claimed   int
	Published int
	Completed int
	Released  int
}

// NewDispatcher validates and freezes one dispatch worker.
func NewDispatcher(
	store Store,
	publisher Publisher,
	options Options,
	observers ...Observer,
) (*Dispatcher, error) {
	if nilInterface(store) {
		return nil, errors.New("construct outbox dispatcher: store is nil")
	}
	if nilInterface(publisher) {
		return nil, errors.New("construct outbox dispatcher: publisher is nil")
	}
	if err := validateMetadata("owner", options.Owner); err != nil {
		return nil, err
	}
	if options.BatchSize < 1 || options.BatchSize > 1000 {
		return nil, errors.New("construct outbox dispatcher: batch size must be between 1 and 1000")
	}
	if options.Lease <= 0 || options.Lease > maxFailureDelay {
		return nil, errors.New("construct outbox dispatcher: lease must be between 1ns and 24h")
	}
	if options.Clock == nil {
		options.Clock = time.Now
	}
	for index, observer := range observers {
		if observer == nil {
			return nil, fmt.Errorf("construct outbox dispatcher: observer %d is nil", index)
		}
	}
	return &Dispatcher{
		store:     store,
		publisher: publisher,
		options:   options,
		observers: append([]Observer(nil), observers...),
	}, nil
}

// RunOnce performs one deterministic claim batch. Publishing is at least once:
// a successful publish followed by a completion failure can be delivered again.
func (dispatcher *Dispatcher) RunOnce(ctx context.Context) (Result, error) {
	var result Result
	if ctx == nil {
		return result, errors.New("dispatch outbox: context is nil")
	}
	if dispatcher == nil || nilInterface(dispatcher.store) || nilInterface(dispatcher.publisher) {
		return result, errors.New("dispatch outbox: dispatcher is nil")
	}
	if cause := context.Cause(ctx); cause != nil {
		return result, fmt.Errorf("dispatch outbox: %w", cause)
	}
	now := dispatcher.options.Clock().UTC()
	if now.IsZero() {
		return result, errors.New("dispatch outbox: clock returned zero time")
	}
	deliveries, err := dispatcher.store.Claim(ctx, ClaimRequest{
		Owner: dispatcher.options.Owner,
		Now:   now,
		Lease: dispatcher.options.Lease,
		Limit: dispatcher.options.BatchSize,
	})
	if err != nil {
		return result, fmt.Errorf("claim outbox messages: %w", err)
	}
	if err := validateDeliveries(deliveries, dispatcher.options.BatchSize); err != nil {
		return result, err
	}
	result.Claimed = len(deliveries)
	var resultErr error
	for _, delivery := range deliveries {
		if cause := context.Cause(ctx); cause != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("dispatch outbox: %w", cause))
			break
		}
		deliveryErr := dispatcher.deliver(ctx, delivery, &result)
		resultErr = errors.Join(resultErr, deliveryErr)
	}
	return result, resultErr
}

func (dispatcher *Dispatcher) deliver(
	ctx context.Context,
	delivery Delivery,
	result *Result,
) (resultErr error) {
	started := time.Now()
	observation := Observation{
		Topic:   delivery.message.topic,
		Module:  delivery.message.module,
		Attempt: delivery.attempt,
	}
	defer func() {
		observation.Duration = time.Since(started)
		dispatcher.observe(ctx, observation)
	}()
	panicked, publishErr := invokePublisher(ctx, dispatcher.publisher, delivery.message)
	if panicked != nil {
		observation.Err = ErrPublisherPanicked
		observation.Panicked = true
		panic(panicked)
	}
	if publishErr != nil {
		publishErr = fmt.Errorf("publish outbox message %q: %w", delivery.message.id, publishErr)
		availableAt, availabilityErr := dispatcher.failureAvailableAt(delivery)
		if availabilityErr != nil {
			resultErr = errors.Join(publishErr, availabilityErr)
			observation.Err = resultErr
			return resultErr
		}
		releaseErr := dispatcher.store.Release(ctx, Release{
			Owner:       dispatcher.options.Owner,
			Receipt:     delivery.receipt,
			AvailableAt: availableAt,
		})
		if releaseErr != nil {
			releaseErr = fmt.Errorf("release outbox message %q: %w", delivery.message.id, releaseErr)
		} else {
			result.Released++
			observation.Released = true
		}
		resultErr = errors.Join(publishErr, releaseErr)
		observation.Err = resultErr
		return resultErr
	}
	result.Published++
	observation.Published = true
	if err := dispatcher.store.Complete(ctx, Completion{
		Owner:   dispatcher.options.Owner,
		Receipt: delivery.receipt,
	}); err != nil {
		resultErr = fmt.Errorf("complete outbox message %q: %w", delivery.message.id, err)
		observation.Err = resultErr
		return resultErr
	}
	result.Completed++
	observation.Completed = true
	return nil
}

func invokePublisher(
	ctx context.Context,
	publisher Publisher,
	message Message,
) (panicked any, resultErr error) {
	defer func() {
		panicked = recover()
	}()
	return nil, publisher.Publish(ctx, message)
}

func (dispatcher *Dispatcher) failureAvailableAt(delivery Delivery) (time.Time, error) {
	now := dispatcher.options.Clock().UTC()
	if now.IsZero() {
		return time.Time{}, errors.New("release outbox message: clock returned zero time")
	}
	if dispatcher.options.FailureDelay == nil {
		return now, nil
	}
	delay := dispatcher.options.FailureDelay(delivery)
	if delay < 0 || delay > maxFailureDelay {
		return time.Time{}, fmt.Errorf(
			"release outbox message %q: failure delay must be between 0 and 24h",
			delivery.message.id,
		)
	}
	return now.Add(delay), nil
}

func (dispatcher *Dispatcher) observe(ctx context.Context, observation Observation) {
	for _, observer := range dispatcher.observers {
		observer(ctx, observation)
	}
}

func validateDeliveries(deliveries []Delivery, limit int) error {
	if len(deliveries) > limit {
		return fmt.Errorf("claim outbox messages: store returned %d messages above limit %d", len(deliveries), limit)
	}
	messageIDs := make(map[string]struct{}, len(deliveries))
	receipts := make(map[string]struct{}, len(deliveries))
	for index, delivery := range deliveries {
		if err := validateDelivery(delivery); err != nil {
			return fmt.Errorf("claim outbox messages: delivery %d is invalid: %w", index, err)
		}
		if _, duplicate := messageIDs[delivery.message.id]; duplicate {
			return fmt.Errorf("claim outbox messages: duplicate message ID %q", delivery.message.id)
		}
		if _, duplicate := receipts[delivery.receipt]; duplicate {
			return errors.New("claim outbox messages: duplicate lease receipt")
		}
		messageIDs[delivery.message.id] = struct{}{}
		receipts[delivery.receipt] = struct{}{}
	}
	if !slices.IsSortedFunc(deliveries, compareDeliveries) {
		return errors.New("claim outbox messages: store returned non-deterministic order")
	}
	return nil
}

func compareDeliveries(left, right Delivery) int {
	if left.message.occurredAt.Before(right.message.occurredAt) {
		return -1
	}
	if left.message.occurredAt.After(right.message.occurredAt) {
		return 1
	}
	return cmp.Compare(left.message.id, right.message.id)
}

func validateDelivery(delivery Delivery) error {
	if err := validateMessage(delivery.message); err != nil {
		return err
	}
	if err := validateMetadata("receipt", delivery.receipt); err != nil {
		return err
	}
	if delivery.attempt < 1 {
		return errors.New("attempt must be positive")
	}
	return nil
}

func validateMessage(message Message) error {
	if message.contentType == "" ||
		len(message.payload) == 0 ||
		len(message.payload) > maxPayloadBytes ||
		message.occurredAt.IsZero() {
		return errors.New("outbox message is invalid")
	}
	return validateMessageIdentity(message.id, message.topic, message.module)
}

func validateMessageIdentity(id, topic, module string) error {
	if err := validateMetadata("ID", id); err != nil {
		return err
	}
	if err := validateMetadata("topic", topic); err != nil {
		return err
	}
	return validateMetadata("module", module)
}

func validateMetadata(name, value string) error {
	if value == "" ||
		strings.TrimSpace(value) != value ||
		len(value) > maxMetadataBytes {
		return fmt.Errorf(
			"construct outbox: %s must be between 1 and %d bytes with no surrounding space",
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
