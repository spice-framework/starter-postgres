package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateReleaseToolAuthorization(t *testing.T) {
	t.Parallel()
	valid := fmt.Sprintf(
		`{"Require":[{"Path":%q,"Version":%q},{"Path":%q,"Version":%q}],"Tool":[{"Path":%q},{"Path":%q}]}`,
		developmentModule,
		developmentVersion,
		toolchainModule,
		toolchainVersion,
		developmentTool,
		releaseVerifierTool,
	)
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{name: "exact authorization", content: valid},
		{
			name: "missing development tool",
			content: fmt.Sprintf(
				`{"Require":[{"Path":%q,"Version":%q},{"Path":%q,"Version":%q}],"Tool":[{"Path":%q}]}`,
				developmentModule, developmentVersion, toolchainModule, toolchainVersion, releaseVerifierTool,
			),
			wantErr: developmentTool,
		},
		{
			name: "missing verifier tool",
			content: fmt.Sprintf(
				`{"Require":[{"Path":%q,"Version":%q},{"Path":%q,"Version":%q}],"Tool":[{"Path":%q}]}`,
				developmentModule, developmentVersion, toolchainModule, toolchainVersion, developmentTool,
			),
			wantErr: releaseVerifierTool,
		},
		{
			name: "wrong development version",
			content: fmt.Sprintf(
				`{"Require":[{"Path":%q,"Version":"v0.0.0-wrong"},{"Path":%q,"Version":%q}],"Tool":[{"Path":%q},{"Path":%q}]}`,
				developmentModule, toolchainModule, toolchainVersion, developmentTool, releaseVerifierTool,
			),
			wantErr: "require exactly " + developmentVersion,
		},
		{
			name: "wrong verifier version",
			content: fmt.Sprintf(
				`{"Require":[{"Path":%q,"Version":%q},{"Path":%q,"Version":"v0.0.0-wrong"}],"Tool":[{"Path":%q},{"Path":%q}]}`,
				developmentModule, developmentVersion, toolchainModule, developmentTool, releaseVerifierTool,
			),
			wantErr: "require exactly " + toolchainVersion,
		},
		{
			name: "missing development requirement",
			content: fmt.Sprintf(
				`{"Require":[{"Path":%q,"Version":%q}],"Tool":[{"Path":%q},{"Path":%q}]}`,
				toolchainModule, toolchainVersion, developmentTool, releaseVerifierTool,
			),
			wantErr: "must require " + developmentModule,
		},
		{
			name: "missing verifier requirement",
			content: fmt.Sprintf(
				`{"Require":[{"Path":%q,"Version":%q}],"Tool":[{"Path":%q},{"Path":%q}]}`,
				developmentModule, developmentVersion, developmentTool, releaseVerifierTool,
			),
			wantErr: "must require " + toolchainModule,
		},
		{name: "malformed metadata", content: `{`, wantErr: "decode release tool authorization"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := validateReleaseToolAuthorization([]byte(test.content))
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("validateReleaseToolAuthorization() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("validateReleaseToolAuthorization() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestDeterministicReleaseArtifactsRejectsDrift(t *testing.T) {
	t.Parallel()
	first := t.TempDir()
	second := t.TempDir()
	writeParityTestFile(t, first, "artifact", []byte("first"))
	writeParityTestFile(t, second, "artifact", []byte("second"))
	_, err := deterministicReleaseArtifacts("fixture", []string{first, second})
	if err == nil || !strings.Contains(err.Error(), "different artifacts") {
		t.Fatalf("deterministicReleaseArtifacts() error = %v, want drift diagnostic", err)
	}
}

func TestValidateReleaseArchiveParity(t *testing.T) {
	t.Parallel()
	central := filepath.Join(t.TempDir(), "central.tar.gz")
	retained := filepath.Join(t.TempDir(), "retained.tar.gz")
	writeParityTestArchive(t, central, "starter-postgres_0.0.0-rehearsal/", "same", "")
	writeParityTestArchive(t, retained, "starter-postgres-0.0.0-rehearsal/", "same", "")
	if err := validateReleaseArchiveParity(
		central,
		"starter-postgres_0.0.0-rehearsal/",
		retained,
		"starter-postgres-0.0.0-rehearsal/",
	); err != nil {
		t.Fatalf("validateReleaseArchiveParity() error = %v", err)
	}

	t.Run("rejects entry drift", func(t *testing.T) {
		t.Parallel()
		drifted := filepath.Join(t.TempDir(), "drifted.tar.gz")
		writeParityTestArchive(t, drifted, "starter-postgres-0.0.0-rehearsal/", "different", "")
		err := validateReleaseArchiveParity(
			central,
			"starter-postgres_0.0.0-rehearsal/",
			drifted,
			"starter-postgres-0.0.0-rehearsal/",
		)
		if err == nil || !strings.Contains(err.Error(), "differ outside") {
			t.Fatalf("validateReleaseArchiveParity() error = %v, want content drift", err)
		}
	})

	t.Run("rejects unexpected retained root", func(t *testing.T) {
		t.Parallel()
		wrong := filepath.Join(t.TempDir(), "wrong.tar.gz")
		writeParityTestArchive(t, wrong, "starter-postgres_0.0.0-rehearsal/", "same", "")
		err := validateReleaseArchiveParity(
			central,
			"starter-postgres_0.0.0-rehearsal/",
			wrong,
			"starter-postgres-0.0.0-rehearsal/",
		)
		if err == nil || !strings.Contains(err.Error(), "outside required root") {
			t.Fatalf("validateReleaseArchiveParity() error = %v, want root diagnostic", err)
		}
	})

	t.Run("rejects hidden decompressed tail", func(t *testing.T) {
		t.Parallel()
		wrong := filepath.Join(t.TempDir(), "hidden-tail.tar.gz")
		prefix := "starter-postgres_0.0.0-rehearsal/"
		writeParityTestArchive(t, wrong, prefix, "same", "hidden")
		if _, err := readParityArchive(wrong, prefix); err == nil ||
			!strings.Contains(err.Error(), "hidden trailing bytes") {
			t.Fatalf("readParityArchive(hidden tail) error = %v", err)
		}
	})
}

func TestValidateReleaseSBOMParity(t *testing.T) {
	t.Parallel()
	central, retained := paritySBOMFixtures()
	tests := []struct {
		name    string
		mutate  func(*releaseSBOM, *releaseSBOM)
		wantErr string
	}{
		{name: "documented R1 differences"},
		{
			name: "package drift",
			mutate: func(_ *releaseSBOM, retained *releaseSBOM) {
				retained.Packages[0].VersionInfo = "v9.9.9"
			},
			wantErr: "outside documented R1",
		},
		{
			name: "wrong PostgreSQL provenance",
			mutate: func(_ *releaseSBOM, retained *releaseSBOM) {
				retained.CreationInfo.Creators[0] = "Organization: Spice Authors"
			},
			wantErr: "PostgreSQL builder",
		},
		{
			name: "retained describes",
			mutate: func(_ *releaseSBOM, retained *releaseSBOM) {
				retained.Relationships = append(retained.Relationships, sbomRelationship{
					SPDXElementID: "SPDXRef-DOCUMENT", RelationshipType: "DESCRIBES",
					RelatedSPDXElement: "SPDXRef-Package-root",
				})
			},
			wantErr: "must not contain DESCRIBES",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			centralCopy := cloneParitySBOM(t, central)
			retainedCopy := cloneParitySBOM(t, retained)
			if test.mutate != nil {
				test.mutate(&centralCopy, &retainedCopy)
			}
			err := validateReleaseSBOMParity(
				marshalParitySBOM(t, centralCopy),
				marshalParitySBOM(t, retainedCopy),
			)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("validateReleaseSBOMParity() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("validateReleaseSBOMParity() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestValidateReleaseParityRejectsSignaturesAndBadChecksums(t *testing.T) {
	t.Parallel()
	centralRoot, central := writeParityTestRelease(t, true)
	retainedRoot, retained := writeParityTestRelease(t, false)
	if err := validateReleaseParity(centralRoot, central, retainedRoot, retained); err != nil {
		t.Fatalf("validateReleaseParity() error = %v", err)
	}

	t.Run("signature", func(t *testing.T) {
		t.Parallel()
		signed := maps.Clone(central)
		signed["checksums.txt.sig"] = sha256.Sum256([]byte("signature"))
		err := validateReleaseParity(centralRoot, signed, retainedRoot, retained)
		if err == nil || !strings.Contains(err.Error(), "signatures are forbidden") {
			t.Fatalf("validateReleaseParity() error = %v, want signature diagnostic", err)
		}
	})

	t.Run("checksum", func(t *testing.T) {
		t.Parallel()
		corruptRoot, corrupt := writeParityTestRelease(t, true)
		writeParityTestFile(t, corruptRoot, "checksums.txt", []byte("invalid\n"))
		corrupt["checksums.txt"] = sha256.Sum256([]byte("invalid\n"))
		err := validateReleaseParity(corruptRoot, corrupt, retainedRoot, retained)
		if err == nil || !strings.Contains(err.Error(), "checksums.txt") {
			t.Fatalf("validateReleaseParity() error = %v, want checksum diagnostic", err)
		}
	})
}

func paritySBOMFixtures() (releaseSBOM, releaseSBOM) {
	root := sbomPackage{
		Name: modulePath, SPDXID: "SPDXRef-Package-root", VersionInfo: rehearsalVersion,
		DownloadLocation: "NOASSERTION", LicenseConcluded: "NOASSERTION",
		LicenseDeclared: "NOASSERTION", CopyrightText: "NOASSERTION",
	}
	dependency := sbomPackage{
		Name: "example.com/dependency", SPDXID: "SPDXRef-Package-dependency", VersionInfo: "v1.2.3",
		DownloadLocation: "NOASSERTION", LicenseConcluded: "NOASSERTION",
		LicenseDeclared: "NOASSERTION", CopyrightText: "NOASSERTION",
	}
	dependsOn := sbomRelationship{
		SPDXElementID: root.SPDXID, RelationshipType: "DEPENDS_ON",
		RelatedSPDXElement: dependency.SPDXID,
	}
	common := releaseSBOM{
		SPDXVersion: "SPDX-2.3", DataLicense: "CC0-1.0", SPDXID: "SPDXRef-DOCUMENT",
		CreationInfo: sbomCreationInfo{Created: "2026-01-01T00:00:00Z"},
	}
	central := common
	central.Name = "starter-postgres " + rehearsalVersion
	central.DocumentNamespace = parityNamespace("v1/", 'a')
	central.CreationInfo.Creators = []string{
		"Organization: Spice Framework",
		"Tool: github.com/spice-framework/development/cmd/spice-dev library-release renderer/v1",
	}
	central.Packages = []sbomPackage{root, dependency}
	central.Relationships = []sbomRelationship{{
		SPDXElementID: "SPDXRef-DOCUMENT", RelationshipType: "DESCRIBES",
		RelatedSPDXElement: root.SPDXID,
	}, dependsOn}
	retained := common
	retained.Name = "Spice PostgreSQL Starter " + rehearsalVersion
	retained.DocumentNamespace = parityNamespace("", 'b')
	retained.CreationInfo.Creators = []string{
		"Organization: Spice Framework",
		"Tool: github.com/spice-framework/starter-postgres/cmd/starter-postgres-release",
	}
	retained.Packages = []sbomPackage{dependency, root}
	retained.Relationships = []sbomRelationship{dependsOn}
	return central, retained
}

func writeParityTestRelease(
	t *testing.T,
	central bool,
) (string, map[string][sha256.Size]byte) {
	t.Helper()
	root := t.TempDir()
	prefix := "starter-postgres-0.0.0-rehearsal/"
	sbomCentral, sbomRetained := paritySBOMFixtures()
	sbom := sbomRetained
	if central {
		prefix = "starter-postgres_0.0.0-rehearsal/"
		sbom = sbomCentral
	}
	archiveName := releaseArchiveName()
	sbomName := releaseSBOMName()
	writeParityTestArchive(t, filepath.Join(root, archiveName), prefix, "same", "")
	writeParityTestFile(t, root, sbomName, marshalParitySBOM(t, sbom))
	archive, err := os.ReadFile(filepath.Join(root, archiveName))
	if err != nil {
		t.Fatal(err)
	}
	sbomContent, err := os.ReadFile(filepath.Join(root, sbomName))
	if err != nil {
		t.Fatal(err)
	}
	checksums := fmt.Sprintf(
		"%x  %s\n%x  %s\n",
		sha256.Sum256(sbomContent),
		sbomName,
		sha256.Sum256(archive),
		archiveName,
	)
	writeParityTestFile(t, root, "checksums.txt", []byte(checksums))
	digests, err := treeDigests(root)
	if err != nil {
		t.Fatal(err)
	}
	return root, digests
}

func writeParityTestArchive(
	t *testing.T,
	filename string,
	prefix string,
	content string,
	hiddenTail string,
) {
	t.Helper()
	file, err := os.Create(filename) // #nosec G304 -- test path is inside t.TempDir.
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter, err := gzip.NewWriterLevel(file, gzip.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	epoch := time.Unix(1_700_000_000, 0).UTC()
	gzipWriter.ModTime = epoch
	gzipWriter.OS = 255
	tarWriter := tar.NewWriter(gzipWriter)
	entryName := strings.Repeat("nested/", 16) + "README.md"
	header := tar.Header{
		Name: prefix + entryName, Mode: 0o644, Size: int64(len(content)),
		ModTime: epoch, AccessTime: epoch, ChangeTime: epoch, Format: tar.FormatPAX,
	}
	if err := tarWriter.WriteHeader(&header); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := gzipWriter.Write([]byte(hiddenTail)); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func releaseArchiveName() string {
	return "starter-postgres_" + strings.TrimPrefix(rehearsalVersion, "v") + "_source.tar.gz"
}

func releaseSBOMName() string {
	return "starter-postgres_" + strings.TrimPrefix(rehearsalVersion, "v") + "_sbom.spdx.json"
}

func parityNamespace(extra string, digit rune) string {
	return "https://github.com/spice-framework/starter-postgres/releases/" + rehearsalVersion +
		"/spdx/" + extra + strings.Repeat(string(digit), sha256.Size*2)
}

func cloneParitySBOM(t *testing.T, value releaseSBOM) releaseSBOM {
	t.Helper()
	content := marshalParitySBOM(t, value)
	cloned, err := decodeReleaseSBOM(content)
	if err != nil {
		t.Fatal(err)
	}
	return cloned
}

func marshalParitySBOM(t *testing.T, value releaseSBOM) []byte {
	t.Helper()
	content, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func writeParityTestFile(t *testing.T, root string, name string, content []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), content, 0o600); err != nil {
		t.Fatal(err)
	}
}
