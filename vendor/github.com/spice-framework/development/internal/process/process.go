// Package process runs bounded, shell-free development commands.
package process

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const maximumOutput = 2 << 20

type Runner interface {
	Run(context.Context, string, ...string) (string, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(
	ctx context.Context,
	directory string,
	arguments ...string,
) (string, error) {
	if ctx == nil {
		return "", errors.New("process context must not be nil")
	}
	if len(arguments) == 0 || !allowedExecutable(arguments[0]) {
		return "", errors.New("development command executable is not allowed")
	}
	var output boundedBuffer
	// #nosec G204 -- allowedExecutable restricts the binary and CommandContext receives discrete arguments without a shell.
	command := exec.CommandContext(ctx, arguments[0], arguments[1:]...)
	command.Dir = directory
	command.Env = IndependentEnvironment()
	command.Stdout = &output
	command.Stderr = &output
	err := command.Run()
	text := output.String()
	if err != nil {
		return text, fmt.Errorf(
			"run %s: %w%s",
			strings.Join(arguments, " "),
			err,
			commandDetail(text),
		)
	}
	if output.truncated {
		return text, fmt.Errorf(
			"run %s: output exceeded %d bytes",
			strings.Join(arguments, " "),
			maximumOutput,
		)
	}
	return text, nil
}

type boundedBuffer struct {
	content   bytes.Buffer
	truncated bool
}

func (buffer *boundedBuffer) Write(content []byte) (int, error) {
	written := len(content)
	remaining := maximumOutput - buffer.content.Len()
	if remaining <= 0 {
		buffer.truncated = true
		return written, nil
	}
	if len(content) > remaining {
		content = content[:remaining]
		buffer.truncated = true
	}
	_, _ = buffer.content.Write(content)
	return written, nil
}

func (buffer *boundedBuffer) String() string {
	return strings.TrimSpace(buffer.content.String())
}

func allowedExecutable(name string) bool {
	return name == "cargo" || name == "git" || name == "go" ||
		name == "gofmt" || name == "java"
}

func commandDetail(output string) string {
	if output == "" {
		return ""
	}
	return ":\n" + output
}

// IndependentEnvironment isolates repository commands from ambient workspace,
// module-network, and Git object-redirection state.
func IndependentEnvironment() []string {
	return independentEnvironmentFrom(os.Environ())
}

func independentEnvironmentFrom(environment []string) []string {
	result := make([]string, 0, len(environment)+8)
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		upperName := strings.ToUpper(name)
		if blockedEnvironmentName(upperName) {
			continue
		}
		result = append(result, entry)
	}
	return append(
		result,
		"GOWORK=off",
		"GOPROXY=off",
		"GOFLAGS=",
		"GOTOOLCHAIN=local",
		"CARGO_NET_OFFLINE=true",
		"RUSTUP_AUTO_INSTALL=0",
		"GIT_NO_LAZY_FETCH=1",
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_TERMINAL_PROMPT=0",
	)
}

func blockedEnvironmentName(name string) bool {
	if strings.HasPrefix(name, "GIT_CONFIG_KEY_") ||
		strings.HasPrefix(name, "GIT_CONFIG_VALUE_") {
		return true
	}
	switch name {
	case "CARGO_NET_OFFLINE",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES",
		"GIT_COMMON_DIR",
		"GIT_CONFIG_COUNT",
		"GIT_CONFIG_PARAMETERS",
		"GIT_DIR",
		"GIT_EXEC_PATH",
		"GIT_INDEX_FILE",
		"GIT_NAMESPACE",
		"GIT_NO_LAZY_FETCH",
		"GIT_NO_REPLACE_OBJECTS",
		"GIT_OBJECT_DIRECTORY",
		"GIT_OPTIONAL_LOCKS",
		"GIT_PREFIX",
		"GIT_REPLACE_REF_BASE",
		"GIT_SHALLOW_FILE",
		"GIT_TERMINAL_PROMPT",
		"GIT_WORK_TREE",
		"GOFLAGS",
		"GOPROXY",
		"GOTOOLCHAIN",
		"GOWORK",
		"RUSTUP_AUTO_INSTALL":
		return true
	default:
		return false
	}
}
