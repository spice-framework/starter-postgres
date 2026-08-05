// Command corecompat verifies the PostgreSQL starter against an explicit
// Spice core compatibility line without changing the repository module graph.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

const (
	coreModulePath   = "github.com/spice-framework/spice"
	repositoryModule = "github.com/spice-framework/starter-postgres"
	requiredGo       = "go1.26.5"
)

type compatibility struct {
	Schema  int    `json:"schema"`
	Minimum string `json:"minimum"`
	Current string `json:"current"`
}

var output = log.New(os.Stdout, "", 0)

func main() {
	os.Exit(execute(os.Args[1:])) // Entrypoint exception: propagate compatibility failure.
}

func execute(arguments []string) int {
	flags := flag.NewFlagSet("corecompat", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	line := flags.String("line", "", "Spice core line: minimum or current")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		output.Printf("compatibility failed: unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := run(ctx, *line); err != nil {
		output.Printf("compatibility failed: %v", err)
		return 1
	}
	return 0
}

func run(ctx context.Context, line string) (returnErr error) {
	if runtime.Version() != requiredGo {
		return fmt.Errorf("go version is %s; require exactly %s", runtime.Version(), requiredGo)
	}
	if line != "minimum" && line != "current" {
		return fmt.Errorf("line %q is invalid; require minimum or current", line)
	}
	root, err := repositoryRoot()
	if err != nil {
		return err
	}
	version, err := compatibilityVersion(ctx, root, line)
	if err != nil {
		return err
	}
	modfile, err := alternateModfile(root)
	if err != nil {
		return err
	}
	sumfile := strings.TrimSuffix(modfile, ".mod") + ".sum"
	defer func() {
		returnErr = errors.Join(returnErr, removeIfPresent(modfile), removeIfPresent(sumfile))
	}()

	output.Printf("==> Spice core %s line resolved to %s", line, version)
	if editErr := goCommand(ctx, root, "mod", "edit", "-modfile="+modfile, "-require="+coreModulePath+"@"+version); editErr != nil {
		return editErr
	}
	if tidyErr := goCommand(ctx, root, "mod", "tidy", "-modfile="+modfile); tidyErr != nil {
		return tidyErr
	}
	selected, err := captureGo(ctx, root, "list", "-mod=mod", "-modfile="+modfile, "-m", "-f={{.Version}}", coreModulePath)
	if err != nil {
		return fmt.Errorf("verify selected Spice core: %w", err)
	}
	if strings.TrimSpace(selected) != version {
		return fmt.Errorf("selected Spice core is %q; require exactly %q", strings.TrimSpace(selected), version)
	}
	if vetErr := goCommand(ctx, root, "vet", "-mod=mod", "-modfile="+modfile, "./..."); vetErr != nil {
		return vetErr
	}
	if testErr := goCommand(ctx, root, "test", "-mod=mod", "-modfile="+modfile, "-race", "-shuffle=on", "-count=1", "./..."); testErr != nil {
		return testErr
	}
	output.Printf("<== Spice core %s compatibility passed at %s", line, version)
	return nil
}

func compatibilityVersion(ctx context.Context, root, line string) (string, error) {
	contract, err := readCompatibility(root)
	if err != nil {
		return "", err
	}
	minimum, err := minimumRequirement(ctx, root)
	if err != nil {
		return "", err
	}
	if minimumErr := ensureMinimum(contract, minimum); minimumErr != nil {
		return "", minimumErr
	}
	if line == "current" {
		return contract.Current, nil
	}
	return contract.Minimum, nil
}

func minimumRequirement(ctx context.Context, root string) (string, error) {
	stdout, err := captureGo(ctx, root, "mod", "edit", "-json")
	if err != nil {
		return "", fmt.Errorf("read minimum Spice core: %w", err)
	}
	return directRequirement(stdout, coreModulePath)
}

func readCompatibility(root string) (compatibility, error) {
	content, err := os.ReadFile(filepath.Join(root, "spice-compatibility.json")) // #nosec G304 -- root is repository-owned.
	if err != nil {
		return compatibility{}, fmt.Errorf("read spice-compatibility.json: %w", err)
	}
	return decodeCompatibility(content)
}

func decodeCompatibility(content []byte) (compatibility, error) {
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var contract compatibility
	if err := decoder.Decode(&contract); err != nil {
		return compatibility{}, fmt.Errorf("decode spice-compatibility.json: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return compatibility{}, errors.New("decode spice-compatibility.json: trailing JSON value")
		}
		return compatibility{}, fmt.Errorf("decode spice-compatibility.json trailing content: %w", err)
	}
	if contract.Schema != 1 {
		return compatibility{}, fmt.Errorf("spice-compatibility.json schema is %d; require 1", contract.Schema)
	}
	if contract.Minimum == "" {
		return compatibility{}, errors.New("spice-compatibility.json minimum is required")
	}
	if contract.Current == "" {
		return compatibility{}, errors.New("spice-compatibility.json current is required")
	}
	if contract.Minimum == contract.Current {
		return compatibility{}, errors.New("spice-compatibility.json minimum and current must differ")
	}
	return contract, nil
}

func ensureMinimum(contract compatibility, direct string) error {
	if contract.Minimum != direct {
		return fmt.Errorf("spice-compatibility.json minimum is %q; go.mod directly requires %q", contract.Minimum, direct)
	}
	return nil
}

func directRequirement(content, path string) (string, error) {
	var module struct {
		Require []struct {
			Path     string
			Version  string
			Indirect bool
		}
	}
	if err := json.Unmarshal([]byte(content), &module); err != nil {
		return "", fmt.Errorf("decode go.mod metadata: %w", err)
	}
	for _, requirement := range module.Require {
		if requirement.Path == path && !requirement.Indirect && requirement.Version != "" {
			return requirement.Version, nil
		}
	}
	return "", fmt.Errorf("go.mod must directly require %s at an exact version", path)
}

func alternateModfile(root string) (string, error) {
	source, err := os.ReadFile(filepath.Join(root, "go.mod")) // #nosec G304 -- root is repository-owned.
	if err != nil {
		return "", fmt.Errorf("read go.mod: %w", err)
	}
	file, err := os.CreateTemp(root, ".spice-core-compat-*.mod")
	if err != nil {
		return "", fmt.Errorf("create alternate modfile: %w", err)
	}
	name := file.Name()
	if _, err = file.Write(source); err != nil {
		return "", errors.Join(fmt.Errorf("write alternate modfile: %w", err), closeAndRemove(file, name))
	}
	if err = file.Close(); err != nil {
		return "", errors.Join(fmt.Errorf("close alternate modfile: %w", err), removeIfPresent(name))
	}
	return name, nil
}

func removeIfPresent(path string) error {
	err := os.Remove(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove temporary compatibility file %q: %w", path, err)
	}
	return nil
}

func closeAndRemove(file *os.File, path string) error {
	closeErr := file.Close()
	if closeErr != nil {
		closeErr = fmt.Errorf("close temporary compatibility file %q: %w", path, closeErr)
	}
	return errors.Join(closeErr, removeIfPresent(path))
}

func repositoryRoot() (string, error) {
	current, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	for {
		content, readErr := os.ReadFile(filepath.Join(current, "go.mod")) // #nosec G304 -- candidates are bounded ancestors.
		if readErr == nil && bytes.Contains(content, []byte("module "+repositoryModule)) {
			return current, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", errors.New("find starter-postgres repository root: go.mod not found")
		}
		current = parent
	}
}

func goCommand(ctx context.Context, directory string, arguments ...string) error {
	// #nosec G204,G702 -- arguments are repository-owned values.
	cmd := exec.CommandContext(ctx, "go", arguments...)
	cmd.Dir = directory
	cmd.Env = environment()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go %s: %w", strings.Join(arguments, " "), err)
	}
	return nil
}

func captureGo(ctx context.Context, directory string, arguments ...string) (string, error) {
	// #nosec G204,G702 -- arguments are repository-owned values.
	cmd := exec.CommandContext(ctx, "go", arguments...)
	cmd.Dir = directory
	cmd.Env = environment()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("go %s: %w\n%s", strings.Join(arguments, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func environment() []string {
	overrides := map[string]string{"GOFLAGS": "", "GOTOOLCHAIN": "local", "GOWORK": "off"}
	result := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, found := strings.Cut(entry, "=")
		if found {
			if _, replaced := overrides[strings.ToUpper(key)]; !replaced {
				result = append(result, entry)
			}
		}
	}
	for key, value := range overrides {
		result = append(result, key+"="+value)
	}
	slices.Sort(result)
	return result
}
