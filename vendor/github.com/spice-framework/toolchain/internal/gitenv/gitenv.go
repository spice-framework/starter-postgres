// Package gitenv provides a fail-closed environment for read-only release Git
// operations.
package gitenv

import (
	"os"
	"strings"
)

// ReadOnly removes ambient repository and object substitutions, disables Git
// replacement objects and locks, and ignores machine-global configuration.
func ReadOnly(environment []string) []string {
	result := make([]string, 0, len(environment)+5)
	for _, entry := range environment {
		name, _, found := strings.Cut(entry, "=")
		if found && unsafeVariable(strings.ToUpper(name)) {
			continue
		}
		result = append(result, entry)
	}
	return append(
		result,
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_OPTIONAL_LOCKS=0",
	)
}

func unsafeVariable(name string) bool {
	if strings.HasPrefix(name, "GIT_CONFIG_KEY_") ||
		strings.HasPrefix(name, "GIT_CONFIG_VALUE_") {
		return true
	}
	switch name {
	case "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CEILING_DIRECTORIES",
		"GIT_COMMON_DIR", "GIT_CONFIG_COUNT", "GIT_CONFIG_GLOBAL",
		"GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_SYSTEM", "GIT_DIR",
		"GIT_DISCOVERY_ACROSS_FILESYSTEM", "GIT_INDEX_FILE", "GIT_NAMESPACE",
		"GIT_NO_REPLACE_OBJECTS", "GIT_OBJECT_DIRECTORY", "GIT_OPTIONAL_LOCKS",
		"GIT_REPLACE_REF_BASE", "GIT_SHALLOW_FILE", "GIT_WORK_TREE":
		return true
	default:
		return false
	}
}
