package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const releaseWorkflowRevision = "37dbac1ce9a616574f899afcb531f2097c71855c"

func checkReleaseWorkflow(root string) error {
	path := filepath.Join(root, ".github", "workflows", "release.yml")
	content, err := os.ReadFile(path) // #nosec G304 -- root and workflow path are repository-owned.
	if err != nil {
		return fmt.Errorf("read release workflow: %w", err)
	}
	if strings.ReplaceAll(string(content), "\r\n", "\n") != expectedReleaseWorkflow(modulePath) {
		return fmt.Errorf("release workflow must call the protected central workflow at %s for module %s with only the named signing secret", releaseWorkflowRevision, modulePath)
	}
	return nil
}

func expectedReleaseWorkflow(module string) string {
	return fmt.Sprintf(`name: Release

on:
  push:
    tags:
      - "v[0-9]*.[0-9]*.[0-9]*"

permissions: {}

jobs:
  release:
    name: Centrally verify, sign, and publish
    permissions:
      contents: write
    uses: spice-framework/.github/.github/workflows/library-release.yml@%s
    secrets:
      SPICE_LIBRARY_RELEASE_SIGNING_KEY: ${{ secrets.SPICE_LIBRARY_RELEASE_SIGNING_KEY }}
    with:
      module: %s
`, releaseWorkflowRevision, module)
}
