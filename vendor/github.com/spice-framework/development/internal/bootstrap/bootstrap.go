// Package bootstrap safely establishes catalog-owned repository checkouts.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spice-framework/development/internal/catalog"
	"github.com/spice-framework/development/internal/process"
)

type Result struct {
	Repository string
	Directory  string
	Action     string
}

func Ensure(
	ctx context.Context,
	root string,
	value catalog.Catalog,
	offline bool,
	runner process.Runner,
) ([]Result, error) {
	if ctx == nil {
		return nil, errors.New("bootstrap context must not be nil")
	}
	if runner == nil {
		return nil, errors.New("bootstrap runner must not be nil")
	}
	if err := value.Validate(); err != nil {
		return nil, err
	}
	absolute, err := prepareRoot(root)
	if err != nil {
		return nil, err
	}
	results := make([]Result, 0, len(value.Active()))
	for _, repository := range value.Active() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result, ensureErr := ensureRepository(
			ctx,
			absolute,
			repository,
			offline,
			runner,
		)
		if ensureErr != nil {
			return nil, ensureErr
		}
		results = append(results, result)
	}
	return results, nil
}

func prepareRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("bootstrap root is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve bootstrap root: %w", err)
	}
	info, err := os.Lstat(absolute)
	if errors.Is(err, os.ErrNotExist) {
		if mkdirErr := os.Mkdir(absolute, 0o750); mkdirErr != nil {
			return "", fmt.Errorf("create bootstrap root: %w", mkdirErr)
		}
		return absolute, nil
	}
	if err != nil {
		return "", fmt.Errorf("inspect bootstrap root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("bootstrap root %q is not a real directory", absolute)
	}
	return absolute, nil
}

func ensureRepository(
	ctx context.Context,
	root string,
	repository catalog.Repository,
	offline bool,
	runner process.Runner,
) (Result, error) {
	target := filepath.Join(root, repository.Directory)
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		if offline {
			return Result{}, fmt.Errorf(
				"repository %s is missing at %s in offline mode",
				repository.Name,
				target,
			)
		}
		if _, cloneErr := runner.Run(
			ctx,
			root,
			"git",
			"clone",
			"--filter=blob:none",
			"--origin",
			"origin",
			repository.CloneURL,
			target,
		); cloneErr != nil {
			return Result{}, fmt.Errorf("clone repository %s: %w", repository.Name, cloneErr)
		}
		return Result{Repository: repository.Name, Directory: target, Action: "cloned"}, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("inspect repository %s: %w", repository.Name, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return Result{}, fmt.Errorf("repository path %s is not a real directory", target)
	}
	if _, statErr := os.Stat(filepath.Join(target, ".git")); statErr != nil {
		return Result{}, fmt.Errorf("repository %s is not a Git checkout: %w", repository.Name, statErr)
	}
	remote, remoteErr := runner.Run(
		ctx,
		target,
		"git",
		"remote",
		"get-url",
		"origin",
	)
	if remoteErr != nil {
		return Result{}, fmt.Errorf("inspect repository %s origin: %w", repository.Name, remoteErr)
	}
	if strings.TrimSpace(remote) != repository.CloneURL {
		return Result{}, fmt.Errorf(
			"repository %s origin is %q, expected %q; refusing to rewrite it",
			repository.Name,
			strings.TrimSpace(remote),
			repository.CloneURL,
		)
	}
	if offline {
		return Result{Repository: repository.Name, Directory: target, Action: "checked"}, nil
	}
	if _, fetchErr := runner.Run(
		ctx,
		target,
		"git",
		"fetch",
		"--prune",
		"origin",
	); fetchErr != nil {
		return Result{}, fmt.Errorf("fetch repository %s: %w", repository.Name, fetchErr)
	}
	return Result{Repository: repository.Name, Directory: target, Action: "fetched"}, nil
}
