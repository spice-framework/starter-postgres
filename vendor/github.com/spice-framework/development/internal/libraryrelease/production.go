package libraryrelease

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const maximumProductionStatusBytes = 1 << 20

func validateProductionState(ctx context.Context, repositoryRoot string, plan Plan) error {
	if plan.Mode != "production" {
		return errors.New("production release validation requires a production plan")
	}
	head, err := gitBytes(
		ctx,
		repositoryRoot,
		128,
		"rev-parse",
		"--verify",
		"HEAD^{commit}",
	)
	if err != nil {
		return fmt.Errorf("resolve production release HEAD: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(string(head)), plan.Commit) {
		return fmt.Errorf("production release HEAD does not match planned commit %s", plan.Commit)
	}
	status, err := gitBytes(
		ctx,
		repositoryRoot,
		maximumProductionStatusBytes,
		"status",
		"--porcelain=v1",
		"--untracked-files=all",
	)
	if err != nil {
		return fmt.Errorf("inspect production release checkout: %w", err)
	}
	if len(status) != 0 {
		return errors.New("production release checkout must be clean, including untracked files")
	}
	tag, err := gitBytes(
		ctx,
		repositoryRoot,
		128,
		"rev-parse",
		"--verify",
		"refs/tags/"+plan.Version+"^{commit}",
	)
	if err != nil {
		return fmt.Errorf("resolve exact production release tag %q: %w", plan.Version, err)
	}
	if !strings.EqualFold(strings.TrimSpace(string(tag)), plan.Commit) {
		return fmt.Errorf("production release tag %q does not match planned commit", plan.Version)
	}
	if err := validateCommitEpoch(ctx, repositoryRoot, plan); err != nil {
		return err
	}
	return nil
}

func requireOutsideRepository(repositoryRoot string, configuredPath string, label string) error {
	if strings.TrimSpace(configuredPath) == "" {
		return fmt.Errorf("%s path is required", label)
	}
	repository, err := filepath.Abs(repositoryRoot)
	if err != nil {
		return fmt.Errorf("resolve production repository boundary: %w", err)
	}
	absolute, err := filepath.Abs(configuredPath)
	if err != nil {
		return fmt.Errorf("resolve %s path: %w", label, err)
	}
	if pathWithin(repository, absolute) {
		return fmt.Errorf("%s path must be outside the production repository", label)
	}
	resolvedRepository, err := filepath.EvalSymlinks(repository)
	if err != nil {
		return fmt.Errorf("resolve production repository boundary: %w", err)
	}
	resolvedPath, err := resolveBoundaryPath(absolute)
	if err != nil {
		return fmt.Errorf("resolve %s boundary: %w", label, err)
	}
	if pathWithin(resolvedRepository, resolvedPath) {
		return fmt.Errorf("%s path must be outside the production repository", label)
	}
	return nil
}

func resolveBoundaryPath(path string) (string, error) {
	current := path
	var missing []string
	for {
		_, err := os.Lstat(current)
		if err == nil {
			resolved, resolveErr := filepath.EvalSymlinks(current)
			if resolveErr != nil {
				return "", resolveErr
			}
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func pathWithin(root string, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil &&
		(relative == "." || relative != ".." &&
			!strings.HasPrefix(relative, ".."+string(filepath.Separator)) &&
			!filepath.IsAbs(relative))
}
