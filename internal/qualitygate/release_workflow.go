package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const releaseWorkflowRevision = "164e81ea4a31fa124670dc69afaec5bdf5747d78"

func checkReleaseWorkflow(root string) error {
	path := filepath.Join(root, ".github", "workflows", "release.yml")
	content, err := os.ReadFile(path) // #nosec G304 -- root and workflow path are repository-owned.
	if err != nil {
		return fmt.Errorf("read release workflow: %w", err)
	}
	if strings.ReplaceAll(string(content), "\r\n", "\n") != expectedReleaseWorkflow(modulePath) {
		return fmt.Errorf("release workflow must call the protected central workflow at %s for module %s without secret forwarding", releaseWorkflowRevision, modulePath)
	}
	return nil
}

func expectedReleaseWorkflow(module string) string {
	return fmt.Sprintf(`name: Release

on:
  push:
    tags:
      - "v[0-9]*.[0-9]*.[0-9]*"

# The called workflow narrows validation and signing to read-only access and
# reserves this maximum capability for its protected publish job.
permissions:
  contents: write

jobs:
  release:
    name: Centrally verify, sign, and publish
    uses: spice-framework/.github/.github/workflows/library-release.yml@%s
    with:
      module: %s
`, releaseWorkflowRevision, module)
}
