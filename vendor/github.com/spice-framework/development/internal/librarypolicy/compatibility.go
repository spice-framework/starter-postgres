// Package librarypolicy owns shared policy checks for Spice library modules.
package librarypolicy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spice-framework/development/internal/catalog"
	"github.com/spice-framework/development/internal/process"
)

const maximumCompatibilityMetadata = 64 << 10

// Inspection is the trusted module and compatibility identity of one library.
type Inspection struct {
	Module        string
	Compatibility catalog.StarterCompatibility
}

type moduleFile struct {
	Module  moduleIdentity      `json:"Module"`
	Require []moduleRequirement `json:"Require"`
}

type moduleIdentity struct {
	Path string `json:"Path"`
}

type moduleRequirement struct {
	Path     string `json:"Path"`
	Version  string `json:"Version"`
	Indirect bool   `json:"Indirect"`
}

// Inspect validates one starter's compatibility file and direct core module
// requirement without consulting the network. Executed reports whether the Go
// metadata command ran; output is returned only when that command fails.
func Inspect(
	ctx context.Context,
	directory string,
	policy catalog.StarterCompatibilityPolicy,
	runner process.Runner,
) (Inspection, string, bool, error) {
	content, err := readCompatibilityMetadata(directory, policy.MetadataFile)
	if err != nil {
		return Inspection{}, "", false, err
	}
	metadata, err := catalog.ParseStarterCompatibility(content, policy)
	if err != nil {
		return Inspection{}, "", false, err
	}
	output, err := runner.Run(ctx, directory, "go", "mod", "edit", "-json")
	if err != nil {
		return Inspection{}, output, true, fmt.Errorf("inspect go.mod: %w", err)
	}
	module, err := inspectModule(output, policy.CoreModule, metadata.Minimum)
	if err != nil {
		return Inspection{}, "", true, err
	}
	return Inspection{Module: module, Compatibility: metadata}, "", true, nil
}

func readCompatibilityMetadata(directory string, name string) (_ []byte, resultErr error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, fmt.Errorf("open starter checkout: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	file, err := root.Open(name)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", name, err)
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", name, err)
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maximumCompatibilityMetadata {
		return nil, fmt.Errorf(
			"%s is not a regular file bounded to %d bytes",
			name,
			maximumCompatibilityMetadata,
		)
	}
	content, err := io.ReadAll(io.LimitReader(file, maximumCompatibilityMetadata+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	if len(content) > maximumCompatibilityMetadata {
		return nil, fmt.Errorf(
			"%s exceeds %d bytes",
			name,
			maximumCompatibilityMetadata,
		)
	}
	return content, nil
}

func inspectModule(content string, coreModule string, minimum string) (string, error) {
	decoder := json.NewDecoder(strings.NewReader(content))
	var file moduleFile
	if err := decoder.Decode(&file); err != nil {
		return "", fmt.Errorf("decode go.mod metadata: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return "", errors.New("go.mod metadata has trailing JSON values")
		}
		return "", fmt.Errorf("decode trailing go.mod metadata: %w", err)
	}
	if strings.TrimSpace(file.Module.Path) == "" {
		return "", errors.New("go.mod module path must be explicit")
	}
	matching := make([]moduleRequirement, 0, 1)
	for _, requirement := range file.Require {
		if requirement.Path == coreModule {
			matching = append(matching, requirement)
		}
	}
	if len(matching) == 0 {
		return "", fmt.Errorf("go.mod must directly require core module %q", coreModule)
	}
	if len(matching) != 1 {
		return "", fmt.Errorf("go.mod has %d requirements for core module %q", len(matching), coreModule)
	}
	requirement := matching[0]
	if requirement.Indirect {
		return "", fmt.Errorf("go.mod requirement for core module %q must be direct", coreModule)
	}
	if requirement.Version != minimum {
		return "", fmt.Errorf(
			"starter compatibility minimum %q does not match direct go.mod requirement %q",
			minimum,
			requirement.Version,
		)
	}
	return file.Module.Path, nil
}
