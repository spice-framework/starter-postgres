package libraryreleaseverify

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"
)

const rendererIdentity = "github.com/spice-framework/development/cmd/spice-dev library-release renderer/v1"

type sbomIdentity struct {
	repository string
	module     string
	source     string
	version    string
	commit     string
	epoch      time.Time
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

func verifySBOM(data []byte, identity sbomIdentity, modules []listedModule) error {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return fmt.Errorf("validate SPDX JSON object keys: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var actual spdxDocument
	if err := decoder.Decode(&actual); err != nil {
		return fmt.Errorf("decode SPDX document: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("SPDX document has trailing JSON content")
	}
	expected := expectedSBOM(identity, modules)
	if !reflect.DeepEqual(actual, expected) {
		return errors.New("SPDX document does not exactly match the trusted source module graph and renderer contract")
	}
	return nil
}

func expectedSBOM(identity sbomIdentity, modules []listedModule) spdxDocument {
	rootID := packageSPDXID(identity.module, identity.version)
	packages := []spdxPackage{newSPDXPackage(identity.module, identity.version, "")}
	relationships := []spdxRelationship{{
		SPDXElementID:      "SPDXRef-DOCUMENT",
		RelationshipType:   "DESCRIBES",
		RelatedSPDXElement: rootID,
	}}
	for _, module := range modules {
		item := newSPDXPackage(module.path, module.version, module.replacement)
		packages = append(packages, item)
		relationships = append(relationships, spdxRelationship{
			SPDXElementID:      rootID,
			RelationshipType:   "DEPENDS_ON",
			RelatedSPDXElement: item.SPDXID,
		})
	}
	var namespaceIdentity strings.Builder
	fmt.Fprintf(
		&namespaceIdentity,
		"plan=%d\nartifact=%d\nversion=%s\ncommit=%s",
		rendererV1PlanSchema,
		rendererV1ArtifactSchema,
		identity.version,
		identity.commit,
	)
	for _, item := range packages {
		fmt.Fprintf(&namespaceIdentity, "\n%s@%s", item.Name, item.VersionInfo)
	}
	namespaceHash := sha256.Sum256([]byte(namespaceIdentity.String()))
	return spdxDocument{
		SPDXVersion:       "SPDX-2.3",
		DataLicense:       "CC0-1.0",
		SPDXID:            "SPDXRef-DOCUMENT",
		Name:              identity.repository + " " + identity.version,
		DocumentNamespace: strings.TrimSuffix(identity.source, "/") + "/releases/" + identity.version + "/spdx/v1/" + hex.EncodeToString(namespaceHash[:]),
		CreationInfo: spdxCreationInfo{
			Created: identity.epoch.UTC().Format(time.RFC3339),
			Creators: []string{
				"Organization: Spice Framework",
				"Tool: " + rendererIdentity,
			},
		},
		Packages:      packages,
		Relationships: relationships,
	}
}

func newSPDXPackage(name, version, replacement string) spdxPackage {
	item := spdxPackage{
		Name: name, SPDXID: packageSPDXID(name, version), VersionInfo: version,
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

func packageSPDXID(name, version string) string {
	digest := sha256.Sum256([]byte(name + "@" + version))
	return "SPDXRef-Package-" + hex.EncodeToString(digest[:8])
}
