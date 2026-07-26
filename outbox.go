package postgres

import (
	"errors"
	"strings"

	"github.com/StevenBuglione/spice/data"
	"github.com/StevenBuglione/spice/event/outbox"
)

const defaultOutboxTable = "spice_event_outbox"

// OutboxOptions configures PostgreSQL-backed durable event publication.
// Schema and Table must already be covered by an application-owned migration.
// Empty values select public.spice_event_outbox.
type OutboxOptions struct {
	Schema string
	Table  string
}

// NewOutboxStore constructs a PostgreSQL outbox store without connecting or
// applying schema changes.
func NewOutboxStore(
	executor data.Executor,
	options OutboxOptions,
) (*outbox.SQLStore, error) {
	relation, err := outboxRelation(options)
	if err != nil {
		return nil, err
	}
	return outbox.NewSQLStore(executor, postgresOutboxStatements(relation))
}

// OutboxSchemaSQL returns deterministic initial DDL for an application-owned
// migration. It performs no database operation.
func OutboxSchemaSQL(options OutboxOptions) (string, error) {
	relation, err := outboxRelation(options)
	if err != nil {
		return "", err
	}
	return `CREATE TABLE IF NOT EXISTS ` + relation + ` (
	id text PRIMARY KEY CHECK (octet_length(id) BETWEEN 1 AND 512),
	topic text NOT NULL CHECK (octet_length(topic) BETWEEN 1 AND 512),
	module text NOT NULL CHECK (octet_length(module) BETWEEN 1 AND 512),
	content_type text NOT NULL CHECK (
		octet_length(content_type) BETWEEN 1 AND 512
	),
	payload bytea NOT NULL CHECK (octet_length(payload) BETWEEN 1 AND 1048576),
	occurred_at timestamp with time zone NOT NULL,
	available_at timestamp with time zone NOT NULL,
	owner text CHECK (
		owner IS NULL OR octet_length(owner) BETWEEN 1 AND 512
	),
	receipt text CHECK (
		receipt IS NULL OR octet_length(receipt) BETWEEN 1 AND 512
	),
	lease_until timestamp with time zone,
	attempt bigint NOT NULL DEFAULT 0 CHECK (attempt >= 0),
	CHECK ((owner IS NULL) = (receipt IS NULL)),
	CHECK ((owner IS NULL) = (lease_until IS NULL))
)`, nil
}

func outboxRelation(options OutboxOptions) (string, error) {
	schema := options.Schema
	if schema == "" {
		schema = defaultMigrationSchema
	}
	table := options.Table
	if table == "" {
		table = defaultOutboxTable
	}
	if !validPostgreSQLIdentifier(schema) {
		return "", errors.New("construct PostgreSQL outbox store: schema is invalid")
	}
	if !validPostgreSQLIdentifier(table) {
		return "", errors.New("construct PostgreSQL outbox store: table is invalid")
	}
	return `"` + schema + `"."` + table + `"`, nil
}

func postgresOutboxStatements(relation string) outbox.SQLStatements {
	insert := `INSERT INTO ` + relation + ` (
	id,
	topic,
	module,
	content_type,
	payload,
	occurred_at,
	available_at
) VALUES ($1, $2, $3, $4, $5, $6, $6)`
	claim := `WITH candidates AS (
	SELECT message.id
	FROM ` + relation + ` AS message
	WHERE message.available_at <= $2
		AND (
			message.lease_until IS NULL
			OR message.lease_until <= $2
		)
		AND message.attempt < 9223372036854775807
	ORDER BY message.occurred_at, message.id
	FOR UPDATE SKIP LOCKED
	LIMIT $4
),
claimed AS (
	UPDATE ` + relation + ` AS message
	SET owner = $1,
		receipt = gen_random_uuid()::text,
		lease_until = $3,
		attempt = message.attempt + 1
	FROM candidates
	WHERE message.id = candidates.id
	RETURNING
		message.id,
		message.topic,
		message.module,
		message.content_type,
		message.payload,
		message.occurred_at,
		message.receipt,
		message.attempt
)
SELECT
	claimed.id,
	claimed.topic,
	claimed.module,
	claimed.content_type,
	claimed.payload,
	claimed.occurred_at,
	claimed.receipt,
	claimed.attempt
FROM claimed
ORDER BY claimed.occurred_at, claimed.id`
	complete := `DELETE FROM ` + relation + `
WHERE owner = $1
	AND receipt = $2`
	release := `UPDATE ` + relation + `
SET owner = NULL,
	receipt = NULL,
	lease_until = NULL,
	available_at = $3
WHERE owner = $1
	AND receipt = $2`
	return outbox.SQLStatements{
		Insert:   strings.TrimSpace(insert),
		Claim:    strings.TrimSpace(claim),
		Complete: strings.TrimSpace(complete),
		Release:  strings.TrimSpace(release),
	}
}
