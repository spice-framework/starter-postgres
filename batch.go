package postgres

import (
	"errors"
	"strings"
	"time"

	spicebatch "github.com/StevenBuglione/spice/batch"
	"github.com/StevenBuglione/spice/data"
)

const defaultBatchTable = "spice_batch_execution"

// BatchOptions configures PostgreSQL-backed batch attempt persistence. Schema
// and Table must already be covered by an application-owned migration. Empty
// values select public.spice_batch_execution.
type BatchOptions struct {
	Schema       string
	Table        string
	AttemptLease time.Duration
	Clock        func() time.Time
}

// NewBatchStore constructs a lease-aware PostgreSQL batch store without
// connecting or applying schema changes.
func NewBatchStore(
	executor data.Executor,
	options BatchOptions,
) (*spicebatch.SQLStore, error) {
	relation, err := batchRelation(options)
	if err != nil {
		return nil, err
	}
	return spicebatch.NewSQLStore(
		executor,
		postgresBatchStatements(relation),
		spicebatch.SQLStoreOptions{
			AttemptLease: options.AttemptLease,
			Clock:        options.Clock,
		},
	)
}

// BatchSchemaSQL returns the deterministic initial table DDL for an
// application-owned migration. It performs no database operation.
func BatchSchemaSQL(options BatchOptions) (string, error) {
	relation, err := batchRelation(options)
	if err != nil {
		return "", err
	}
	return `CREATE TABLE IF NOT EXISTS ` + relation + ` (
	job_id text NOT NULL CHECK (octet_length(job_id) BETWEEN 1 AND 512),
	module text NOT NULL CHECK (octet_length(module) BETWEEN 1 AND 512),
	instance text NOT NULL CHECK (octet_length(instance) BETWEEN 1 AND 512),
	steps jsonb NOT NULL CHECK (
		jsonb_typeof(steps) = 'array'
		AND jsonb_array_length(steps) BETWEEN 1 AND 10000
	),
	attempt bigint NOT NULL CHECK (attempt >= 1),
	completed_steps jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (
		jsonb_typeof(completed_steps) = 'array'
		AND jsonb_array_length(completed_steps) <= jsonb_array_length(steps)
	),
	state text NOT NULL CHECK (state IN ('running', 'failed', 'complete')),
	lease_until timestamp with time zone,
	failure_step text CHECK (
		failure_step IS NULL
		OR octet_length(failure_step) BETWEEN 1 AND 512
	),
	failure_kind text CHECK (
		failure_kind IS NULL
		OR failure_kind IN ('error', 'canceled', 'panic')
	),
	begin_outcome text NOT NULL CHECK (
		begin_outcome IN (
			'started',
			'resumed',
			'complete',
			'running',
			'changed',
			'overflow'
		)
	),
	updated_at timestamp with time zone NOT NULL,
	PRIMARY KEY (job_id, module, instance),
	CHECK ((state = 'running') = (lease_until IS NOT NULL)),
	CHECK (
		(state = 'failed')
		= (failure_step IS NOT NULL AND failure_kind IS NOT NULL)
	),
	CHECK (
		state <> 'complete'
		OR jsonb_array_length(completed_steps) = jsonb_array_length(steps)
	)
)`, nil
}

func batchRelation(options BatchOptions) (string, error) {
	schema := options.Schema
	if schema == "" {
		schema = defaultMigrationSchema
	}
	table := options.Table
	if table == "" {
		table = defaultBatchTable
	}
	if !validPostgreSQLIdentifier(schema) {
		return "", errors.New("construct PostgreSQL batch store: schema is invalid")
	}
	if !validPostgreSQLIdentifier(table) {
		return "", errors.New("construct PostgreSQL batch store: table is invalid")
	}
	return `"` + schema + `"."` + table + `"`, nil
}

func postgresBatchStatements(relation string) spicebatch.SQLStatements {
	resumable := `execution.steps = EXCLUDED.steps
		AND execution.state <> 'complete'
		AND NOT (
			execution.state = 'running'
			AND execution.lease_until > EXCLUDED.updated_at
		)
		AND execution.attempt < 9223372036854775807`
	begin := `INSERT INTO ` + relation + ` AS execution (
	job_id,
	module,
	instance,
	steps,
	attempt,
	completed_steps,
	state,
	lease_until,
	begin_outcome,
	updated_at
) VALUES (
	$1,
	$2,
	$3,
	$4::jsonb,
	1,
	'[]'::jsonb,
	'running',
	$6,
	'started',
	$5
)
ON CONFLICT (job_id, module, instance) DO UPDATE SET
	begin_outcome = CASE
		WHEN execution.steps <> EXCLUDED.steps THEN 'changed'
		WHEN execution.state = 'complete' THEN 'complete'
		WHEN execution.state = 'running'
			AND execution.lease_until > EXCLUDED.updated_at THEN 'running'
		WHEN execution.attempt = 9223372036854775807 THEN 'overflow'
		ELSE 'resumed'
	END,
	attempt = CASE WHEN ` + resumable + `
		THEN execution.attempt + 1
		ELSE execution.attempt
	END,
	state = CASE WHEN ` + resumable + `
		THEN 'running'
		ELSE execution.state
	END,
	lease_until = CASE WHEN ` + resumable + `
		THEN EXCLUDED.lease_until
		ELSE execution.lease_until
	END,
	failure_step = CASE WHEN ` + resumable + `
		THEN NULL
		ELSE execution.failure_step
	END,
	failure_kind = CASE WHEN ` + resumable + `
		THEN NULL
		ELSE execution.failure_kind
	END,
	updated_at = CASE WHEN ` + resumable + `
		THEN EXCLUDED.updated_at
		ELSE execution.updated_at
	END
RETURNING begin_outcome, attempt, completed_steps`
	checkpoint := `UPDATE ` + relation + ` AS execution
SET completed_steps = execution.completed_steps || jsonb_build_array($5::text),
	lease_until = $7,
	updated_at = $6
WHERE execution.job_id = $1
	AND execution.module = $2
	AND execution.instance = $3
	AND execution.attempt = $4
	AND execution.state = 'running'
	AND jsonb_array_length(execution.completed_steps)
		< jsonb_array_length(execution.steps)
	AND execution.steps ->> jsonb_array_length(execution.completed_steps) = $5`
	complete := `UPDATE ` + relation + ` AS execution
SET state = 'complete',
	lease_until = NULL,
	failure_step = NULL,
	failure_kind = NULL,
	updated_at = $5
WHERE execution.job_id = $1
	AND execution.module = $2
	AND execution.instance = $3
	AND execution.attempt = $4
	AND execution.state = 'running'
	AND jsonb_array_length(execution.completed_steps)
		= jsonb_array_length(execution.steps)`
	fail := `UPDATE ` + relation + ` AS execution
SET state = 'failed',
	lease_until = NULL,
	failure_step = $5,
	failure_kind = $6,
	updated_at = $7
WHERE execution.job_id = $1
	AND execution.module = $2
	AND execution.instance = $3
	AND execution.attempt = $4
	AND execution.state = 'running'
	AND (
		(
			jsonb_array_length(execution.completed_steps)
				< jsonb_array_length(execution.steps)
			AND execution.steps ->> jsonb_array_length(execution.completed_steps)
				= $5
		)
		OR (
			jsonb_array_length(execution.completed_steps)
				= jsonb_array_length(execution.steps)
			AND execution.steps ->> (jsonb_array_length(execution.steps) - 1)
				= $5
		)
	)`
	return spicebatch.SQLStatements{
		Begin:      strings.TrimSpace(begin),
		Checkpoint: strings.TrimSpace(checkpoint),
		Complete:   strings.TrimSpace(complete),
		Fail:       strings.TrimSpace(fail),
	}
}
