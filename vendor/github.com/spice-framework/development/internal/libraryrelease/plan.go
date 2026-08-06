// Package libraryrelease defines the central trusted release contract for
// independently versioned Spice libraries.
package libraryrelease

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/spice-framework/development/internal/catalog"
	"github.com/spice-framework/development/internal/librarypolicy"
	"github.com/spice-framework/development/internal/process"
)

const PlanSchema = 1

var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

var requiredFiles = []string{
	"LICENSE",
	"README.md",
	"go.mod",
	"go.sum",
	"vendor/modules.txt",
}

// Options selects one read-only release identity inspection.
type Options struct {
	Root            string
	Repository      string
	Version         string
	Rehearsal       bool
	SourceDateEpoch int64
}

// Plan is a deterministic, path-independent release description. A later
// rendering phase must consume this exact identity rather than rediscovering
// mutable repository state.
type Plan struct {
	Schema               int      `json:"schema"`
	Repository           string   `json:"repository"`
	Module               string   `json:"module"`
	Source               string   `json:"source"`
	Mode                 string   `json:"mode"`
	Version              string   `json:"version"`
	Commit               string   `json:"commit"`
	SourceDateEpoch      int64    `json:"source_date_epoch"`
	CompatibilityMinimum string   `json:"compatibility_minimum"`
	CompatibilityCurrent string   `json:"compatibility_current"`
	RequiredFiles        []string `json:"required_files"`
	Artifacts            []string `json:"artifacts"`
}

// CreatePlan validates release policy and exact source identity without
// changing the repository, downloading modules, creating artifacts, or signing.
func CreatePlan(
	ctx context.Context,
	value catalog.Catalog,
	options Options,
	runner process.Runner,
) (Plan, error) {
	if ctx == nil {
		return Plan{}, errors.New("library release context must not be nil")
	}
	if runner == nil {
		return Plan{}, errors.New("library release runner must not be nil")
	}
	if err := value.Validate(); err != nil {
		return Plan{}, err
	}
	if !catalog.ValidModuleVersion(options.Version) {
		return Plan{}, fmt.Errorf("release version %q is not canonical", options.Version)
	}
	repository, err := selectLibrary(value, options.Repository)
	if err != nil {
		return Plan{}, err
	}
	directory, err := libraryDirectory(options.Root)
	if err != nil {
		return Plan{}, err
	}
	remote, err := runner.Run(ctx, directory, "git", "remote", "get-url", "origin")
	if err != nil {
		return Plan{}, fmt.Errorf("resolve release origin: %w", err)
	}
	match, matchErr := sameGitRemote(remote, repository.CloneURL)
	if matchErr != nil {
		return Plan{}, fmt.Errorf("validate release origin: %w", matchErr)
	}
	if !match {
		return Plan{}, fmt.Errorf(
			"release origin %q does not match catalog clone URL %q",
			strings.TrimSpace(remote),
			repository.CloneURL,
		)
	}
	inspection, _, _, err := librarypolicy.Inspect(
		ctx,
		directory,
		value.StarterCompatibility,
		runner,
	)
	if err != nil {
		return Plan{}, fmt.Errorf("inspect library policy: %w", err)
	}
	if inspection.Module != repository.Module {
		return Plan{}, fmt.Errorf(
			"go.mod module %q does not match catalog module %q",
			inspection.Module,
			repository.Module,
		)
	}
	commit, epoch, err := sourceIdentity(ctx, directory, options.SourceDateEpoch, runner)
	if err != nil {
		return Plan{}, err
	}
	files := slices.Clone(requiredFiles)
	files = append(files, value.StarterCompatibility.MetadataFile)
	slices.Sort(files)
	for _, name := range files {
		if _, runErr := runner.Run(ctx, directory, "git", "cat-file", "-e", commit+":"+name); runErr != nil {
			return Plan{}, fmt.Errorf("required committed release file %q: %w", name, runErr)
		}
	}
	if err := validatePolicyFilesMatchCommit(
		ctx,
		directory,
		commit,
		value.StarterCompatibility.MetadataFile,
		runner,
	); err != nil {
		return Plan{}, err
	}
	mode := "production"
	if options.Rehearsal {
		mode = "rehearsal"
	} else if err := validateProduction(ctx, directory, options.Version, commit, runner); err != nil {
		return Plan{}, err
	}
	artifacts := artifactNames(repository.Name, options.Version, !options.Rehearsal)
	return Plan{
		Schema:               PlanSchema,
		Repository:           repository.Name,
		Module:               repository.Module,
		Source:               repository.CanonicalURL,
		Mode:                 mode,
		Version:              options.Version,
		Commit:               commit,
		SourceDateEpoch:      epoch,
		CompatibilityMinimum: inspection.Compatibility.Minimum,
		CompatibilityCurrent: inspection.Compatibility.Current,
		RequiredFiles:        files,
		Artifacts:            artifacts,
	}, nil
}

func sameGitRemote(actual string, expected string) (bool, error) {
	actualIdentity, err := gitRemoteIdentity(actual)
	if err != nil {
		return false, fmt.Errorf("parse origin: %w", err)
	}
	expectedIdentity, err := gitRemoteIdentity(expected)
	if err != nil {
		return false, fmt.Errorf("parse catalog clone URL %q: %w", expected, err)
	}
	return actualIdentity == expectedIdentity, nil
}

func gitRemoteIdentity(value string) (string, error) {
	value = strings.TrimSpace(value)
	if before, after, found := strings.Cut(value, ":"); found &&
		!strings.Contains(before, "/") && strings.Contains(before, "@") {
		value = "ssh://" + before + "/" + after
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", err
	}
	if !slices.Contains([]string{"https", "ssh"}, parsed.Scheme) ||
		parsed.Hostname() == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("require an HTTPS or SSH repository URL without query or fragment")
	}
	if parsed.Scheme == "https" && parsed.User != nil {
		return "", errors.New("HTTPS repository URL must not contain credentials")
	}
	if parsed.Scheme == "ssh" && (parsed.User == nil || parsed.User.Username() != "git") {
		return "", errors.New("SSH repository URL must use the git user")
	}
	repositoryPath := strings.TrimSuffix(strings.Trim(parsed.Path, "/"), ".git")
	if repositoryPath == "" || path.Clean(repositoryPath) != repositoryPath ||
		repositoryPath == ".." || strings.HasPrefix(repositoryPath, "../") {
		return "", errors.New("repository path is empty or unsafe")
	}
	host := strings.ToLower(parsed.Hostname())
	if parsed.Port() != "" {
		host += ":" + parsed.Port()
	}
	return host + "/" + repositoryPath, nil
}

func validatePolicyFilesMatchCommit(
	ctx context.Context,
	directory string,
	commit string,
	metadataFile string,
	runner process.Runner,
) error {
	output, err := runner.Run(
		ctx,
		directory,
		"git",
		"diff",
		"--no-ext-diff",
		"--unified=0",
		commit,
		"--",
		"go.mod",
		metadataFile,
	)
	if err != nil {
		return fmt.Errorf("compare release policy files with commit: %w", err)
	}
	if strings.TrimSpace(output) != "" {
		return errors.New("go.mod and compatibility metadata must match the release commit")
	}
	return nil
}

func selectLibrary(value catalog.Catalog, name string) (catalog.Repository, error) {
	if strings.TrimSpace(name) == "" {
		return catalog.Repository{}, errors.New("library repository is required")
	}
	for _, repository := range value.Repositories {
		if repository.Name != name {
			continue
		}
		if !value.StarterCompatibility.Applies(repository) ||
			repository.Artifact != "go-module" || repository.Module == "" {
			return catalog.Repository{}, fmt.Errorf(
				"repository %q is not a governed active Go library",
				name,
			)
		}
		return repository, nil
	}
	return catalog.Repository{}, fmt.Errorf("library repository %q is not in the catalog", name)
}

func libraryDirectory(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("library release repository root is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve library release repository root: %w", err)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect library release path %q: %w", absolute, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("library release path %q is not a real directory", absolute)
	}
	return absolute, nil
}

func sourceIdentity(
	ctx context.Context,
	directory string,
	explicitEpoch int64,
	runner process.Runner,
) (string, int64, error) {
	commit, err := runner.Run(ctx, directory, "git", "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", 0, fmt.Errorf("resolve release HEAD: %w", err)
	}
	commit = strings.TrimSpace(commit)
	if !commitPattern.MatchString(commit) {
		return "", 0, fmt.Errorf("release HEAD %q is not a full Git object ID", commit)
	}
	epochText, err := runner.Run(ctx, directory, "git", "show", "-s", "--format=%ct", commit)
	if err != nil {
		return "", 0, fmt.Errorf("read release source epoch: %w", err)
	}
	epoch, err := strconv.ParseInt(strings.TrimSpace(epochText), 10, 64)
	if err != nil || epoch <= 0 {
		return "", 0, fmt.Errorf("release source epoch %q is invalid", strings.TrimSpace(epochText))
	}
	if explicitEpoch != 0 && explicitEpoch != epoch {
		return "", 0, fmt.Errorf(
			"source-date-epoch %d does not match commit epoch %d",
			explicitEpoch,
			epoch,
		)
	}
	return commit, epoch, nil
}

func validateProduction(
	ctx context.Context,
	directory string,
	version string,
	commit string,
	runner process.Runner,
) error {
	status, err := runner.Run(ctx, directory, "git", "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("inspect release checkout: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		return errors.New("production release checkout must be clean, including untracked files")
	}
	tagCommit, err := runner.Run(
		ctx,
		directory,
		"git",
		"rev-parse",
		"--verify",
		"refs/tags/"+version+"^{commit}",
	)
	if err != nil {
		return fmt.Errorf("resolve release tag %q: %w", version, err)
	}
	if strings.TrimSpace(tagCommit) != commit {
		return fmt.Errorf("release tag %q does not resolve to HEAD", version)
	}
	return nil
}

func artifactNames(repository string, version string, signed bool) []string {
	base := repository + "_" + strings.TrimPrefix(version, "v")
	result := []string{
		"checksums.txt",
		base + "_sbom.spdx.json",
		base + "_source.tar.gz",
	}
	if signed {
		result = append(result, "checksums.txt.pem", "checksums.txt.sig")
	}
	slices.Sort(result)
	return result
}
