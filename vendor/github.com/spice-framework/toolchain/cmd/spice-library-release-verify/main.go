// Command spice-library-release-verify independently authenticates a signed
// Spice library release against an exact commit in a trusted checkout.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spice-framework/toolchain/internal/libraryreleaseverify"
)

const maxTrustedKeyFileBytes = 64 << 10

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("spice-library-release-verify", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var artifacts, root, repository, source, module, version, commit, trustedPublicKey string
	flags.StringVar(&artifacts, "artifacts", "", "signed library artifact directory")
	flags.StringVar(&root, "root", ".", "trusted library repository root")
	flags.StringVar(&repository, "repository", "", "trusted repository name")
	flags.StringVar(&source, "source", "", "trusted canonical HTTPS source URL")
	flags.StringVar(&module, "module", "", "trusted canonical Go module path")
	flags.StringVar(&version, "version", "", "canonical v-prefixed release version")
	flags.StringVar(&commit, "commit", "", "exact trusted Git commit object ID")
	flags.StringVar(&trustedPublicKey, "trusted-public-key", "", "trusted Ed25519 public-key PEM file")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		return writeExit(stderr, 2, "unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if artifacts == "" || repository == "" || source == "" || module == "" ||
		version == "" || commit == "" || trustedPublicKey == "" {
		return writeExit(
			stderr,
			2,
			"-artifacts, -repository, -source, -module, -version, -commit, and -trusted-public-key are required",
		)
	}
	key, err := readBoundedFile(trustedPublicKey, maxTrustedKeyFileBytes)
	if err != nil {
		return writeExit(stderr, 1, "read trusted public key: %v", err)
	}
	result, err := libraryreleaseverify.Verify(ctx, libraryreleaseverify.Config{
		Directory: artifacts, Repository: root, RepositoryName: repository,
		CanonicalSource: source, Module: module, Version: version,
		Commit: commit, TrustedPublicKey: key,
	})
	if err != nil {
		return writeExit(stderr, 1, "%v", err)
	}
	if _, err := fmt.Fprintf(
		stdout,
		"Spice library release %s@%s verified: %d artifacts at %s.\n",
		result.Module,
		version,
		len(result.Files),
		result.Commit,
	); err != nil {
		return 1
	}
	return 0
}

func readBoundedFile(filename string, maximum int64) (result []byte, resultErr error) {
	absolute, err := filepath.Abs(filename)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(filepath.Dir(absolute))
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	name := filepath.Base(absolute)
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maximum {
		return nil, fmt.Errorf("file is not a regular file bounded to %d bytes", maximum)
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || opened.Size() < 0 || opened.Size() > maximum ||
		!os.SameFile(info, opened) {
		return nil, errors.New("trusted public-key file changed while it was opened")
	}
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("file exceeds %d bytes", maximum)
	}
	return data, nil
}

func writeExit(writer io.Writer, code int, format string, arguments ...any) int {
	if _, err := fmt.Fprintf(writer, "spice-library-release-verify: "+format+"\n", arguments...); err != nil {
		return 1
	}
	return code
}
