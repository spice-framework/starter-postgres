// Command starter-postgres-release builds deterministic signed source releases.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	starterrelease "github.com/spice-framework/starter-postgres/internal/release"
)

func main() {
	//nolint:forbidigo // This process entrypoint must propagate the command's explicit exit code.
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("starter-postgres-release", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var root, output, version, signingKey string
	var epoch int64
	var rehearsal bool
	flags.StringVar(&root, "root", ".", "repository root")
	flags.StringVar(&output, "output", "dist", "new release output directory")
	flags.StringVar(&version, "version", "", "canonical v-prefixed release version")
	flags.StringVar(&signingKey, "signing-key", "", "Ed25519 PKCS#8 PEM or base64 private-key file")
	flags.Int64Var(&epoch, "source-date-epoch", 0, "reproducible Unix timestamp (defaults to SOURCE_DATE_EPOCH or HEAD)")
	flags.BoolVar(&rehearsal, "rehearsal", false, "build an explicitly unsigned source-release rehearsal")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		return writeExit(stderr, 2, "starter-postgres-release: unexpected arguments: %s\n", strings.Join(flags.Args(), " "))
	}
	if version == "" {
		return writeExit(stderr, 2, "starter-postgres-release: -version is required\n")
	}
	if rehearsal && signingKey != "" {
		return writeExit(stderr, 2, "starter-postgres-release: -rehearsal is unsigned and cannot use -signing-key\n")
	}
	resolvedEpoch, err := sourceEpoch(ctx, root, epoch)
	if err != nil {
		return writeExit(stderr, 1, "starter-postgres-release: %v\n", err)
	}
	if !rehearsal {
		if validationErr := validateReleaseCheckout(ctx, root, version); validationErr != nil {
			return writeExit(stderr, 1, "starter-postgres-release: %v\n", validationErr)
		}
		headEpoch, epochErr := gitEpoch(ctx, root)
		if epochErr != nil {
			return writeExit(stderr, 1, "starter-postgres-release: read HEAD source epoch: %v\n", epochErr)
		}
		if !resolvedEpoch.Equal(headEpoch) {
			return writeExit(stderr, 1, "starter-postgres-release: source epoch %d differs from tagged commit epoch %d\n", resolvedEpoch.Unix(), headEpoch.Unix())
		}
	}
	var key []byte
	if signingKey != "" {
		key, err = readSigningKey(signingKey)
		if err != nil {
			return writeExit(stderr, 1, "starter-postgres-release: read signing key: %v\n", err)
		}
	}
	result, err := starterrelease.Build(ctx, starterrelease.Config{
		Root: root, OutputDir: output, Version: version, Epoch: resolvedEpoch,
		PrivateKey: key, AllowUnsigned: rehearsal,
	})
	if err != nil {
		return writeExit(stderr, 1, "starter-postgres-release: %v\n", err)
	}
	if _, err := fmt.Fprintf(stdout, "starter-postgres release %s created %d artifact(s) in %s.\n", version, len(result.Files), result.OutputDir); err != nil {
		return 1
	}
	return 0
}

func sourceEpoch(ctx context.Context, root string, explicit int64) (time.Time, error) {
	if explicit != 0 {
		return time.Unix(explicit, 0).UTC(), nil
	}
	if value := os.Getenv("SOURCE_DATE_EPOCH"); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("parse SOURCE_DATE_EPOCH: %w", err)
		}
		return time.Unix(parsed, 0).UTC(), nil
	}
	return gitEpoch(ctx, root)
}

func gitEpoch(ctx context.Context, root string) (time.Time, error) {
	output, err := gitOutput(ctx, root, "show", "-s", "--format=%ct", "HEAD")
	if err != nil {
		return time.Time{}, err
	}
	parsed, err := strconv.ParseInt(strings.TrimSpace(output), 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse HEAD source epoch: %w", err)
	}
	return time.Unix(parsed, 0).UTC(), nil
}

func validateReleaseCheckout(ctx context.Context, root, version string) error {
	status, err := gitOutput(ctx, root, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("inspect release checkout: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		return fmt.Errorf("release checkout is not clean")
	}
	tags, err := gitOutput(ctx, root, "tag", "--points-at", "HEAD")
	if err != nil {
		return fmt.Errorf("inspect release tag: %w", err)
	}
	if !slices.Contains(strings.Fields(tags), version) {
		return fmt.Errorf("release HEAD is not tagged exactly %q", version)
	}
	return nil
}

func gitOutput(ctx context.Context, root string, arguments ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", arguments...) // #nosec G204 -- fixed executable; callers provide repository-owned arguments.
	command.Dir = root
	command.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return "", errors.Join(err, fmt.Errorf("%s", strings.TrimSpace(stderr.String())))
	}
	return stdout.String(), nil
}

func readSigningKey(filename string) ([]byte, error) {
	const maximumKeyBytes = 1 << 20
	root, err := os.OpenRoot(filepath.Dir(filename))
	if err != nil {
		return nil, err
	}
	file, err := root.Open(filepath.Base(filename))
	if err != nil {
		return nil, errors.Join(err, root.Close())
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maximumKeyBytes+1))
	if err := errors.Join(readErr, file.Close(), root.Close()); err != nil {
		return nil, err
	}
	if len(data) > maximumKeyBytes {
		return nil, fmt.Errorf("signing key exceeds %d bytes", maximumKeyBytes)
	}
	return data, nil
}

func writeExit(writer io.Writer, code int, format string, arguments ...any) int {
	if _, err := fmt.Fprintf(writer, format, arguments...); err != nil {
		return 1
	}
	return code
}
