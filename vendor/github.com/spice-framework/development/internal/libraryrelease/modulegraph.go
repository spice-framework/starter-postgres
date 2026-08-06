package libraryrelease

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spice-framework/development/internal/catalog"
	"github.com/spice-framework/development/internal/process"
)

const maximumModuleGraphBytes = 16 << 20

type moduleIdentity struct {
	Path    string
	Version string
}

type moduleMetadata struct {
	Module  moduleIdentity
	Require []struct {
		Path     string
		Version  string
		Indirect bool
	}
	Replace []struct {
		Old moduleIdentity
		New moduleIdentity
	}
}

type listedModule struct {
	Path    string
	Version string
	Replace string
}

func committedModules(
	ctx context.Context,
	plan Plan,
	files map[string][]byte,
) ([]listedModule, error) {
	goMod, found := files["go.mod"]
	if !found {
		return nil, errors.New("release source has no committed go.mod")
	}
	metadata, err := parseCommittedModfile(ctx, goMod)
	if err != nil {
		return nil, err
	}
	if metadata.Module.Path != plan.Module {
		return nil, fmt.Errorf("committed go.mod declares %q; plan requires %q", metadata.Module.Path, plan.Module)
	}
	selected, err := selectedModules(metadata)
	if err != nil {
		return nil, err
	}
	if err := validateCoreCompatibility(selected, plan); err != nil {
		return nil, err
	}
	goSum, found := files["go.sum"]
	if !found {
		return nil, errors.New("release source has no committed go.sum")
	}
	if err := validateModuleSums(selected, goSum); err != nil {
		return nil, err
	}
	vendor, found := files["vendor/modules.txt"]
	if !found {
		return nil, errors.New("release source has no committed vendor/modules.txt")
	}
	if len(vendor) > maximumModuleGraphBytes {
		return nil, fmt.Errorf("committed vendor graph exceeds %d bytes", maximumModuleGraphBytes)
	}
	actual, err := parseVendorModules(vendor)
	if err != nil {
		return nil, err
	}
	if err := validateVendorGraph(selected, actual); err != nil {
		return nil, err
	}
	slices.SortFunc(actual, func(left, right listedModule) int {
		return strings.Compare(left.Path, right.Path)
	})
	return actual, nil
}

func validateCoreCompatibility(modules []listedModule, plan Plan) error {
	for _, module := range modules {
		if module.Path != "github.com/spice-framework/spice" {
			continue
		}
		if module.Version != plan.CompatibilityMinimum || module.Replace != "" {
			return fmt.Errorf(
				"committed core module is %s => %s; plan requires %s without replacement",
				module.Version,
				module.Replace,
				plan.CompatibilityMinimum,
			)
		}
		return nil
	}
	return errors.New("committed go.mod does not require the Spice core module")
}

func parseCommittedModfile(
	ctx context.Context,
	content []byte,
) (metadata moduleMetadata, resultErr error) {
	directory, err := os.MkdirTemp("", "spice-library-release-modfile-*")
	if err != nil {
		return moduleMetadata{}, fmt.Errorf("create committed modfile parser directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(directory); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("remove committed modfile parser directory: %w", err))
		}
	}()
	name := filepath.Join(directory, "go.mod")
	// #nosec G703 -- directory is the exact path returned by os.MkdirTemp and
	// the child name is fixed.
	if err := os.WriteFile(name, content, 0o600); err != nil {
		return moduleMetadata{}, fmt.Errorf("write committed modfile parser input: %w", err)
	}
	command := exec.CommandContext(ctx, "go", "mod", "edit", "-json")
	command.Dir = directory
	command.Env = process.IndependentEnvironment()
	var stdout limitedBuffer
	stdout.maximum = maximumModuleGraphBytes
	var stderr limitedBuffer
	stderr.maximum = maximumGitDiagnostic
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return moduleMetadata{}, fmt.Errorf("parse committed go.mod: %w: %s", err, stderr.String())
	}
	if stdout.truncated {
		return moduleMetadata{}, fmt.Errorf("committed go.mod metadata exceeds %d bytes", maximumModuleGraphBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	if err := decoder.Decode(&metadata); err != nil {
		return moduleMetadata{}, fmt.Errorf("decode committed go.mod: %w", err)
	}
	return metadata, nil
}

func selectedModules(metadata moduleMetadata) ([]listedModule, error) {
	replacements := make(map[string]moduleIdentity, len(metadata.Replace))
	for _, replacement := range metadata.Replace {
		key := replacement.Old.Path + "@" + replacement.Old.Version
		if _, duplicate := replacements[key]; duplicate {
			return nil, fmt.Errorf("committed go.mod has duplicate replacement for %s", key)
		}
		if replacement.New.Version == "" || isLocalReplacement(replacement.New.Path) {
			return nil, fmt.Errorf("committed go.mod contains local replacement %q", replacement.New.Path)
		}
		replacements[key] = replacement.New
	}
	seen := make(map[string]struct{}, len(metadata.Require))
	result := make([]listedModule, 0, len(metadata.Require))
	for _, requirement := range metadata.Require {
		if _, duplicate := seen[requirement.Path]; duplicate {
			return nil, fmt.Errorf("committed go.mod requires %s more than once", requirement.Path)
		}
		seen[requirement.Path] = struct{}{}
		module := listedModule{Path: requirement.Path, Version: requirement.Version}
		replacement, found := replacements[requirement.Path+"@"+requirement.Version]
		if !found {
			replacement, found = replacements[requirement.Path+"@"]
		}
		if found {
			module.Replace = replacement.Path + " " + replacement.Version
		}
		result = append(result, module)
	}
	return result, nil
}

func validateModuleSums(modules []listedModule, sums []byte) error {
	for _, module := range modules {
		modulePath, version := module.Path, module.Version
		if module.Replace != "" {
			fields := strings.Fields(module.Replace)
			modulePath, version = fields[0], fields[1]
		}
		if !containsModuleSum(sums, modulePath, version) {
			return fmt.Errorf("committed go.sum has no checksum for %s %s", modulePath, version)
		}
	}
	return nil
}

func containsModuleSum(content []byte, modulePath string, version string) bool {
	for line := range strings.SplitSeq(string(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == modulePath &&
			(fields[1] == version || fields[1] == version+"/go.mod") &&
			validGoSumHash(fields[2]) {
			return true
		}
	}
	return false
}

func validGoSumHash(value string) bool {
	encoded, found := strings.CutPrefix(value, "h1:")
	if !found {
		return false
	}
	digest, err := base64.StdEncoding.DecodeString(encoded)
	return err == nil && len(digest) == 32
}

func parseVendorModules(content []byte) ([]listedModule, error) {
	var result []listedModule
	replacementMarkers := make(map[string]string)
	for line := range strings.SplitSeq(string(content), "\n") {
		if !strings.HasPrefix(line, "# ") || strings.HasPrefix(line, "## ") {
			continue
		}
		header := strings.TrimPrefix(line, "# ")
		if module, found := parseVendorModule(header); found {
			if isLocalReplacement(module.Replace) {
				return nil, fmt.Errorf("committed vendor graph contains local replacement %q", module.Replace)
			}
			result = append(result, module)
			continue
		}
		modulePath, replacement, found := parseVendorReplacementMarker(header)
		if !found {
			return nil, fmt.Errorf("committed vendor graph has malformed module header %q", header)
		}
		if isLocalReplacement(replacement) {
			return nil, fmt.Errorf("committed vendor graph contains local replacement %q", replacement)
		}
		if _, duplicate := replacementMarkers[modulePath]; duplicate {
			return nil, fmt.Errorf("committed vendor graph repeats replacement marker for %s", modulePath)
		}
		replacementMarkers[modulePath] = replacement
	}
	modules := make(map[string]listedModule, len(result))
	for _, module := range result {
		if _, duplicate := modules[module.Path]; duplicate {
			return nil, fmt.Errorf("committed vendor graph contains duplicate module %s", module.Path)
		}
		modules[module.Path] = module
	}
	for modulePath, replacement := range replacementMarkers {
		module, found := modules[modulePath]
		if !found || module.Replace != replacement {
			return nil, fmt.Errorf("committed vendor replacement marker for %s does not match a selected header", modulePath)
		}
	}
	for _, module := range result {
		if module.Replace != "" && replacementMarkers[module.Path] != module.Replace {
			return nil, fmt.Errorf("committed vendor replacement for %s has no matching marker", module.Path)
		}
	}
	return result, nil
}

func parseVendorModule(line string) (listedModule, bool) {
	left, replacement, replaced := strings.Cut(line, " => ")
	fields := strings.Fields(left)
	if len(fields) != 2 || !catalog.ValidModuleVersion(fields[1]) {
		return listedModule{}, false
	}
	module := listedModule{Path: fields[0], Version: fields[1]}
	if replaced {
		module.Replace = canonicalRemoteReplacement(replacement)
		if module.Replace == "" {
			return listedModule{}, false
		}
	}
	return module, true
}

func parseVendorReplacementMarker(line string) (string, string, bool) {
	modulePath, replacement, found := strings.Cut(line, " => ")
	modulePath = strings.TrimSpace(modulePath)
	replacement = canonicalRemoteReplacement(replacement)
	return modulePath, replacement, found && modulePath != "" &&
		!strings.ContainsAny(modulePath, " \t") && replacement != ""
}

func canonicalRemoteReplacement(value string) string {
	fields := strings.Fields(value)
	if len(fields) != 2 || !catalog.ValidModuleVersion(fields[1]) {
		return ""
	}
	return fields[0] + " " + fields[1]
}

func isLocalReplacement(value string) bool {
	return strings.HasPrefix(value, ".") || strings.HasPrefix(value, "/") ||
		len(value) > 2 && value[1] == ':'
}

func validateVendorGraph(expected []listedModule, actual []listedModule) error {
	selected := make(map[string]listedModule, len(expected))
	for _, module := range expected {
		selected[module.Path] = module
	}
	seen := make(map[string]struct{}, len(actual))
	for _, module := range actual {
		if _, duplicate := seen[module.Path]; duplicate {
			return fmt.Errorf("committed vendor graph contains duplicate module %s", module.Path)
		}
		seen[module.Path] = struct{}{}
		wanted, found := selected[module.Path]
		if !found {
			return fmt.Errorf("committed vendor graph contains unselected module %s", module.Path)
		}
		if wanted.Version != module.Version || wanted.Replace != module.Replace {
			return fmt.Errorf(
				"committed vendor module %s is %s => %s; require %s => %s",
				module.Path,
				module.Version,
				module.Replace,
				wanted.Version,
				wanted.Replace,
			)
		}
	}
	if len(seen) != len(selected) {
		for modulePath := range selected {
			if _, found := seen[modulePath]; !found {
				return fmt.Errorf("committed vendor graph is missing selected module %s", modulePath)
			}
		}
	}
	return nil
}
