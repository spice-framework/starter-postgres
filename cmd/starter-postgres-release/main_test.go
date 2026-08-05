package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunBuildsExplicitUnsignedRehearsal(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "release")
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{
		"-rehearsal", "-root", root, "-output", output,
		"-version", "v0.1.0-rc.1", "-source-date-epoch", "1788000000",
	}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "created 3 artifact(s)") || stderr.Len() != 0 {
		t.Fatalf("run() code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	for _, name := range []string{"checksums.txt.sig", "checksums.txt.pem"} {
		if _, err := os.Stat(filepath.Join(output, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unsigned rehearsal unexpectedly created %s", name)
		}
	}
}

func TestRunRejectsInvalidInvocationBeforeBuilding(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		arguments []string
		want      string
	}{
		{name: "missing version", want: "-version is required"},
		{name: "positional argument", arguments: []string{"unexpected"}, want: "unexpected arguments"},
		{name: "signed rehearsal", arguments: []string{"-rehearsal", "-version=v0.1.0", "-signing-key=key"}, want: "cannot use -signing-key"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			if code := run(t.Context(), test.arguments, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), test.want) || stdout.Len() != 0 {
				t.Fatalf("run() code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestValidateReleaseCheckoutRequiresCleanExactTag(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init")
	runGit(t, root, "config", "user.name", "Spice Test")
	runGit(t, root, "config", "user.email", "spice@example.test")
	filename := filepath.Join(root, "tracked.txt")
	if err := os.WriteFile(filename, []byte("stable\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "tracked.txt")
	runGit(t, root, "commit", "-m", "fixture")
	if err := validateReleaseCheckout(t.Context(), root, "v0.1.0"); err == nil {
		t.Fatal("untagged checkout was accepted")
	}
	runGit(t, root, "tag", "v0.1.0")
	if err := validateReleaseCheckout(t.Context(), root, "v0.1.0"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "untracked.txt"), []byte("dirty"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateReleaseCheckout(t.Context(), root, "v0.1.0"); err == nil {
		t.Fatal("untracked dirty checkout was accepted")
	}
}

func TestSourceEpochUsesExplicitEnvironmentAndGit(t *testing.T) {
	got, err := sourceEpoch(t.Context(), t.TempDir(), 1_788_000_000)
	if err != nil || !got.Equal(time.Unix(1_788_000_000, 0).UTC()) {
		t.Fatalf("sourceEpoch(explicit) = %s, %v", got, err)
	}
	t.Setenv("SOURCE_DATE_EPOCH", "1788000001")
	got, err = sourceEpoch(t.Context(), t.TempDir(), 0)
	if err != nil || got.Unix() != 1_788_000_001 {
		t.Fatalf("sourceEpoch(environment) = %s, %v", got, err)
	}
	t.Setenv("SOURCE_DATE_EPOCH", "invalid")
	if _, parseErr := sourceEpoch(t.Context(), t.TempDir(), 0); parseErr == nil {
		t.Fatal("invalid SOURCE_DATE_EPOCH was accepted")
	}
	t.Setenv("SOURCE_DATE_EPOCH", "")
	root := t.TempDir()
	runGit(t, root, "init")
	runGit(t, root, "config", "user.name", "Spice Test")
	runGit(t, root, "config", "user.email", "spice@example.test")
	if writeErr := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("stable\n"), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	runGit(t, root, "add", "tracked.txt")
	runGit(t, root, "commit", "-m", "fixture")
	if got, err = sourceEpoch(t.Context(), root, 0); err != nil || got.IsZero() {
		t.Fatalf("sourceEpoch(git) = %s, %v", got, err)
	}
}

func TestReadSigningKeyAndWriterBoundaries(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	valid := filepath.Join(root, "valid.key")
	if err := os.WriteFile(valid, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, err := readSigningKey(valid); err != nil || string(data) != "key" {
		t.Fatalf("readSigningKey() = %q, %v", data, err)
	}
	oversized := filepath.Join(root, "oversized.key")
	if err := os.WriteFile(oversized, make([]byte, (1<<20)+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSigningKey(oversized); err == nil {
		t.Fatal("oversized signing key was accepted")
	}
	if code := writeExit(errorWriter{}, 2, "failure\n"); code != 1 {
		t.Fatalf("writeExit() = %d", code)
	}
}

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func runGit(t *testing.T, root string, arguments ...string) {
	t.Helper()
	command := exec.CommandContext(context.Background(), "git", arguments...)
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
}
