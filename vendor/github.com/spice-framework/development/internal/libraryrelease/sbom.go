package libraryrelease

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	artifactSchemaVersion = 1
	rendererIdentity      = "github.com/spice-framework/development/cmd/spice-dev library-release renderer/v1"
)

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
	Name             string            `json:"name"`
	SPDXID           string            `json:"SPDXID"`
	VersionInfo      string            `json:"versionInfo"`
	DownloadLocation string            `json:"downloadLocation"`
	FilesAnalyzed    bool              `json:"filesAnalyzed"`
	LicenseConcluded string            `json:"licenseConcluded"`
	LicenseDeclared  string            `json:"licenseDeclared"`
	CopyrightText    string            `json:"copyrightText"`
	ExternalRefs     []spdxExternalRef `json:"externalRefs,omitempty"`
}

type spdxExternalRef struct {
	ReferenceCategory string `json:"referenceCategory"`
	ReferenceType     string `json:"referenceType"`
	ReferenceLocator  string `json:"referenceLocator"`
}

type spdxRelationship struct {
	SPDXElementID      string `json:"spdxElementId"`
	RelationshipType   string `json:"relationshipType"`
	RelatedSPDXElement string `json:"relatedSpdxElement"`
}

func buildSBOM(
	ctx context.Context,
	plan Plan,
	files map[string][]byte,
) ([]byte, error) {
	modules, err := committedModules(ctx, plan, files)
	if err != nil {
		return nil, err
	}
	rootID := packageID(plan.Module, plan.Version)
	packages := []spdxPackage{newSPDXPackage(plan.Module, plan.Version, "")}
	relationships := []spdxRelationship{{
		SPDXElementID:      "SPDXRef-DOCUMENT",
		RelationshipType:   "DESCRIBES",
		RelatedSPDXElement: rootID,
	}}
	for _, module := range modules {
		item := newSPDXPackage(module.Path, module.Version, module.Replace)
		packages = append(packages, item)
		relationships = append(relationships, spdxRelationship{
			SPDXElementID:      rootID,
			RelationshipType:   "DEPENDS_ON",
			RelatedSPDXElement: item.SPDXID,
		})
	}
	var identity strings.Builder
	fmt.Fprintf(
		&identity,
		"plan=%d\nartifact=%d\nversion=%s\ncommit=%s",
		plan.Schema,
		artifactSchemaVersion,
		plan.Version,
		plan.Commit,
	)
	for _, item := range packages {
		fmt.Fprintf(&identity, "\n%s@%s", item.Name, item.VersionInfo)
	}
	namespaceHash := sha256.Sum256([]byte(identity.String()))
	document := spdxDocument{
		SPDXVersion: "SPDX-2.3",
		DataLicense: "CC0-1.0",
		SPDXID:      "SPDXRef-DOCUMENT",
		Name:        plan.Repository + " " + plan.Version,
		DocumentNamespace: strings.TrimSuffix(plan.Source, "/") + "/releases/" +
			plan.Version + "/spdx/v" + fmt.Sprint(artifactSchemaVersion) + "/" +
			hex.EncodeToString(namespaceHash[:]),
		CreationInfo: spdxCreationInfo{
			Created: time.Unix(plan.SourceDateEpoch, 0).UTC().Format(time.RFC3339),
			Creators: []string{
				"Organization: Spice Framework",
				"Tool: " + rendererIdentity,
			},
		},
		Packages:      packages,
		Relationships: relationships,
	}
	content, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode release SBOM: %w", err)
	}
	return append(content, '\n'), nil
}

func newSPDXPackage(name string, version string, replacement string) spdxPackage {
	item := spdxPackage{
		Name: name, SPDXID: packageID(name, version), VersionInfo: version,
		DownloadLocation: "NOASSERTION", FilesAnalyzed: false,
		LicenseConcluded: "NOASSERTION", LicenseDeclared: "NOASSERTION",
		CopyrightText: "NOASSERTION",
	}
	if replacement != "" {
		item.ExternalRefs = []spdxExternalRef{{
			ReferenceCategory: "OTHER",
			ReferenceType:     "spice:go-replace",
			ReferenceLocator:  replacement,
		}}
	}
	return item
}

func packageID(name string, version string) string {
	sum := sha256.Sum256([]byte(name + "@" + version))
	return "SPDXRef-Package-" + hex.EncodeToString(sum[:8])
}
