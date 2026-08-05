package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirectRequirement(t *testing.T) {
	t.Parallel()
	content := `{
		"Require": [
			{"Path": "example.com/indirect", "Version": "v1.0.0", "Indirect": true},
			{"Path": "github.com/spice-framework/spice", "Version": "v0.2.3"}
		]
	}`
	version, err := directRequirement(content, coreModulePath)
	if err != nil {
		t.Fatalf("directRequirement() error = %v", err)
	}
	if version != "v0.2.3" {
		t.Fatalf("directRequirement() = %q, want v0.2.3", version)
	}
}

func TestDirectRequirementRejectsMissingAndIndirect(t *testing.T) {
	t.Parallel()
	for _, content := range []string{
		`{"Require": []}`,
		`{"Require": [{"Path": "github.com/spice-framework/spice", "Version": "v0.2.3", "Indirect": true}]}`,
	} {
		if _, err := directRequirement(content, coreModulePath); err == nil {
			t.Fatalf("directRequirement(%s) succeeded, want error", content)
		}
	}
}

func TestDecodeCompatibility(t *testing.T) {
	t.Parallel()
	contract, err := decodeCompatibility([]byte(`{
		"schema": 1,
		"minimum": "v0.1.0",
		"current": "v0.2.0"
	}`))
	if err != nil {
		t.Fatalf("decodeCompatibility() error = %v", err)
	}
	if contract.Schema != 1 || contract.Minimum != "v0.1.0" || contract.Current != "v0.2.0" {
		t.Fatalf("decodeCompatibility() = %#v", contract)
	}
}

func TestDecodeCompatibilityRejectsInvalidContracts(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"missing schema":  `{"minimum":"v0.1.0","current":"v0.2.0"}`,
		"wrong schema":    `{"schema":2,"minimum":"v0.1.0","current":"v0.2.0"}`,
		"missing minimum": `{"schema":1,"current":"v0.2.0"}`,
		"missing current": `{"schema":1,"minimum":"v0.1.0"}`,
		"equal versions":  `{"schema":1,"minimum":"v0.1.0","current":"v0.1.0"}`,
		"unknown field":   `{"schema":1,"minimum":"v0.1.0","current":"v0.2.0","future":true}`,
		"trailing value":  `{"schema":1,"minimum":"v0.1.0","current":"v0.2.0"} {}`,
		"trailing syntax": `{"schema":1,"minimum":"v0.1.0","current":"v0.2.0"} !`,
	}
	for name, content := range tests {
		if _, err := decodeCompatibility([]byte(content)); err == nil {
			t.Errorf("%s: decodeCompatibility() succeeded, want error", name)
		}
	}
}

func TestEnsureMinimumRequiresDirectRequirementMatch(t *testing.T) {
	t.Parallel()
	contract := compatibility{Schema: 1, Minimum: "v0.1.0", Current: "v0.2.0"}
	if err := ensureMinimum(contract, "v0.1.0"); err != nil {
		t.Fatalf("ensureMinimum() error = %v", err)
	}
	if err := ensureMinimum(contract, "v0.1.1"); err == nil {
		t.Fatal("ensureMinimum() succeeded for mismatched direct requirement")
	}
}

func TestAlternateModfileIsIsolated(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	want := "module example.com/test\n"
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := alternateModfile(root)
	if err != nil {
		t.Fatalf("alternateModfile() error = %v", err)
	}
	t.Cleanup(func() {
		if cleanupErr := removeIfPresent(path); cleanupErr != nil {
			t.Errorf("cleanup alternate modfile: %v", cleanupErr)
		}
	})
	if filepath.Dir(path) != root || !strings.HasSuffix(path, ".mod") {
		t.Fatalf("alternateModfile() = %q, want sibling .mod", path)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != want {
		t.Fatalf("alternate content = %q, want %q", content, want)
	}
}

func TestRunRejectsUnknownLineBeforeNetworkAccess(t *testing.T) {
	t.Parallel()
	if err := run(context.Background(), "preview"); err == nil || !strings.Contains(err.Error(), "require minimum or current") {
		t.Fatalf("run() error = %v, want line validation", err)
	}
}
