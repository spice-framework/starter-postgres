package libraryrelease

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spice-framework/development/internal/catalog"
)

const maximumPlanBytes = 64 << 10

// Result describes one atomically committed release directory.
type Result struct {
	OutputDir string
	Files     []string
}

// ParsePlan strictly decodes a release plan. Persisted provenance fields are
// intentionally not authoritative: rendering or signing reconstructs them from
// the current catalog, checkout origin, and selected commit before creating
// output.
func ParsePlan(content []byte) (Plan, error) {
	if len(content) > maximumPlanBytes {
		return Plan{}, fmt.Errorf("library release plan exceeds %d bytes", maximumPlanBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var plan Plan
	if err := decoder.Decode(&plan); err != nil {
		return Plan{}, fmt.Errorf("decode library release plan: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Plan{}, errors.New("library release plan has trailing JSON values")
		}
		return Plan{}, fmt.Errorf("decode trailing library release plan: %w", err)
	}
	if err := validatePersistedPlan(plan); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

// LoadPlan reads one bounded regular plan file without following a final-path
// symlink and then applies strict decoding.
func LoadPlan(filename string) (_ Plan, resultErr error) {
	if strings.TrimSpace(filename) == "" {
		return Plan{}, errors.New("library release plan file is required")
	}
	absolute, err := filepath.Abs(filename)
	if err != nil {
		return Plan{}, fmt.Errorf("resolve library release plan: %w", err)
	}
	root, err := os.OpenRoot(filepath.Dir(absolute))
	if err != nil {
		return Plan{}, fmt.Errorf("open library release plan root: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	info, err := root.Lstat(filepath.Base(absolute))
	if err != nil {
		return Plan{}, fmt.Errorf("inspect library release plan: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maximumPlanBytes {
		return Plan{}, fmt.Errorf("library release plan must be a regular file bounded to %d bytes", maximumPlanBytes)
	}
	file, err := root.Open(filepath.Base(absolute))
	if err != nil {
		return Plan{}, fmt.Errorf("open library release plan: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	openedInfo, err := file.Stat()
	if err != nil {
		return Plan{}, fmt.Errorf("inspect open library release plan: %w", err)
	}
	if !openedInfo.Mode().IsRegular() || openedInfo.Size() < 0 ||
		openedInfo.Size() > maximumPlanBytes || !os.SameFile(info, openedInfo) {
		return Plan{}, errors.New("library release plan changed while it was opened")
	}
	content, err := io.ReadAll(io.LimitReader(file, maximumPlanBytes+1))
	if err != nil {
		return Plan{}, fmt.Errorf("read library release plan: %w", err)
	}
	return ParsePlan(content)
}

// Render creates deterministic unsigned source, SBOM, and checksum artifacts
// from the exact committed Git objects selected by a persisted plan. The plan
// authorizes only its schema, mode, version, commit, and commit epoch. Catalog
// identity and committed policy files rebind every provenance field before it
// can influence names or artifact content.
//
// The output's nearest existing ancestor is a trust boundary: callers must
// control it for the duration of Render. Descendants are checked or created one
// component at a time without accepting symlinks, staging stays on the same
// filesystem, and the final platform operation is atomic and no-replace. A
// platform without an atomic no-replace rename fails closed.
func Render(
	ctx context.Context,
	repositoryRoot string,
	outputDirectory string,
	plan Plan,
	value catalog.Catalog,
) (Result, error) {
	prepared, err := prepareRelease(ctx, repositoryRoot, plan, value, "rehearsal")
	if err != nil {
		return Result{}, err
	}
	artifacts, err := renderArtifacts(ctx, prepared.plan, prepared.tree)
	if err != nil {
		return Result{}, err
	}
	return commitReleaseArtifacts(outputDirectory, prepared.plan, artifacts, nil)
}

// Sign creates one deterministic production release from exact committed
// objects, authenticates the signing key against a separately trusted public
// key, revalidates the clean exact-tagged checkout, and atomically commits five
// artifacts without replacing an existing output directory.
func Sign(
	ctx context.Context,
	repositoryRoot string,
	outputDirectory string,
	plan Plan,
	value catalog.Catalog,
	files SigningFiles,
) (Result, error) {
	prepared, err := prepareRelease(ctx, repositoryRoot, plan, value, "production")
	if err != nil {
		return Result{}, err
	}
	if err := requireOutsideRepository(prepared.root, outputDirectory, "production output"); err != nil {
		return Result{}, err
	}
	if err := requireOutsideRepository(prepared.root, files.PrivateKey, "release private key"); err != nil {
		return Result{}, err
	}
	artifacts, err := renderArtifacts(ctx, prepared.plan, prepared.tree)
	if err != nil {
		return Result{}, err
	}
	material, err := loadSigningMaterial(files)
	if err != nil {
		return Result{}, err
	}
	defer material.clear()
	signature, publicPEM, err := material.sign(artifacts["checksums.txt"])
	if err != nil {
		return Result{}, err
	}
	artifacts["checksums.txt.pem"] = publicPEM
	artifacts["checksums.txt.sig"] = signature
	return commitReleaseArtifacts(
		outputDirectory,
		prepared.plan,
		artifacts,
		func() error {
			return validateProductionState(ctx, prepared.root, prepared.plan)
		},
	)
}

type preparedRelease struct {
	root string
	plan Plan
	tree committedTree
}

func prepareRelease(
	ctx context.Context,
	repositoryRoot string,
	plan Plan,
	value catalog.Catalog,
	mode string,
) (preparedRelease, error) {
	if ctx == nil {
		return preparedRelease{}, errors.New("render library release: context must not be nil")
	}
	if err := validatePersistedPlan(plan); err != nil {
		return preparedRelease{}, err
	}
	if plan.Mode != mode {
		return preparedRelease{}, fmt.Errorf("library release %s requires a %s plan", mode, mode)
	}
	if err := value.Validate(); err != nil {
		return preparedRelease{}, err
	}
	root, err := libraryDirectory(repositoryRoot)
	if err != nil {
		return preparedRelease{}, err
	}
	if mode == "production" {
		if err := validateProductionState(ctx, root, plan); err != nil {
			return preparedRelease{}, err
		}
	} else if err := validateCommitEpoch(ctx, root, plan); err != nil {
		return preparedRelease{}, err
	}
	tree, err := readCommittedTree(ctx, root, plan.Commit)
	if err != nil {
		return preparedRelease{}, err
	}
	plan, err = rebindPlanProvenance(ctx, root, plan, value, tree)
	if err != nil {
		return preparedRelease{}, err
	}
	if err := validateRenderPlan(plan); err != nil {
		return preparedRelease{}, err
	}
	return preparedRelease{root: root, plan: plan, tree: tree}, nil
}

func commitReleaseArtifacts(
	outputDirectory string,
	plan Plan,
	artifacts map[string][]byte,
	beforeCommit func() error,
) (result Result, resultErr error) {
	files := make([]string, 0, len(artifacts))
	for name := range artifacts {
		files = append(files, name)
	}
	slices.Sort(files)
	if !slices.Equal(files, plan.Artifacts) {
		return Result{}, errors.New("rendered artifacts do not match the validated plan")
	}
	output, staging, err := prepareStaging(outputDirectory)
	if err != nil {
		return Result{}, err
	}
	committed := false
	defer func() {
		if !committed {
			resultErr = errors.Join(resultErr, os.RemoveAll(staging))
		}
	}()
	for _, name := range files {
		if err := writeNewArtifact(staging, name, artifacts[name]); err != nil {
			return Result{}, err
		}
	}
	if beforeCommit != nil {
		if err := beforeCommit(); err != nil {
			return Result{}, err
		}
	}
	if err := commitStaging(staging, output); err != nil {
		return Result{}, err
	}
	committed = true
	return Result{OutputDir: output, Files: files}, nil
}

// validatePersistedPlan checks only fields that a persisted plan is allowed to
// authorize. All descriptive and derived fields are untrusted until
// rebindPlanProvenance reconstructs them.
func validatePersistedPlan(plan Plan) error {
	if plan.Schema != PlanSchema {
		return fmt.Errorf("library release plan schema %d is unsupported", plan.Schema)
	}
	if plan.Mode != "rehearsal" && plan.Mode != "production" {
		return fmt.Errorf("library release plan mode %q is unsupported", plan.Mode)
	}
	if !catalogVersion(plan.Version) || !commitPattern.MatchString(plan.Commit) ||
		plan.SourceDateEpoch <= 0 {
		return errors.New("library release version, commit, or source epoch is invalid")
	}
	return nil
}

func rebindPlanProvenance(
	ctx context.Context,
	repositoryRoot string,
	plan Plan,
	value catalog.Catalog,
	tree committedTree,
) (Plan, error) {
	repository, err := catalogRepositoryForOrigin(ctx, repositoryRoot, value)
	if err != nil {
		return Plan{}, err
	}
	goMod, found := tree.files["go.mod"]
	if !found {
		return Plan{}, errors.New("release source has no committed go.mod")
	}
	module, err := parseCommittedModfile(ctx, goMod)
	if err != nil {
		return Plan{}, err
	}
	if module.Module.Path != repository.Module {
		return Plan{}, fmt.Errorf(
			"committed go.mod declares %q; catalog origin requires %q",
			module.Module.Path,
			repository.Module,
		)
	}
	compatibilityContent, found := tree.files[value.StarterCompatibility.MetadataFile]
	if !found {
		return Plan{}, fmt.Errorf(
			"release commit is missing compatibility metadata %q",
			value.StarterCompatibility.MetadataFile,
		)
	}
	compatibility, err := catalog.ParseStarterCompatibility(
		compatibilityContent,
		value.StarterCompatibility,
	)
	if err != nil {
		return Plan{}, fmt.Errorf("validate committed compatibility metadata: %w", err)
	}

	plan.Repository = repository.Name
	plan.Module = repository.Module
	plan.Source = repository.CanonicalURL
	plan.CompatibilityMinimum = compatibility.Minimum
	plan.CompatibilityCurrent = value.StarterCompatibility.CurrentCore
	plan.RequiredFiles = slices.Clone(requiredFiles)
	plan.RequiredFiles = append(plan.RequiredFiles, value.StarterCompatibility.MetadataFile)
	slices.Sort(plan.RequiredFiles)
	plan.Artifacts = artifactNames(repository.Name, plan.Version, plan.Mode == "production")
	return plan, nil
}

func catalogRepositoryForOrigin(
	ctx context.Context,
	repositoryRoot string,
	value catalog.Catalog,
) (catalog.Repository, error) {
	content, err := gitBytes(ctx, repositoryRoot, 4096, "remote", "get-url", "origin")
	if err != nil {
		return catalog.Repository{}, fmt.Errorf("resolve renderer origin: %w", err)
	}
	origin := strings.TrimSpace(string(content))
	var selected *catalog.Repository
	for index := range value.Repositories {
		repository := &value.Repositories[index]
		match, matchErr := sameGitRemote(origin, repository.CloneURL)
		if matchErr != nil {
			return catalog.Repository{}, fmt.Errorf(
				"validate catalog origin for %q: %w",
				repository.Name,
				matchErr,
			)
		}
		if !match {
			continue
		}
		if selected != nil {
			return catalog.Repository{}, fmt.Errorf(
				"renderer origin %q ambiguously matches catalog repositories %q and %q",
				origin,
				selected.Name,
				repository.Name,
			)
		}
		selected = repository
	}
	if selected == nil {
		return catalog.Repository{}, fmt.Errorf(
			"renderer origin %q does not identify a catalog repository",
			origin,
		)
	}
	return selectLibrary(value, selected.Name)
}

func validateRenderPlan(plan Plan) error {
	if err := validatePersistedPlan(plan); err != nil {
		return err
	}
	if strings.TrimSpace(plan.Repository) == "" || filepath.Base(plan.Repository) != plan.Repository ||
		strings.ContainsAny(plan.Repository, `\/:`) {
		return fmt.Errorf("library release repository %q is unsafe", plan.Repository)
	}
	if strings.TrimSpace(plan.Module) == "" || strings.TrimSpace(plan.Source) == "" {
		return errors.New("library release module and source must be explicit")
	}
	if !strings.HasPrefix(plan.Source, "https://") {
		return errors.New("library release source must be an HTTPS repository URL")
	}
	if _, err := gitRemoteIdentity(plan.Source); err != nil {
		return fmt.Errorf("library release source is invalid: %w", err)
	}
	if !catalogVersion(plan.CompatibilityMinimum) || !catalogVersion(plan.CompatibilityCurrent) {
		return errors.New("library release compatibility boundaries are invalid")
	}
	if plan.CompatibilityMinimum == plan.CompatibilityCurrent {
		return errors.New("library release compatibility boundaries must differ")
	}
	if len(plan.RequiredFiles) == 0 || !slices.IsSorted(plan.RequiredFiles) {
		return errors.New("library release required files must be nonempty and sorted")
	}
	for index, name := range plan.RequiredFiles {
		if name == "" || filepath.IsAbs(name) || filepath.ToSlash(filepath.Clean(name)) != name ||
			name == "." || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("library release required file %q is unsafe", name)
		}
		if index != 0 && plan.RequiredFiles[index-1] == name {
			return fmt.Errorf("library release required file %q is duplicated", name)
		}
	}
	wantRequired := slices.Clone(requiredFiles)
	wantRequired = append(wantRequired, "spice-compatibility.json")
	slices.Sort(wantRequired)
	if !slices.Equal(plan.RequiredFiles, wantRequired) {
		return fmt.Errorf("library release required files %v do not match schema contract %v", plan.RequiredFiles, wantRequired)
	}
	wantArtifacts := artifactNames(plan.Repository, plan.Version, plan.Mode == "production")
	if !slices.Equal(plan.Artifacts, wantArtifacts) {
		return fmt.Errorf("library release artifacts %v do not match contract %v", plan.Artifacts, wantArtifacts)
	}
	return nil
}

func catalogVersion(version string) bool {
	return catalog.ValidModuleVersion(version)
}

func renderArtifacts(
	ctx context.Context,
	plan Plan,
	tree committedTree,
) (map[string][]byte, error) {
	for _, name := range plan.RequiredFiles {
		if _, found := tree.files[name]; !found {
			return nil, fmt.Errorf("release commit is missing required file %q", name)
		}
		index := slices.IndexFunc(tree.entries, func(entry gitTreeEntry) bool {
			return entry.name == name
		})
		if index < 0 || (tree.entries[index].mode != "100644" && tree.entries[index].mode != "100755") {
			return nil, fmt.Errorf("required release file %q is not a regular Git blob", name)
		}
	}
	if err := validateCommittedCompatibility(plan, tree.files["spice-compatibility.json"]); err != nil {
		return nil, err
	}
	archiveName := plan.Repository + "_" + strings.TrimPrefix(plan.Version, "v") + "_source.tar.gz"
	archive, err := buildSourceArchive(plan, tree.entries)
	if err != nil {
		return nil, err
	}
	sbomName := plan.Repository + "_" + strings.TrimPrefix(plan.Version, "v") + "_sbom.spdx.json"
	sbom, err := buildSBOM(ctx, plan, tree.files)
	if err != nil {
		return nil, err
	}
	result := map[string][]byte{archiveName: archive, sbomName: sbom}
	checksums, err := artifactChecksums(result)
	if err != nil {
		return nil, err
	}
	result["checksums.txt"] = checksums
	return result, nil
}

func validateCommittedCompatibility(plan Plan, content []byte) error {
	metadata, err := catalog.ParseStarterCompatibility(
		content,
		catalog.StarterCompatibilityPolicy{
			RepositoryPrefix: "starter-",
			MetadataFile:     "spice-compatibility.json",
			MetadataSchema:   1,
			CoreModule:       "github.com/spice-framework/spice",
			CurrentCore:      plan.CompatibilityCurrent,
		},
	)
	if err != nil {
		return fmt.Errorf("validate committed compatibility metadata: %w", err)
	}
	if metadata.Minimum != plan.CompatibilityMinimum {
		return fmt.Errorf(
			"committed compatibility minimum %q does not match plan %q",
			metadata.Minimum,
			plan.CompatibilityMinimum,
		)
	}
	return nil
}

func artifactChecksums(artifacts map[string][]byte) ([]byte, error) {
	names := make([]string, 0, len(artifacts))
	for name := range artifacts {
		if filepath.Base(name) != name || name == "checksums.txt" {
			return nil, fmt.Errorf("checksum artifact name %q is invalid", name)
		}
		names = append(names, name)
	}
	slices.Sort(names)
	var output strings.Builder
	for _, name := range names {
		sum := sha256.Sum256(artifacts[name])
		fmt.Fprintf(&output, "%s  %s\n", hex.EncodeToString(sum[:]), name)
	}
	return []byte(output.String()), nil
}

func prepareStaging(configuredOutput string) (string, string, error) {
	if strings.TrimSpace(configuredOutput) == "" {
		return "", "", errors.New("library release output directory is required")
	}
	output, err := filepath.Abs(configuredOutput)
	if err != nil {
		return "", "", fmt.Errorf("resolve library release output: %w", err)
	}
	parent := filepath.Dir(output)
	if err := ensureRealDirectory(parent); err != nil {
		return "", "", fmt.Errorf("create library release parent: %w", err)
	}
	if _, err := os.Lstat(output); err == nil {
		return "", "", fmt.Errorf("library release output %q already exists", output)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", fmt.Errorf("inspect library release output: %w", err)
	}
	staging, err := os.MkdirTemp(parent, ".spice-library-release-*")
	if err != nil {
		return "", "", fmt.Errorf("create library release staging directory: %w", err)
	}
	return output, staging, nil
}

func ensureRealDirectory(directory string) error {
	info, err := os.Lstat(directory)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("path %q is not a real directory", directory)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect path %q: %w", directory, err)
	}
	parent := filepath.Dir(directory)
	if parent == directory {
		return fmt.Errorf("path %q has no existing directory ancestor", directory)
	}
	if err := ensureRealDirectory(parent); err != nil {
		return err
	}
	if err := os.Mkdir(directory, 0o750); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create path %q: %w", directory, err)
	}
	info, err = os.Lstat(directory)
	if err != nil {
		return fmt.Errorf("inspect created path %q: %w", directory, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("created path %q is not a real directory", directory)
	}
	return nil
}

func commitStaging(staging string, output string) error {
	if err := renameNoReplace(staging, output); err != nil {
		if _, inspectErr := os.Lstat(output); inspectErr == nil {
			return fmt.Errorf("library release output %q already exists", output)
		}
		return fmt.Errorf("commit library release directory without replacement: %w", err)
	}
	return nil
}

func writeNewArtifact(rootPath string, name string, content []byte) (resultErr error) {
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return fmt.Errorf("open library release staging root: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create library release artifact %q: %w", name, err)
	}
	written, writeErr := file.Write(content)
	if writeErr == nil && written != len(content) {
		writeErr = io.ErrShortWrite
	}
	if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return fmt.Errorf("write library release artifact %q: %w", name, err)
	}
	return nil
}
