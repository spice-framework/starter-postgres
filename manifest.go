package postgres

import spicestarter "github.com/spice-framework/spice/starter"

// Manifest returns PostgreSQL starter compatibility and review metadata.
func Manifest() spicestarter.Manifest {
	return spicestarter.Must(spicestarter.Spec{
		Schema:    spicestarter.Schema,
		ID:        "github.com/spice-framework/spice/starter/postgres",
		Version:   "0.1.0-dev",
		Module:    "github.com/spice-framework/spice",
		SpiceAPI:  spicestarter.APIVersion,
		MinimumGo: "1.26",
		License:   "Apache-2.0",
		Review:    "docs/dependency-reviews/pgx.md",
		Activation: spicestarter.Activation{
			Mode: spicestarter.ActivationExplicitConstructor,
			EntryPoints: []spicestarter.EntryPoint{
				{
					Package: "github.com/spice-framework/spice/starter/postgres",
					Symbol:  "Open",
				},
				{
					Package: "github.com/spice-framework/spice/starter/postgres",
					Symbol:  "NewBatchStore",
				},
				{
					Package: "github.com/spice-framework/spice/starter/postgres",
					Symbol:  "NewOutboxStore",
				},
			},
		},
		Capabilities: []string{
			"batch.postgresql",
			"data.postgresql",
			"data.sql",
			"event.outbox.postgresql",
			"migration.postgresql",
		},
		Dependencies: []spicestarter.Dependency{
			{
				Module:  "github.com/jackc/pgx/v5",
				Version: "v5.10.0",
				License: "MIT",
			},
		},
	})
}
