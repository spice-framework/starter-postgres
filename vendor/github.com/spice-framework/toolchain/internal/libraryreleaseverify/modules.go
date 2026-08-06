package libraryreleaseverify

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"
)

const coreModulePath = "github.com/spice-framework/spice"

type listedModule struct {
	path        string
	version     string
	replacement string
}

type compatibilityMetadata struct {
	Schema  int    `json:"schema"`
	Minimum string `json:"minimum"`
	Current string `json:"current"`
}

func sourceModules(files map[string][]byte) ([]listedModule, string, error) {
	goMod, found := files["go.mod"]
	if !found || len(goMod) > rendererV1MaxModuleGraphBytes {
		return nil, "", errors.New("trusted source has no bounded go.mod")
	}
	parsed, parseErr := modfile.Parse("go.mod", goMod, nil)
	if parseErr != nil {
		return nil, "", fmt.Errorf("parse trusted source go.mod: %w", parseErr)
	}
	if parsed.Module == nil || strings.TrimSpace(parsed.Module.Mod.Path) == "" {
		return nil, "", errors.New("trusted source go.mod has no module directive")
	}
	replacements, replacementErr := moduleReplacements(parsed)
	if replacementErr != nil {
		return nil, "", replacementErr
	}
	selected := make([]listedModule, 0, len(parsed.Require))
	seen := make(map[string]struct{}, len(parsed.Require))
	for _, requirement := range parsed.Require {
		if _, duplicate := seen[requirement.Mod.Path]; duplicate {
			return nil, "", fmt.Errorf("trusted source go.mod repeats requirement %q", requirement.Mod.Path)
		}
		seen[requirement.Mod.Path] = struct{}{}
		if !semver.IsValid(requirement.Mod.Version) ||
			semver.Canonical(requirement.Mod.Version) != requirement.Mod.Version {
			return nil, "", fmt.Errorf("trusted source requirement %s has noncanonical version %q", requirement.Mod.Path, requirement.Mod.Version)
		}
		module := listedModule{path: requirement.Mod.Path, version: requirement.Mod.Version}
		if replacement, found := replacements[requirement.Mod.Path+"@"+requirement.Mod.Version]; found {
			module.replacement = replacement
		} else if replacement, found := replacements[requirement.Mod.Path+"@"]; found {
			module.replacement = replacement
		}
		selected = append(selected, module)
	}
	compatibility, compatibilityErr := parseCompatibility(files["spice-compatibility.json"])
	if compatibilityErr != nil {
		return nil, "", compatibilityErr
	}
	if validationErr := validateCoreModule(selected, compatibility.Minimum); validationErr != nil {
		return nil, "", validationErr
	}
	if validationErr := validateModuleSums(selected, files["go.sum"]); validationErr != nil {
		return nil, "", validationErr
	}
	actual, vendorErr := parseVendorModules(files["vendor/modules.txt"])
	if vendorErr != nil {
		return nil, "", vendorErr
	}
	if validationErr := validateVendorGraph(selected, actual); validationErr != nil {
		return nil, "", validationErr
	}
	slices.SortFunc(actual, func(left, right listedModule) int {
		return strings.Compare(left.path, right.path)
	})
	return actual, parsed.Module.Mod.Path, nil
}

func moduleReplacements(parsed *modfile.File) (map[string]string, error) {
	result := make(map[string]string, len(parsed.Replace))
	for _, replacement := range parsed.Replace {
		if replacement.New.Version == "" || isLocalReplacement(replacement.New.Path) ||
			!semver.IsValid(replacement.New.Version) ||
			semver.Canonical(replacement.New.Version) != replacement.New.Version {
			return nil, fmt.Errorf("trusted source go.mod contains local or invalid replacement %q", replacement.New.Path)
		}
		key := replacement.Old.Path + "@" + replacement.Old.Version
		if _, duplicate := result[key]; duplicate {
			return nil, fmt.Errorf("trusted source go.mod repeats replacement for %q", key)
		}
		result[key] = replacement.New.Path + " " + replacement.New.Version
	}
	return result, nil
}

func parseCompatibility(data []byte) (compatibilityMetadata, error) {
	if len(data) == 0 || len(data) > rendererV1MaxCompatibilityMetadataBytes {
		return compatibilityMetadata{}, errors.New("trusted source compatibility metadata is empty or oversized")
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return compatibilityMetadata{}, fmt.Errorf("validate trusted source compatibility metadata keys: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var metadata compatibilityMetadata
	if err := decoder.Decode(&metadata); err != nil {
		return compatibilityMetadata{}, fmt.Errorf("decode trusted source compatibility metadata: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return compatibilityMetadata{}, errors.New("trusted source compatibility metadata has trailing JSON")
	}
	if metadata.Schema != 1 || !canonicalVersion(metadata.Minimum) ||
		!canonicalVersion(metadata.Current) || metadata.Minimum == metadata.Current {
		return compatibilityMetadata{}, errors.New("trusted source compatibility metadata is invalid")
	}
	return metadata, nil
}

func canonicalVersion(version string) bool {
	if !semver.IsValid(version) {
		return false
	}
	withoutBuild, _, _ := strings.Cut(version, "+")
	return semver.Canonical(version) == withoutBuild
}

func validateCoreModule(modules []listedModule, minimum string) error {
	for _, module := range modules {
		if module.path != coreModulePath {
			continue
		}
		if module.version != minimum || module.replacement != "" {
			return fmt.Errorf("trusted source core module is %s => %s; compatibility minimum requires %s without replacement", module.version, module.replacement, minimum)
		}
		return nil
	}
	return errors.New("trusted source go.mod does not require the Spice core module")
}

func validateModuleSums(modules []listedModule, sums []byte) error {
	if len(sums) == 0 || len(sums) > rendererV1MaxGoSumBytes {
		return errors.New("trusted source go.sum is empty or oversized")
	}
	for _, module := range modules {
		modulePath, version := module.path, module.version
		if module.replacement != "" {
			fields := strings.Fields(module.replacement)
			modulePath, version = fields[0], fields[1]
		}
		if !containsModuleSum(sums, modulePath, version) {
			return fmt.Errorf("trusted source go.sum has no checksum for %s %s", modulePath, version)
		}
	}
	return nil
}

func containsModuleSum(content []byte, modulePath, version string) bool {
	for line := range strings.SplitSeq(string(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == modulePath &&
			(fields[1] == version || fields[1] == version+"/go.mod") && validGoSumHash(fields[2]) {
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
	if len(content) == 0 || len(content) > rendererV1MaxModuleGraphBytes {
		return nil, errors.New("trusted source vendor/modules.txt is empty or oversized")
	}
	lines := strings.Split(string(content), "\n")
	var result []listedModule
	markers := make(map[string]string)
	for index, line := range lines {
		if !strings.HasPrefix(line, "# ") || strings.HasPrefix(line, "## ") {
			continue
		}
		header := strings.TrimPrefix(line, "# ")
		if module, found := parseVendorModule(header); found {
			if index+1 >= len(lines) || !vendorExplicit(lines[index+1]) {
				return nil, fmt.Errorf("trusted vendor module header %q is not followed by an explicit marker", header)
			}
			result = append(result, module)
			continue
		}
		path, replacement, found := parseVendorReplacementMarker(header)
		if !found {
			return nil, fmt.Errorf("trusted vendor graph has malformed module header %q", header)
		}
		if _, duplicate := markers[path]; duplicate {
			return nil, fmt.Errorf("trusted vendor graph repeats replacement marker for %s", path)
		}
		markers[path] = replacement
	}
	modules := make(map[string]listedModule, len(result))
	for _, module := range result {
		if _, duplicate := modules[module.path]; duplicate {
			return nil, fmt.Errorf("trusted vendor graph contains duplicate module %s", module.path)
		}
		modules[module.path] = module
	}
	for modulePath, replacement := range markers {
		module, found := modules[modulePath]
		if !found || module.replacement != replacement {
			return nil, fmt.Errorf("trusted vendor replacement marker for %s does not match a selected header", modulePath)
		}
	}
	for _, module := range result {
		if module.replacement != "" && markers[module.path] != module.replacement {
			return nil, fmt.Errorf("trusted vendor replacement for %s has no matching marker", module.path)
		}
	}
	return result, nil
}

func vendorExplicit(line string) bool {
	if !strings.HasPrefix(line, "## ") {
		return false
	}
	return slices.Contains(
		strings.FieldsFunc(strings.TrimPrefix(line, "## "), func(value rune) bool {
			return value == ';' || value == ' '
		}),
		"explicit",
	)
}

func parseVendorModule(line string) (listedModule, bool) {
	left, replacement, replaced := strings.Cut(line, " => ")
	fields := strings.Fields(left)
	if len(fields) != 2 || !canonicalVersion(fields[1]) {
		return listedModule{}, false
	}
	module := listedModule{path: fields[0], version: fields[1]}
	if replaced {
		module.replacement = canonicalRemoteReplacement(replacement)
		if module.replacement == "" {
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
	if len(fields) != 2 || !canonicalVersion(fields[1]) || isLocalReplacement(fields[0]) {
		return ""
	}
	return fields[0] + " " + fields[1]
}

func isLocalReplacement(value string) bool {
	return strings.HasPrefix(value, ".") || strings.HasPrefix(value, "/") ||
		len(value) > 2 && value[1] == ':'
}

func validateVendorGraph(expected, actual []listedModule) error {
	wanted := make(map[string]listedModule, len(expected))
	for _, module := range expected {
		wanted[module.path] = module
	}
	seen := make(map[string]struct{}, len(actual))
	for _, module := range actual {
		if _, duplicate := seen[module.path]; duplicate {
			return fmt.Errorf("trusted vendor graph contains duplicate module %s", module.path)
		}
		seen[module.path] = struct{}{}
		expectedModule, found := wanted[module.path]
		if !found {
			return fmt.Errorf("trusted vendor graph contains unselected module %s", module.path)
		}
		if expectedModule != module {
			return fmt.Errorf("trusted vendor module %s is %s => %s; require %s => %s", module.path, module.version, module.replacement, expectedModule.version, expectedModule.replacement)
		}
	}
	if len(seen) != len(wanted) {
		for modulePath := range wanted {
			if _, found := seen[modulePath]; !found {
				return fmt.Errorf("trusted vendor graph is missing selected module %s", modulePath)
			}
		}
	}
	return nil
}
