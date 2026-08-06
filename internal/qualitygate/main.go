// Command qualitygate runs starter-postgres's repository-owned checks.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	requiredGoVersion = "go1.26.5"
	modulePath        = "github.com/spice-framework/starter-postgres"
	minimumCoverage   = 85.0
)

var output = log.New(os.Stdout, "", 0)

func main() {
	os.Exit(execute()) // Entrypoint exception: propagate verification failure.
}

func execute() int {
	mode := flag.String("mode", "verify", "verification mode: check, fmt, release-parity, verify, or verify-release")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	root, err := repositoryRoot()
	if err == nil {
		err = run(ctx, root, *mode)
	}
	if err != nil {
		output.Printf("quality gate failed: %v", err)
		return 1
	}
	return 0
}

type step struct {
	name string
	run  func() error
}

func run(ctx context.Context, root, mode string) error {
	if runtime.Version() != requiredGoVersion {
		return fmt.Errorf("go version is %s; require exactly %s", runtime.Version(), requiredGoVersion)
	}
	identity := step{"repository identity", func() error { return checkIdentity(ctx, root) }}
	dependencies := step{"dependency preparation", func() error { return prepareDependencies(ctx, root) }}
	formatting := step{"formatting", func() error { return format(ctx, root, false) }}
	modules := step{"module and vendor", func() error { return checkModule(ctx, root) }}
	vet := step{"go vet", func() error { return command(ctx, root, nil, "go", "vet", "./...") }}
	release := step{"central and retained release parity", func() error {
		return releaseParity(ctx, root)
	}}
	var steps []step
	switch mode {
	case "check":
		steps = []step{identity, dependencies, formatting, modules, vet}
	case "fmt":
		steps = []step{{"formatting", func() error { return format(ctx, root, true) }}}
	case "release-parity":
		steps = []step{identity, release}
	case "verify":
		steps = []step{
			identity,
			dependencies,
			formatting,
			modules,
			vet,
			{"lint and nil safety", func() error { return lint(ctx, root) }},
			{"security", func() error { return security(ctx, root) }},
			{"shuffled race tests and coverage", func() error { return testsAndCoverage(ctx, root) }},
			{"offline vendor", func() error { return offline(ctx, root) }},
		}
	case "verify-release":
		steps = []step{identity, release}
	default:
		return fmt.Errorf("unknown mode %q", mode)
	}
	for _, current := range steps {
		started := time.Now()
		output.Printf("==> %s", current.name)
		if err := current.run(); err != nil {
			return fmt.Errorf("%s (%s): %w", current.name, time.Since(started).Round(time.Millisecond), err)
		}
		output.Printf("<== %s passed in %s", current.name, time.Since(started).Round(time.Millisecond))
	}
	output.Print("==> all verification passed")
	return nil
}

func prepareDependencies(ctx context.Context, root string) error {
	if err := networkCommand(ctx, root, "mod", "download"); err != nil {
		return err
	}
	// The product tidy graph includes test-only dependencies of pgx packages.
	// Fetch and validate that graph only in this explicit network-capable phase;
	// checkModule repeats the same read-only assertion with GOPROXY disabled.
	if err := networkCommand(ctx, root, "mod", "tidy", "-diff"); err != nil {
		return err
	}
	if err := networkCommand(ctx, root, "-C", "tools", "mod", "download"); err != nil {
		return err
	}
	return networkCommand(ctx, root, "-C", "tools", "mod", "tidy", "-diff")
}

func checkIdentity(ctx context.Context, root string) error {
	content, err := os.ReadFile(filepath.Join(root, "go.mod")) // #nosec G304 -- root is repository-owned.
	if err != nil {
		return fmt.Errorf("read go.mod: %w", err)
	}
	if !strings.Contains(string(content), "module "+modulePath+"\n") {
		return fmt.Errorf("go.mod does not declare canonical module %s", modulePath)
	}
	if bytes.Contains(content, []byte("\nreplace ")) || bytes.Contains(content, []byte("\nreplace (")) {
		return errors.New("committed go.mod must not contain replace directives")
	}
	if err := requireReleaseTool(ctx, root); err != nil {
		return err
	}
	return checkReleaseWorkflow(root)
}

func format(ctx context.Context, root string, write bool) error {
	files, err := goFiles(root)
	if err != nil {
		return err
	}
	for _, name := range []string{"goimports", "gofumpt"} {
		executable, pathErr := toolPath(ctx, root, name)
		if pathErr != nil {
			return pathErr
		}
		option := "-l"
		if write {
			option = "-w"
		}
		stdout, runErr := capture(ctx, root, nil, executable, append([]string{option}, files...)...)
		if runErr != nil {
			return runErr
		}
		if !write && strings.TrimSpace(stdout) != "" {
			return fmt.Errorf("%s requires formatting: %s", name, strings.Join(strings.Fields(stdout), ", "))
		}
	}
	return nil
}

func goFiles(root string) ([]string, error) {
	var result []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && path != root && slices.Contains([]string{".git", "tools", "vendor"}, entry.Name()) {
			return filepath.SkipDir
		}
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".go" {
			result = append(result, path)
		}
		return nil
	})
	slices.Sort(result)
	return result, err
}

func checkModule(ctx context.Context, root string) error {
	if err := command(ctx, root, nil, "go", "mod", "tidy", "-diff"); err != nil {
		return err
	}
	temporary, err := os.MkdirTemp("", "spice-starter-postgres-vendor-")
	if err != nil {
		return fmt.Errorf("create vendor comparison directory: %w", err)
	}
	defer removeTree(temporary)
	candidate := filepath.Join(temporary, "vendor")
	if vendorErr := command(ctx, root, nil, "go", "mod", "vendor", "-o", candidate); vendorErr != nil {
		return vendorErr
	}
	current, err := treeDigests(filepath.Join(root, "vendor"))
	if err != nil {
		return err
	}
	expected, err := treeDigests(candidate)
	if err != nil {
		return err
	}
	if !maps.Equal(current, expected) {
		return errors.New("vendor differs from a fresh go mod vendor result")
	}
	return nil
}

func removeTree(path string) {
	if err := os.RemoveAll(path); err != nil {
		output.Printf("warning: remove temporary tree %q: %v", path, err)
	}
}

func treeDigests(root string) (map[string][sha256.Size]byte, error) {
	opened, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("open vendor root: %w", err)
	}
	defer func() {
		if closeErr := opened.Close(); closeErr != nil {
			output.Printf("warning: close vendor root %q: %v", root, closeErr)
		}
	}()
	result := make(map[string][sha256.Size]byte)
	err = fs.WalkDir(opened.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		content, readErr := opened.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		result[filepath.ToSlash(path)] = sha256.Sum256(content)
		return nil
	})
	return result, err
}

func lint(ctx context.Context, root string) error {
	golangci, err := toolPath(ctx, root, "golangci-lint")
	if err != nil {
		return err
	}
	if lintErr := command(ctx, root, nil, golangci, "run", "--timeout=10m"); lintErr != nil {
		return lintErr
	}
	nilaway, err := toolPath(ctx, root, "nilaway")
	if err != nil {
		return err
	}
	return command(ctx, root, nil, nilaway, "-include-pkgs="+modulePath, "./...")
}

func security(ctx context.Context, root string) error {
	gosec, err := toolPath(ctx, root, "gosec")
	if err != nil {
		return err
	}
	if securityErr := command(ctx, root, nil, gosec, "-quiet", "-exclude-generated", "./..."); securityErr != nil {
		return securityErr
	}
	govulncheck, err := toolPath(ctx, root, "govulncheck")
	if err != nil {
		return err
	}
	return command(ctx, root, nil, govulncheck, "./...")
}

func testsAndCoverage(ctx context.Context, root string) (returnErr error) {
	profile, err := os.CreateTemp("", "spice-starter-postgres-coverage-*.out")
	if err != nil {
		return fmt.Errorf("create coverage profile: %w", err)
	}
	path := profile.Name()
	if closeErr := profile.Close(); closeErr != nil {
		return fmt.Errorf("close coverage profile: %w", closeErr)
	}
	defer func() { returnErr = errors.Join(returnErr, os.Remove(path)) }()
	if testErr := command(ctx, root, nil, "go", "test", "-race", "-shuffle=on", "-count=1", "-covermode=atomic", "-coverprofile="+path, "."); testErr != nil {
		return testErr
	}
	if gateErr := command(ctx, root, nil, "go", "test", "-race", "-shuffle=on", "-count=1", "./internal/qualitygate"); gateErr != nil {
		return gateErr
	}
	stdout, err := capture(ctx, root, nil, "go", "tool", "cover", "-func="+path)
	if err != nil {
		return err
	}
	percentage, err := totalCoverage(stdout)
	if err != nil {
		return err
	}
	output.Printf("product coverage %.1f%% (minimum %.1f%%)", percentage, minimumCoverage)
	if percentage < minimumCoverage {
		return fmt.Errorf("product coverage %.1f%% is below %.1f%%", percentage, minimumCoverage)
	}
	return nil
}

func totalCoverage(report string) (float64, error) {
	lines := strings.Split(strings.TrimSpace(report), "\n")
	if len(lines) == 0 {
		return 0, errors.New("coverage report is empty")
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) == 0 || !strings.HasSuffix(fields[len(fields)-1], "%") {
		return 0, errors.New("coverage report has no total percentage")
	}
	value := strings.TrimSuffix(fields[len(fields)-1], "%")
	percentage, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("parse coverage percentage %q: %w", value, err)
	}
	return percentage, nil
}

func offline(ctx context.Context, root string) error {
	environment := map[string]string{"GOFLAGS": "-mod=vendor"}
	if err := command(ctx, root, environment, "go", "test", "-count=1", "./..."); err != nil {
		return err
	}
	return command(ctx, root, environment, "go", "build", "-trimpath", "./...")
}

func toolPath(ctx context.Context, root, name string) (string, error) {
	stdout, err := capture(ctx, root, nil, "go", "tool", "-C", "tools", "-n", name)
	if err != nil {
		return "", err
	}
	path := strings.TrimSpace(stdout)
	if path == "" {
		return "", fmt.Errorf("resolve tool %q: empty path", name)
	}
	return path, nil
}

func repositoryRoot() (string, error) {
	current, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	for {
		content, readErr := os.ReadFile(filepath.Join(current, "go.mod")) // #nosec G304 -- candidates are bounded ancestors.
		if readErr == nil && bytes.Contains(content, []byte("module "+modulePath)) {
			return current, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", errors.New("find starter-postgres repository root: go.mod not found")
		}
		current = parent
	}
}

func command(ctx context.Context, directory string, environment map[string]string, executable string, arguments ...string) error {
	// #nosec G204,G702 -- executable and arguments are fixed repository-owned values.
	cmd := exec.CommandContext(ctx, executable, arguments...)
	cmd.Dir = directory
	cmd.Env = mergedEnvironment(environment, false)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", executable, strings.Join(arguments, " "), err)
	}
	return nil
}

func networkCommand(ctx context.Context, directory string, arguments ...string) error {
	// #nosec G204,G702 -- executable and arguments are fixed repository-owned values.
	cmd := exec.CommandContext(ctx, "go", arguments...)
	cmd.Dir = directory
	cmd.Env = mergedEnvironment(nil, true)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go %s: %w", strings.Join(arguments, " "), err)
	}
	return nil
}

func capture(ctx context.Context, directory string, environment map[string]string, executable string, arguments ...string) (string, error) {
	// #nosec G204,G702 -- executable and arguments are fixed repository-owned values.
	cmd := exec.CommandContext(ctx, executable, arguments...)
	cmd.Dir = directory
	cmd.Env = mergedEnvironment(environment, false)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w\n%s", executable, strings.Join(arguments, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func mergedEnvironment(overrides map[string]string, online bool) []string {
	values := map[string]string{"GOWORK": "off", "GOFLAGS": "", "GOTOOLCHAIN": "local"}
	if !online {
		values["GOPROXY"] = "off"
	}
	maps.Copy(values, overrides)
	result := make([]string, 0, len(os.Environ())+len(values))
	for _, entry := range os.Environ() {
		key, _, found := strings.Cut(entry, "=")
		if found {
			if _, replaced := values[strings.ToUpper(key)]; !replaced {
				result = append(result, entry)
			}
		}
	}
	for key, value := range values {
		result = append(result, key+"="+value)
	}
	slices.Sort(result)
	return result
}
