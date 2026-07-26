package postgres

import (
	"strings"
	"testing"
	"time"
)

func TestBatchSchemaAndStatementsAreDeterministicAndScoped(t *testing.T) {
	t.Parallel()

	options := BatchOptions{
		Schema:       "orders_batch",
		Table:        "executions",
		AttemptLease: time.Minute,
	}
	first, err := BatchSchemaSQL(options)
	if err != nil {
		t.Fatalf("BatchSchemaSQL() error = %v", err)
	}
	second, err := BatchSchemaSQL(options)
	if err != nil {
		t.Fatalf("second BatchSchemaSQL() error = %v", err)
	}
	if first != second ||
		!strings.Contains(first, `"orders_batch"."executions"`) ||
		!strings.Contains(first, "PRIMARY KEY (job_id, module, instance)") ||
		!strings.Contains(first, "jsonb_array_length(completed_steps)") {
		t.Fatalf("batch schema = %q", first)
	}
	statements := postgresBatchStatements(`"orders_batch"."executions"`)
	for name, statement := range map[string]string{
		"begin":      statements.Begin,
		"checkpoint": statements.Checkpoint,
		"complete":   statements.Complete,
		"fail":       statements.Fail,
	} {
		if !strings.Contains(statement, `"orders_batch"."executions"`) ||
			strings.Contains(statement, "%!") {
			t.Fatalf("%s statement = %q", name, statement)
		}
	}
	if !strings.Contains(statements.Begin, "ON CONFLICT") ||
		!strings.Contains(statements.Begin, "RETURNING begin_outcome") ||
		!strings.Contains(statements.Checkpoint, "jsonb_build_array") {
		t.Fatalf("batch statements = %#v", statements)
	}
}

func TestBatchOptionsRejectUnsafeRelationsAndStoreConfiguration(t *testing.T) {
	t.Parallel()

	tests := []BatchOptions{
		{Schema: "invalid-schema"},
		{Schema: "1invalid"},
		{Table: "invalid.table"},
		{Table: strings.Repeat("x", maxPostgreSQLIdentifierBytes+1)},
	}
	for index, options := range tests {
		if _, err := BatchSchemaSQL(options); err == nil {
			t.Fatalf("BatchSchemaSQL(invalid %d) unexpectedly succeeded", index)
		}
		if _, err := NewBatchStore(nil, options); err == nil {
			t.Fatalf("NewBatchStore(invalid %d) unexpectedly succeeded", index)
		}
	}
	if _, err := NewBatchStore(nil, BatchOptions{
		AttemptLease: time.Minute,
	}); err == nil {
		t.Fatal("NewBatchStore(nil executor) unexpectedly succeeded")
	}
}

func TestBatchSchemaUsesDocumentedDefaults(t *testing.T) {
	t.Parallel()

	schema, err := BatchSchemaSQL(BatchOptions{})
	if err != nil {
		t.Fatalf("BatchSchemaSQL() error = %v", err)
	}
	if !strings.Contains(
		schema,
		`"public"."spice_batch_execution"`,
	) {
		t.Fatalf("default batch schema = %q", schema)
	}
}
