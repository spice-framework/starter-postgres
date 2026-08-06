package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const (
	developmentModule  = "github.com/spice-framework/development"
	developmentTool    = developmentModule + "/cmd/spice-dev"
	developmentVersion = "v0.0.0-20260806132124-4c308d1b9fda"
	toolchainModule    = "github.com/spice-framework/toolchain"
	toolchainTool      = toolchainModule + "/cmd/spice-library-release-verify"
	toolchainVersion   = "v0.0.0-20260806133530-71211498297c"
	rehearsalVersion   = "v0.0.0-rehearsal"
)

func requireReleaseTool(ctx context.Context, root string) error {
	content, err := capture(ctx, root, nil, "go", "mod", "edit", "-json")
	if err != nil {
		return fmt.Errorf("read release tool authorization: %w", err)
	}
	return validateReleaseToolAuthorization([]byte(content))
}

func validateReleaseToolAuthorization(content []byte) error {
	var metadata struct {
		Require []struct {
			Path    string
			Version string
		}
		Tool []struct{ Path string }
	}
	if err := json.Unmarshal(content, &metadata); err != nil {
		return fmt.Errorf("decode release tool authorization: %w", err)
	}
	authorizations := [...]struct {
		module  string
		tool    string
		version string
	}{
		{developmentModule, developmentTool, developmentVersion},
		{toolchainModule, toolchainTool, toolchainVersion},
	}
	for _, authorization := range authorizations {
		count := 0
		for _, tool := range metadata.Tool {
			if tool.Path == authorization.tool {
				count++
			}
		}
		if count != 1 {
			return fmt.Errorf("go.mod must authorize exactly one %s tool declaration; found %d", authorization.tool, count)
		}
		found := false
		for _, requirement := range metadata.Require {
			if requirement.Path != authorization.module {
				continue
			}
			found = true
			if requirement.Version != authorization.version {
				return fmt.Errorf("go.mod selects release tool %s; require exactly %s", requirement.Version, authorization.version)
			}
		}
		if !found {
			return fmt.Errorf("go.mod must require %s at exactly %s", authorization.module, authorization.version)
		}
	}
	return nil
}

func releaseRehearsal(ctx context.Context, root string) error {
	parent, err := os.MkdirTemp("", "starter-postgres-release-rehearsal-*")
	if err != nil {
		return fmt.Errorf("create release rehearsal root: %w", err)
	}
	defer removeTree(parent)

	offlineVendor := map[string]string{"GOFLAGS": "-mod=vendor"}
	resolved, err := capture(ctx, root, offlineVendor, "go", "tool", "-n", developmentTool)
	if err != nil {
		return fmt.Errorf("resolve authorized central release tool: %w", err)
	}
	if strings.TrimSpace(resolved) == "" {
		return errors.New("resolve authorized central release tool: empty executable path")
	}
	plan, err := capture(ctx, root, offlineVendor, "go", "tool", developmentTool,
		"library-release", "plan", "--root="+root, "--repo=starter-postgres",
		"--version="+rehearsalVersion, "--rehearsal")
	if err != nil {
		return fmt.Errorf("plan central release rehearsal: %w", err)
	}
	planFile := filepath.Join(parent, "plan.json")
	if writeErr := os.WriteFile(planFile, []byte(plan+"\n"), 0o600); writeErr != nil {
		return fmt.Errorf("write central release rehearsal plan: %w", writeErr)
	}
	outputs := []string{filepath.Join(parent, "first"), filepath.Join(parent, "second")}
	for _, outputDir := range outputs {
		if renderErr := command(ctx, root, offlineVendor, "go", "tool", developmentTool,
			"library-release", "render", "--root="+root, "--plan="+planFile, "--output="+outputDir); renderErr != nil {
			return fmt.Errorf("render central release rehearsal: %w", renderErr)
		}
	}
	artifacts, err := deterministicReleaseArtifacts(outputs)
	if err != nil {
		return err
	}
	return validateReleaseRehearsal(outputs[0], artifacts)
}

func deterministicReleaseArtifacts(outputs []string) (map[string][sha256.Size]byte, error) {
	if len(outputs) != 2 {
		return nil, fmt.Errorf("release rehearsal requires exactly two outputs; got %d", len(outputs))
	}
	first, err := treeDigests(outputs[0])
	if err != nil {
		return nil, err
	}
	second, err := treeDigests(outputs[1])
	if err != nil {
		return nil, err
	}
	if !maps.Equal(first, second) {
		return nil, errors.New("identical central release rehearsals produced different artifacts")
	}
	return first, nil
}

func validateReleaseRehearsal(root string, artifacts map[string][sha256.Size]byte) error {
	base := "starter-postgres_" + strings.TrimPrefix(rehearsalVersion, "v")
	archive := base + "_source.tar.gz"
	sbom := base + "_sbom.spdx.json"
	expected := []string{"checksums.txt", sbom, archive}
	actual := slices.Sorted(maps.Keys(artifacts))
	if !slices.Equal(actual, expected) {
		return fmt.Errorf("release rehearsal artifacts %v do not match %v; signatures are forbidden", actual, expected)
	}
	if err := validateReleaseChecksums(root, artifacts, sbom, archive); err != nil {
		return err
	}
	content, err := readReleaseArtifact(root, sbom)
	if err != nil {
		return err
	}
	return validateReleaseSBOM(content)
}

func validateReleaseChecksums(root string, artifacts map[string][sha256.Size]byte, names ...string) error {
	content, err := readReleaseArtifact(root, "checksums.txt")
	if err != nil {
		return err
	}
	if len(content) == 0 || content[len(content)-1] != '\n' {
		return errors.New("checksums.txt must end with one newline")
	}
	lines := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
	ordered := slices.Clone(names)
	slices.Sort(ordered)
	if len(lines) != len(ordered) {
		return fmt.Errorf("checksums.txt has %d lines; require %d", len(lines), len(ordered))
	}
	for index, name := range ordered {
		want := fmt.Sprintf("%x  %s", artifacts[name], name)
		if lines[index] != want {
			return fmt.Errorf("checksums.txt line %d is %q; require canonical %q", index+1, lines[index], want)
		}
	}
	return nil
}

type releaseSBOM struct {
	SPDXVersion       string           `json:"spdxVersion"`
	DataLicense       string           `json:"dataLicense"`
	SPDXID            string           `json:"SPDXID"`
	Name              string           `json:"name"`
	DocumentNamespace string           `json:"documentNamespace"`
	CreationInfo      sbomCreationInfo `json:"creationInfo"`
	Packages          []map[string]any `json:"packages"`
	Relationships     []map[string]any `json:"relationships"`
}

type sbomCreationInfo struct {
	Created  string   `json:"created"`
	Creators []string `json:"creators"`
}

func validateReleaseSBOM(content []byte) error {
	sbom, err := decodeReleaseSBOM(content)
	if err != nil {
		return fmt.Errorf("decode release SBOM: %w", err)
	}
	prefix := "https://github.com/spice-framework/starter-postgres/releases/" + rehearsalVersion + "/spdx/v1/"
	if sbom.SPDXVersion != "SPDX-2.3" || sbom.DataLicense != "CC0-1.0" || sbom.SPDXID != "SPDXRef-DOCUMENT" ||
		sbom.Name != "starter-postgres "+rehearsalVersion || !validSBOMNamespace(sbom.DocumentNamespace, prefix) ||
		!slices.Equal(sbom.CreationInfo.Creators, []string{
			"Organization: Spice Framework",
			"Tool: github.com/spice-framework/development/cmd/spice-dev library-release renderer/v1",
		}) || len(sbom.Packages) == 0 || len(sbom.Relationships) == 0 {
		return errors.New("release SBOM does not match the central renderer/v1 contract")
	}
	return nil
}

func decodeReleaseSBOM(content []byte) (releaseSBOM, error) {
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var result releaseSBOM
	if err := decoder.Decode(&result); err != nil {
		return releaseSBOM{}, err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return releaseSBOM{}, errors.New("release SBOM has trailing JSON values")
		}
		return releaseSBOM{}, err
	}
	return result, nil
}

func validSBOMNamespace(value, prefix string) bool {
	digest, found := strings.CutPrefix(value, prefix)
	if !found || len(digest) != sha256.Size*2 {
		return false
	}
	for _, character := range digest {
		decimal := character >= '0' && character <= '9'
		hexadecimal := character >= 'a' && character <= 'f'
		if !decimal && !hexadecimal {
			return false
		}
	}
	return true
}

func readReleaseArtifact(rootPath, name string) (_ []byte, resultErr error) {
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, fmt.Errorf("open release artifact root: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	content, err := root.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("read release artifact %q: %w", name, err)
	}
	return content, nil
}
