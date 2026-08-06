// Package cli exposes the cross-platform spice-dev command boundary.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"runtime"
	"strings"

	"github.com/spice-framework/development/internal/bootstrap"
	"github.com/spice-framework/development/internal/catalog"
	"github.com/spice-framework/development/internal/libraryrelease"
	"github.com/spice-framework/development/internal/process"
	"github.com/spice-framework/development/internal/verify"
	"github.com/spice-framework/development/internal/workspace"
)

var Version = "0.1.0-dev"

type Runtime struct {
	Catalog catalog.Catalog
	Runner  process.Runner
}

func Main(
	ctx context.Context,
	arguments []string,
	stdout io.Writer,
	stderr io.Writer,
) int {
	value, err := catalog.Default()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "spice-dev failed: %v\n", err)
		return 1
	}
	return Runtime{Catalog: value, Runner: process.ExecRunner{}}.Run(
		ctx,
		arguments,
		stdout,
		stderr,
	)
}

func (runtime Runtime) Run(
	ctx context.Context,
	arguments []string,
	stdout io.Writer,
	stderr io.Writer,
) int {
	if ctx == nil || stdout == nil || stderr == nil {
		return 1
	}
	if len(arguments) == 0 || arguments[0] == "help" || arguments[0] == "-h" ||
		arguments[0] == "--help" {
		if err := printHelp(stdout); err != nil {
			return 1
		}
		return 0
	}
	var code int
	switch arguments[0] {
	case "version":
		code = writeVersion(stdout)
	case "catalog":
		code = runtime.catalogCommand(arguments[1:], stdout, stderr)
	case "bootstrap":
		code = runtime.bootstrapCommand(ctx, arguments[1:], stdout, stderr)
	case "workspace":
		code = runtime.workspaceCommand(arguments[1:], stdout, stderr)
	case "verify":
		code = runtime.verifyCommand(ctx, arguments[1:], stdout, stderr)
	case "library-release":
		code = runtime.libraryReleaseCommand(ctx, arguments[1:], stdout, stderr)
	default:
		if _, err := fmt.Fprintf(stderr, "spice-dev: unknown command %q\n", arguments[0]); err != nil {
			return 1
		}
		code = 2
	}
	return code
}

func (runtime Runtime) libraryReleaseCommand(
	ctx context.Context,
	arguments []string,
	stdout io.Writer,
	stderr io.Writer,
) int {
	if len(arguments) == 0 {
		return usageError(stderr, "library-release requires the plan or render subcommand")
	}
	if arguments[0] == "render" {
		return runtime.libraryReleaseRenderCommand(ctx, arguments[1:], stdout, stderr)
	}
	if arguments[0] != "plan" {
		return usageError(stderr, "library-release requires the plan or render subcommand")
	}
	flags := flag.NewFlagSet("library-release plan", flag.ContinueOnError)
	flags.SetOutput(stderr)
	root := flags.String("root", "", "library repository root")
	repository := flags.String("repo", "", "catalog library repository")
	version := flags.String("version", "", "canonical v-prefixed release version")
	rehearsal := flags.Bool("rehearsal", false, "plan an unsigned untagged rehearsal")
	epoch := flags.Int64("source-date-epoch", 0, "require this exact source commit epoch")
	if err := flags.Parse(arguments[1:]); err != nil {
		return flagCode(err)
	}
	if flags.NArg() != 0 {
		return usageError(stderr, "library-release plan accepts no positional arguments")
	}
	plan, err := libraryrelease.CreatePlan(ctx, runtime.Catalog, libraryrelease.Options{
		Root: *root, Repository: *repository, Version: *version,
		Rehearsal: *rehearsal, SourceDateEpoch: *epoch,
	}, runtime.Runner)
	if err != nil {
		return commandError(stderr, "library-release plan", err)
	}
	content, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return commandError(stderr, "library-release plan", err)
	}
	if _, err := stdout.Write(append(content, '\n')); err != nil {
		return 1
	}
	return 0
}

func (runtime Runtime) libraryReleaseRenderCommand(
	ctx context.Context,
	arguments []string,
	stdout io.Writer,
	stderr io.Writer,
) int {
	flags := flag.NewFlagSet("library-release render", flag.ContinueOnError)
	flags.SetOutput(stderr)
	root := flags.String("root", "", "library repository root")
	planFile := flags.String("plan", "", "validated release plan JSON file")
	output := flags.String("output", "", "new release output directory")
	if err := flags.Parse(arguments); err != nil {
		return flagCode(err)
	}
	if flags.NArg() != 0 {
		return usageError(stderr, "library-release render accepts no positional arguments")
	}
	plan, err := libraryrelease.LoadPlan(*planFile)
	if err != nil {
		return commandError(stderr, "library-release render", err)
	}
	result, err := libraryrelease.Render(ctx, *root, *output, plan, runtime.Catalog)
	if err != nil {
		return commandError(stderr, "library-release render", err)
	}
	if _, err := fmt.Fprintf(
		stdout,
		"%s\t%s\t%d artifact(s)\n",
		result.OutputDir,
		plan.Commit,
		len(result.Files),
	); err != nil {
		return 1
	}
	return 0
}

func (runtime Runtime) catalogCommand(
	arguments []string,
	stdout io.Writer,
	stderr io.Writer,
) int {
	flags := flag.NewFlagSet("catalog", flag.ContinueOnError)
	flags.SetOutput(stderr)
	asJSON := flags.Bool("json", false, "print the exact compatibility catalog as JSON")
	if err := flags.Parse(arguments); err != nil {
		return flagCode(err)
	}
	if flags.NArg() != 0 {
		return usageError(stderr, "catalog accepts no positional arguments")
	}
	if *asJSON {
		content, err := json.MarshalIndent(runtime.Catalog, "", "  ")
		if err != nil {
			return commandError(stderr, "catalog", err)
		}
		content = append(content, '\n')
		if _, err := stdout.Write(content); err != nil {
			return 1
		}
		return 0
	}
	for _, repository := range runtime.Catalog.Repositories {
		if _, err := fmt.Fprintf(
			stdout,
			"%s\t%s\t%s\t%s\n",
			repository.Name,
			repository.Status,
			repository.Artifact,
			repository.CanonicalURL,
		); err != nil {
			return 1
		}
	}
	return 0
}

func (runtime Runtime) bootstrapCommand(
	ctx context.Context,
	arguments []string,
	stdout io.Writer,
	stderr io.Writer,
) int {
	flags := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	flags.SetOutput(stderr)
	root := flags.String("root", "", "workspace root")
	offline := flags.Bool("offline", false, "validate without cloning or fetching")
	if err := flags.Parse(arguments); err != nil {
		return flagCode(err)
	}
	if flags.NArg() != 0 {
		return usageError(stderr, "bootstrap accepts no positional arguments")
	}
	results, err := bootstrap.Ensure(ctx, *root, runtime.Catalog, *offline, runtime.Runner)
	if err != nil {
		return commandError(stderr, "bootstrap", err)
	}
	for _, result := range results {
		if _, err := fmt.Fprintf(
			stdout,
			"%s\t%s\t%s\n",
			result.Repository,
			result.Action,
			result.Directory,
		); err != nil {
			return 1
		}
	}
	return 0
}

func (runtime Runtime) workspaceCommand(
	arguments []string,
	stdout io.Writer,
	stderr io.Writer,
) int {
	flags := flag.NewFlagSet("workspace", flag.ContinueOnError)
	flags.SetOutput(stderr)
	root := flags.String("root", "", "workspace root")
	check := flags.Bool("check", false, "require current generated workspace files")
	if err := flags.Parse(arguments); err != nil {
		return flagCode(err)
	}
	if flags.NArg() != 0 {
		return usageError(stderr, "workspace accepts no positional arguments")
	}
	plan, err := workspace.Render(*root, runtime.Catalog)
	if err != nil {
		return commandError(stderr, "workspace", err)
	}
	if err := workspace.Apply(*root, plan, *check); err != nil {
		return commandError(stderr, "workspace", err)
	}
	action := "updated"
	if *check {
		action = "current"
	}
	if _, err := fmt.Fprintf(stdout, "workspace %s: go.work, spice.code-workspace\n", action); err != nil {
		return 1
	}
	return 0
}

func (runtime Runtime) verifyCommand(
	ctx context.Context,
	arguments []string,
	stdout io.Writer,
	stderr io.Writer,
) int {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	flags.SetOutput(stderr)
	root := flags.String("root", "", "workspace root")
	full := flags.Bool("full", false, "run complete repository gates")
	jobs := flags.Int("jobs", min(4, runtime2GOMAXPROCS()), "maximum concurrent repositories")
	var repositories stringList
	flags.Var(&repositories, "repo", "repository to verify; repeat to select multiple")
	if err := flags.Parse(arguments); err != nil {
		return flagCode(err)
	}
	if flags.NArg() != 0 {
		return usageError(stderr, "verify accepts no positional arguments")
	}
	mode := verify.Fast
	if *full {
		mode = verify.Full
	}
	results, err := verify.Run(ctx, runtime.Catalog, verify.Options{
		Root: *root, Mode: mode, Repositories: repositories, Jobs: *jobs,
	}, runtime.Runner)
	for _, result := range results {
		status := "passed"
		if result.Err != nil {
			status = "failed"
		}
		if _, writeErr := fmt.Fprintf(
			stdout,
			"%s\t%s\t%d command(s)\t%s\n",
			result.Repository,
			status,
			result.Commands,
			result.Duration.Round(10_000_000),
		); writeErr != nil {
			return 1
		}
		if result.Output != "" {
			if _, writeErr := fmt.Fprintln(stdout, result.Output); writeErr != nil {
				return 1
			}
		}
	}
	if err != nil {
		return commandError(stderr, "verify", err)
	}
	return 0
}

type stringList []string

func (values *stringList) String() string { return strings.Join(*values, ",") }

func (values *stringList) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("repository name must not be empty")
	}
	*values = append(*values, value)
	return nil
}

func runtime2GOMAXPROCS() int { return runtime.GOMAXPROCS(0) }

func writeVersion(writer io.Writer) int {
	if _, err := fmt.Fprintln(writer, Version); err != nil {
		return 1
	}
	return 0
}

func printHelp(writer io.Writer) error {
	_, err := io.WriteString(writer, `spice-dev manages the Spice multi-repository workspace.

Usage:
  spice-dev version
  spice-dev catalog [--json]
  spice-dev bootstrap --root path [--offline]
  spice-dev workspace --root path [--check]
  spice-dev verify --root path [--full] [--jobs n] [--repo name ...]
  spice-dev library-release plan --root path --repo name --version vX.Y.Z [--rehearsal]
  spice-dev library-release render --root path --plan plan.json --output new-path
`)
	return err
}

func usageError(writer io.Writer, message string) int {
	if _, err := fmt.Fprintf(writer, "spice-dev: %s\n", message); err != nil {
		return 1
	}
	return 2
}

func commandError(writer io.Writer, command string, err error) int {
	if _, writeErr := fmt.Fprintf(writer, "spice-dev %s failed: %v\n", command, err); writeErr != nil {
		return 1
	}
	return 1
}

func flagCode(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return 2
}
