package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateReleaseToolAuthorization(t *testing.T) {
	t.Parallel()
	valid := fmt.Sprintf(`{"Require":[{"Path":%q,"Version":%q},{"Path":%q,"Version":%q}],"Tool":[{"Path":%q},{"Path":%q}]}`,
		developmentModule, developmentVersion, toolchainModule, toolchainVersion, developmentTool, toolchainTool)
	tests := []struct{ name, content, wantErr string }{
		{name: "exact authorization", content: valid},
		{name: "missing tool", content: `{"Require":[]}`, wantErr: "exactly one"},
		{name: "wrong renderer version", content: fmt.Sprintf(`{"Require":[{"Path":%q,"Version":"v0.0.0-wrong"}],"Tool":[{"Path":%q}]}`, developmentModule, developmentTool), wantErr: "require exactly " + developmentVersion},
		{name: "missing renderer requirement", content: fmt.Sprintf(`{"Require":[],"Tool":[{"Path":%q}]}`, developmentTool), wantErr: "must require " + developmentModule},
		{name: "missing verifier", content: fmt.Sprintf(`{"Require":[{"Path":%q,"Version":%q},{"Path":%q,"Version":%q}],"Tool":[{"Path":%q}]}`, developmentModule, developmentVersion, toolchainModule, toolchainVersion, developmentTool), wantErr: toolchainTool},
		{name: "malformed metadata", content: `{`, wantErr: "decode release tool authorization"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := validateReleaseToolAuthorization([]byte(test.content))
			if test.wantErr == "" && err != nil {
				t.Fatalf("validateReleaseToolAuthorization() error = %v", err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("validateReleaseToolAuthorization() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestDeterministicReleaseArtifacts(t *testing.T) {
	t.Parallel()
	first := t.TempDir()
	second := t.TempDir()
	writeTestFile(t, first, "artifact", "same")
	writeTestFile(t, second, "artifact", "same")
	if _, err := deterministicReleaseArtifacts([]string{first, second}); err != nil {
		t.Fatalf("deterministicReleaseArtifacts() error = %v", err)
	}
	writeTestFile(t, second, "artifact", "different")
	if _, err := deterministicReleaseArtifacts([]string{first, second}); err == nil || !strings.Contains(err.Error(), "different artifacts") {
		t.Fatalf("deterministicReleaseArtifacts() error = %v, want drift diagnostic", err)
	}
	if _, err := deterministicReleaseArtifacts([]string{first}); err == nil || !strings.Contains(err.Error(), "exactly two") {
		t.Fatalf("deterministicReleaseArtifacts(one output) error = %v", err)
	}
}

func TestValidateReleaseRehearsal(t *testing.T) {
	t.Parallel()
	root, artifacts := writeReleaseFixture(t)
	if err := validateReleaseRehearsal(root, artifacts); err != nil {
		t.Fatalf("validateReleaseRehearsal() error = %v", err)
	}

	t.Run("rejects signatures", func(t *testing.T) {
		t.Parallel()
		withSignature := make(map[string][sha256.Size]byte, len(artifacts)+1)
		maps.Copy(withSignature, artifacts)
		withSignature["checksums.txt.sig"] = sha256.Sum256([]byte("signature"))
		if err := validateReleaseRehearsal(root, withSignature); err == nil || !strings.Contains(err.Error(), "signatures are forbidden") {
			t.Fatalf("validateReleaseRehearsal() error = %v", err)
		}
	})

	t.Run("rejects checksum mismatch", func(t *testing.T) {
		t.Parallel()
		corrupt := t.TempDir()
		copyReleaseFixture(t, root, corrupt)
		writeTestFile(t, corrupt, "checksums.txt", strings.Repeat("0", sha256.Size*2)+"  "+releaseSBOMName()+"\n"+strings.Repeat("0", sha256.Size*2)+"  "+releaseArchiveName()+"\n")
		digests, err := treeDigests(corrupt)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateReleaseRehearsal(corrupt, digests); err == nil || !strings.Contains(err.Error(), "require canonical") {
			t.Fatalf("validateReleaseRehearsal() error = %v", err)
		}
	})

	t.Run("rejects provenance drift", func(t *testing.T) {
		t.Parallel()
		corrupt := t.TempDir()
		copyReleaseFixture(t, root, corrupt)
		content, err := os.ReadFile(filepath.Join(corrupt, releaseSBOMName()))
		if err != nil {
			t.Fatal(err)
		}
		content = []byte(strings.Replace(string(content), "Organization: Spice Framework", "Organization: Unknown", 1))
		writeTestBytes(t, corrupt, releaseSBOMName(), content)
		writeFixtureChecksums(t, corrupt)
		digests, err := treeDigests(corrupt)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateReleaseRehearsal(corrupt, digests); err == nil || !strings.Contains(err.Error(), "renderer/v1 contract") {
			t.Fatalf("validateReleaseRehearsal() error = %v", err)
		}
	})
}

func writeReleaseFixture(t *testing.T) (string, map[string][sha256.Size]byte) {
	t.Helper()
	root := t.TempDir()
	sbom := releaseSBOM{
		SPDXVersion: "SPDX-2.3", DataLicense: "CC0-1.0", SPDXID: "SPDXRef-DOCUMENT",
		Name:              "starter-postgres " + rehearsalVersion,
		DocumentNamespace: "https://github.com/spice-framework/starter-postgres/releases/" + rehearsalVersion + "/spdx/v1/" + strings.Repeat("a", sha256.Size*2),
		CreationInfo:      sbomCreationInfo{Created: "2026-01-01T00:00:00Z", Creators: []string{"Organization: Spice Framework", "Tool: github.com/spice-framework/development/cmd/spice-dev library-release renderer/v1"}},
		Packages:          []map[string]any{{"SPDXID": "SPDXRef-Package", "name": "starter-postgres"}},
		Relationships:     []map[string]any{{"spdxElementId": "SPDXRef-DOCUMENT", "relationshipType": "DESCRIBES", "relatedSpdxElement": "SPDXRef-Package"}},
	}
	content, err := json.Marshal(sbom)
	if err != nil {
		t.Fatal(err)
	}
	writeTestBytes(t, root, releaseSBOMName(), content)
	writeTestFile(t, root, releaseArchiveName(), "source")
	writeFixtureChecksums(t, root)
	digests, err := treeDigests(root)
	if err != nil {
		t.Fatal(err)
	}
	return root, digests
}

func writeFixtureChecksums(t *testing.T, root string) {
	t.Helper()
	sbom, err := os.ReadFile(filepath.Join(root, releaseSBOMName()))
	if err != nil {
		t.Fatal(err)
	}
	archive, err := os.ReadFile(filepath.Join(root, releaseArchiveName()))
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, root, "checksums.txt", fmt.Sprintf("%x  %s\n%x  %s\n", sha256.Sum256(sbom), releaseSBOMName(), sha256.Sum256(archive), releaseArchiveName()))
}

func copyReleaseFixture(t *testing.T, source, destination string) {
	t.Helper()
	for _, name := range []string{"checksums.txt", releaseSBOMName(), releaseArchiveName()} {
		content, err := os.ReadFile(filepath.Join(source, name))
		if err != nil {
			t.Fatal(err)
		}
		writeTestBytes(t, destination, name, content)
	}
}

func releaseArchiveName() string {
	return "starter-postgres_" + strings.TrimPrefix(rehearsalVersion, "v") + "_source.tar.gz"
}

func releaseSBOMName() string {
	return "starter-postgres_" + strings.TrimPrefix(rehearsalVersion, "v") + "_sbom.spdx.json"
}

func writeTestFile(t *testing.T, root, name, content string) {
	t.Helper()
	writeTestBytes(t, root, name, []byte(content))
}

func writeTestBytes(t *testing.T, root, name string, content []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), content, 0o600); err != nil {
		t.Fatalf("write fixture %s: %v", name, err)
	}
}
