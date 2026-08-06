package verify

import (
	"context"

	"github.com/spice-framework/development/internal/catalog"
	"github.com/spice-framework/development/internal/librarypolicy"
	"github.com/spice-framework/development/internal/process"
)

func verifyStarterCompatibility(
	ctx context.Context,
	directory string,
	policy catalog.StarterCompatibilityPolicy,
	runner process.Runner,
) (string, bool, error) {
	_, output, executed, err := librarypolicy.Inspect(
		ctx,
		directory,
		policy,
		runner,
	)
	return output, executed, err
}
