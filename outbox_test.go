package postgres

import (
	"strings"
	"testing"
)

func TestOutboxSchemaAndStatementsAreDeterministicAndScoped(t *testing.T) {
	t.Parallel()

	options := OutboxOptions{Schema: "orders_events", Table: "publication"}
	first, err := OutboxSchemaSQL(options)
	if err != nil {
		t.Fatalf("OutboxSchemaSQL() error = %v", err)
	}
	second, err := OutboxSchemaSQL(options)
	if err != nil {
		t.Fatalf("second OutboxSchemaSQL() error = %v", err)
	}
	if first != second ||
		!strings.Contains(first, `"orders_events"."publication"`) ||
		!strings.Contains(first, "PRIMARY KEY") ||
		!strings.Contains(first, "attempt bigint") ||
		!strings.Contains(first, "octet_length(payload)") {
		t.Fatalf("outbox schema = %q", first)
	}

	statements := postgresOutboxStatements(`"orders_events"."publication"`)
	for name, statement := range map[string]string{
		"insert":   statements.Insert,
		"claim":    statements.Claim,
		"complete": statements.Complete,
		"release":  statements.Release,
	} {
		if !strings.Contains(statement, `"orders_events"."publication"`) ||
			strings.Contains(statement, "%!") {
			t.Fatalf("%s statement = %q", name, statement)
		}
	}
	if !strings.Contains(statements.Claim, "FOR UPDATE SKIP LOCKED") ||
		!strings.Contains(statements.Claim, "gen_random_uuid()") ||
		!strings.Contains(statements.Claim, "ORDER BY claimed.occurred_at, claimed.id") {
		t.Fatalf("outbox claim statement = %q", statements.Claim)
	}
}

func TestOutboxOptionsRejectUnsafeRelationsAndNilExecutor(t *testing.T) {
	t.Parallel()

	tests := []OutboxOptions{
		{Schema: "invalid-schema"},
		{Schema: "1invalid"},
		{Table: "invalid.table"},
		{Table: strings.Repeat("x", maxPostgreSQLIdentifierBytes+1)},
	}
	for index, options := range tests {
		if _, err := OutboxSchemaSQL(options); err == nil {
			t.Fatalf("OutboxSchemaSQL(invalid %d) unexpectedly succeeded", index)
		}
		if _, err := NewOutboxStore(nil, options); err == nil {
			t.Fatalf("NewOutboxStore(invalid %d) unexpectedly succeeded", index)
		}
	}
	if _, err := NewOutboxStore(nil, OutboxOptions{}); err == nil {
		t.Fatal("NewOutboxStore(nil executor) unexpectedly succeeded")
	}
}

func TestOutboxSchemaUsesDocumentedDefaults(t *testing.T) {
	t.Parallel()

	schema, err := OutboxSchemaSQL(OutboxOptions{})
	if err != nil {
		t.Fatalf("OutboxSchemaSQL() error = %v", err)
	}
	if !strings.Contains(schema, `"public"."spice_event_outbox"`) {
		t.Fatalf("default outbox schema = %q", schema)
	}
}
