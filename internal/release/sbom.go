package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

const maxModuleGraphBytes = 16 << 20

type listedModule struct {
	Path    string
	Version string
	Main    bool
}

type spdxDocument struct {
	SPDXVersion       string             `json:"spdxVersion"`
	DataLicense       string             `json:"dataLicense"`
	SPDXID            string             `json:"SPDXID"`
	Name              string             `json:"name"`
	DocumentNamespace string             `json:"documentNamespace"`
	CreationInfo      spdxCreationInfo   `json:"creationInfo"`
	Packages          []spdxPackage      `json:"packages"`
	Relationships     []spdxRelationship `json:"relationships"`
}

type spdxCreationInfo struct {
	Created  string   `json:"created"`
	Creators []string `json:"creators"`
}

type spdxPackage struct {
	Name             string `json:"name"`
	SPDXID           string `json:"SPDXID"`
	VersionInfo      string `json:"versionInfo,omitempty"`
	DownloadLocation string `json:"downloadLocation"`
	FilesAnalyzed    bool   `json:"filesAnalyzed"`
	LicenseConcluded string `json:"licenseConcluded"`
	LicenseDeclared  string `json:"licenseDeclared"`
	CopyrightText    string `json:"copyrightText"`
}

type spdxRelationship struct {
	SPDXElementID      string `json:"spdxElementId"`
	RelationshipType   string `json:"relationshipType"`
	RelatedSPDXElement string `json:"relatedSpdxElement"`
}

func buildSBOM(ctx context.Context, root, version string, epoch time.Time) ([]byte, error) {
	modules, err := listModules(ctx, root)
	if err != nil {
		return nil, err
	}
	packages := make([]spdxPackage, 0, len(modules))
	rootID := ""
	for _, module := range modules {
		moduleVersion := module.Version
		if module.Main {
			moduleVersion = version
		}
		id := packageSPDXID(module.Path, moduleVersion)
		if module.Main {
			rootID = id
		}
		packages = append(packages, spdxPackage{
			Name: module.Path, SPDXID: id, VersionInfo: moduleVersion,
			DownloadLocation: "NOASSERTION", FilesAnalyzed: false,
			LicenseConcluded: "NOASSERTION", LicenseDeclared: "NOASSERTION", CopyrightText: "NOASSERTION",
		})
	}
	if rootID == "" {
		return nil, fmt.Errorf("build release SBOM: module graph has no main module")
	}
	relationships := make([]spdxRelationship, 0, len(packages)-1)
	for _, item := range packages {
		if item.SPDXID != rootID {
			relationships = append(relationships, spdxRelationship{SPDXElementID: rootID, RelationshipType: "DEPENDS_ON", RelatedSPDXElement: item.SPDXID})
		}
	}
	var namespace strings.Builder
	namespace.WriteString(version)
	namespace.WriteByte('\n')
	namespace.WriteString(epoch.UTC().Format(time.RFC3339))
	for _, item := range packages {
		fmt.Fprintf(&namespace, "\n%s@%s", item.Name, item.VersionInfo)
	}
	namespaceHash := sha256.Sum256([]byte(namespace.String()))
	document := spdxDocument{
		SPDXVersion: "SPDX-2.3", DataLicense: "CC0-1.0", SPDXID: "SPDXRef-DOCUMENT",
		Name:              "Spice PostgreSQL Starter " + version,
		DocumentNamespace: "https://github.com/spice-framework/starter-postgres/releases/" + version + "/spdx/" + hex.EncodeToString(namespaceHash[:]),
		CreationInfo: spdxCreationInfo{Created: epoch.UTC().Format(time.RFC3339), Creators: []string{
			"Organization: Spice Framework", "Tool: github.com/spice-framework/starter-postgres/cmd/starter-postgres-release",
		}},
		Packages: packages, Relationships: relationships,
	}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode release SBOM: %w", err)
	}
	return append(data, '\n'), nil
}

func listModules(ctx context.Context, root string) ([]listedModule, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("read release module graph: %w", err)
	}
	goMod, err := readScopedFile(root, "go.mod")
	if err != nil {
		return nil, fmt.Errorf("read release go.mod: %w", err)
	}
	modulePath, requirements, err := parseGoMod(string(goMod))
	if err != nil {
		return nil, err
	}
	vendorData, err := readScopedFile(root, "vendor/modules.txt")
	if err != nil {
		return nil, fmt.Errorf("read release vendor module graph: %w", err)
	}
	if len(vendorData) > maxModuleGraphBytes {
		return nil, fmt.Errorf("read release vendor module graph: file exceeds %d bytes", maxModuleGraphBytes)
	}
	vendored := make(map[string]string)
	for line := range strings.SplitSeq(string(vendorData), "\n") {
		if !strings.HasPrefix(line, "# ") || strings.HasPrefix(line, "## ") {
			continue
		}
		module, found := parseVendoredModule(line[2:])
		if found {
			if _, duplicate := vendored[module.Path]; duplicate {
				return nil, fmt.Errorf("read release vendor module graph: duplicate module %s", module.Path)
			}
			vendored[module.Path] = module.Version
		}
	}
	if !equalVersions(requirements, vendored) {
		return nil, fmt.Errorf("read release module graph: go.mod and vendor/modules.txt select different modules")
	}
	modules := []listedModule{{Path: modulePath, Main: true}}
	for modulePath, moduleVersion := range vendored {
		modules = append(modules, listedModule{Path: modulePath, Version: moduleVersion})
	}
	slices.SortFunc(modules, func(left, right listedModule) int { return strings.Compare(left.Path, right.Path) })
	return modules, nil
}

func parseGoMod(data string) (string, map[string]string, error) {
	modulePath := ""
	requirements := make(map[string]string)
	inRequire := false
	for line := range strings.SplitSeq(data, "\n") {
		line = strings.TrimSpace(strings.SplitN(line, "//", 2)[0])
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "module" {
			modulePath = fields[1]
			continue
		}
		if line == "require (" {
			inRequire = true
			continue
		}
		if inRequire && line == ")" {
			inRequire = false
			continue
		}
		if len(fields) == 3 && fields[0] == "require" {
			requirements[fields[1]] = fields[2]
		} else if inRequire && len(fields) >= 2 {
			requirements[fields[0]] = fields[1]
		}
	}
	if modulePath == "" {
		return "", nil, fmt.Errorf("read release go.mod: module directive is missing")
	}
	return modulePath, requirements, nil
}

func equalVersions(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for path, version := range left {
		if right[path] != version {
			return false
		}
	}
	return true
}

func parseVendoredModule(line string) (listedModule, bool) {
	left, replacement, replaced := strings.Cut(line, " => ")
	fields := strings.Fields(left)
	if len(fields) < 2 || !strings.HasPrefix(fields[1], "v") {
		return listedModule{}, false
	}
	version := fields[1]
	if replaced {
		replacementFields := strings.Fields(replacement)
		if len(replacementFields) >= 2 && strings.HasPrefix(replacementFields[1], "v") {
			version = replacementFields[1]
		}
	}
	return listedModule{Path: fields[0], Version: version}, true
}

func packageSPDXID(name, version string) string {
	sum := sha256.Sum256([]byte(name + "@" + version))
	return "SPDXRef-Package-" + hex.EncodeToString(sum[:8])
}
