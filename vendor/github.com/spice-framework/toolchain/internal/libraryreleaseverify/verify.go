// Package libraryreleaseverify independently authenticates and verifies a
// signed Spice library release against an exact commit in a trusted checkout.
// It deliberately has no dependency on the central release renderer.
package libraryreleaseverify

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/mod/module"
)

// Config identifies untrusted artifacts and their independent trust inputs.
type Config struct {
	Directory        string
	Repository       string
	RepositoryName   string
	CanonicalSource  string
	Module           string
	Version          string
	Commit           string
	TrustedPublicKey []byte
}

// Result summarizes a successfully authenticated library release.
type Result struct {
	Files  []string
	Commit string
	Epoch  time.Time
	Module string
}

// Verify authenticates the exact five-file production artifact set and then
// validates the archive and SPDX document against trusted Git objects.
func Verify(ctx context.Context, config Config) (Result, error) {
	if ctx == nil {
		return Result{}, errors.New("verify library release: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, fmt.Errorf("verify library release: %w", err)
	}
	if config.Directory == "" || config.Repository == "" || config.CanonicalSource == "" || config.Module == "" {
		return Result{}, errors.New("verify library release: artifact directory, trusted repository, canonical source, and module are required")
	}
	if !safeRepositoryName(config.RepositoryName) {
		return Result{}, fmt.Errorf("verify library release: repository name %q is unsafe", config.RepositoryName)
	}
	canonicalSource, sourceRepository, sourceErr := trustedCanonicalSource(config.CanonicalSource)
	if sourceErr != nil {
		return Result{}, sourceErr
	}
	if sourceRepository != config.RepositoryName {
		return Result{}, fmt.Errorf(
			"trusted canonical source repository is %q, require %q",
			sourceRepository,
			config.RepositoryName,
		)
	}
	if pathErr := module.CheckPath(config.Module); pathErr != nil {
		return Result{}, fmt.Errorf("verify library release: trusted module %q is invalid: %w", config.Module, pathErr)
	}
	if !canonicalVersion(config.Version) {
		return Result{}, fmt.Errorf("verify library release: version %q is not canonical semantic version", config.Version)
	}
	if commitErr := validateExactCommit(config.Commit); commitErr != nil {
		return Result{}, commitErr
	}
	trustedKey, keyErr := parsePublicKey(config.TrustedPublicKey, "trusted public key")
	if keyErr != nil {
		return Result{}, keyErr
	}
	directory, directoryErr := filepath.Abs(config.Directory)
	if directoryErr != nil {
		return Result{}, fmt.Errorf("resolve library artifact directory: %w", directoryErr)
	}
	repository, repositoryErr := filepath.Abs(config.Repository)
	if repositoryErr != nil {
		return Result{}, fmt.Errorf("resolve trusted library repository: %w", repositoryErr)
	}
	expectedAssets, checksummedAssets := expectedAssetNames(config.RepositoryName, config.Version)
	if directoryErr := validateDirectory(directory, expectedAssets); directoryErr != nil {
		return Result{}, directoryErr
	}
	checksums, checksumErr := readBoundedRegularFile(directory, "checksums.txt", maxChecksumsBytes)
	if checksumErr != nil {
		return Result{}, checksumErr
	}
	if authenticationErr := authenticateChecksums(directory, checksums, trustedKey); authenticationErr != nil {
		return Result{}, authenticationErr
	}
	digests, checksumErr := parseChecksums(checksums, checksummedAssets)
	if checksumErr != nil {
		return Result{}, checksumErr
	}
	identity, source, sourceErr := trustedSource(ctx, repository, config.Commit)
	if sourceErr != nil {
		return Result{}, sourceErr
	}
	if identity.repositoryName != config.RepositoryName {
		return Result{}, fmt.Errorf(
			"trusted origin repository is %q, require %q",
			identity.repositoryName,
			config.RepositoryName,
		)
	}
	if identity.source != canonicalSource {
		return Result{}, fmt.Errorf(
			"trusted checkout origin is %q, require canonical source %q",
			identity.source,
			canonicalSource,
		)
	}
	versionName := strings.TrimPrefix(config.Version, "v")
	sourceName := config.RepositoryName + "_" + versionName + "_source.tar.gz"
	sourceData, artifactErr := readAuthenticatedArtifact(ctx, directory, sourceName, digests[sourceName])
	if artifactErr != nil {
		return Result{}, artifactErr
	}
	entries, archiveErr := readSourceArchive(ctx, sourceData, identity.epoch)
	if archiveErr != nil {
		return Result{}, fmt.Errorf("verify %s: %w", sourceName, archiveErr)
	}
	files, archiveErr := verifySourceArchive(entries, config.RepositoryName+"_"+versionName, source)
	if archiveErr != nil {
		return Result{}, fmt.Errorf("verify %s: %w", sourceName, archiveErr)
	}
	modules, modulePath, moduleErr := sourceModules(files)
	if moduleErr != nil {
		return Result{}, moduleErr
	}
	if modulePath != config.Module {
		return Result{}, fmt.Errorf("trusted source module is %q, require %q", modulePath, config.Module)
	}
	sbomName := config.RepositoryName + "_" + versionName + "_sbom.spdx.json"
	sbom, artifactErr := readAuthenticatedArtifact(ctx, directory, sbomName, digests[sbomName])
	if artifactErr != nil {
		return Result{}, artifactErr
	}
	if int64(len(sbom)) > rendererV1MaxSBOMBytes {
		return Result{}, fmt.Errorf("verify %s: exceeds control-file size limit", sbomName)
	}
	if sbomErr := verifySBOM(sbom, sbomIdentity{
		repository: config.RepositoryName,
		module:     config.Module,
		source:     canonicalSource,
		version:    config.Version,
		commit:     strings.ToLower(config.Commit),
		epoch:      identity.epoch,
	}, modules); sbomErr != nil {
		return Result{}, fmt.Errorf("verify %s: %w", sbomName, sbomErr)
	}
	return Result{
		Files:  slices.Clone(expectedAssets),
		Commit: strings.ToLower(config.Commit),
		Epoch:  identity.epoch,
		Module: modulePath,
	}, nil
}

func safeRepositoryName(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name &&
		!strings.ContainsAny(name, `/\:`)
}

func expectedAssetNames(repository, version string) ([]string, []string) {
	base := repository + "_" + strings.TrimPrefix(version, "v")
	checksummed := []string{base + "_sbom.spdx.json", base + "_source.tar.gz"}
	slices.Sort(checksummed)
	assets := append(slices.Clone(checksummed), "checksums.txt", "checksums.txt.pem", "checksums.txt.sig")
	slices.Sort(assets)
	return assets, checksummed
}

func validateDirectory(directory string, expected []string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("read library artifact directory: %w", err)
	}
	actual := make([]string, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect library artifact %q: %w", entry.Name(), err)
		}
		if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxArtifactBytes {
			return fmt.Errorf("library artifact %q is not a bounded regular file", entry.Name())
		}
		actual = append(actual, entry.Name())
	}
	slices.Sort(actual)
	if !slices.Equal(actual, expected) {
		return fmt.Errorf("library artifact set is %v, require exactly %v", actual, expected)
	}
	return nil
}

func authenticateChecksums(directory string, checksums []byte, trustedKey ed25519.PublicKey) error {
	emittedPEM, err := readBoundedRegularFile(directory, "checksums.txt.pem", maxPublicKeyBytes)
	if err != nil {
		return err
	}
	emittedKey, err := parsePublicKey(emittedPEM, "emitted public key")
	if err != nil {
		return err
	}
	if !bytes.Equal(emittedKey, trustedKey) {
		return errors.New("emitted library public key does not match trusted public key")
	}
	signature, err := readBoundedRegularFile(directory, "checksums.txt.sig", ed25519.SignatureSize)
	if err != nil {
		return err
	}
	if len(signature) != ed25519.SignatureSize {
		return fmt.Errorf("library signature length is %d, require %d", len(signature), ed25519.SignatureSize)
	}
	if !ed25519.Verify(trustedKey, checksums, signature) {
		return errors.New("library checksum signature is invalid")
	}
	return nil
}

func parsePublicKey(data []byte, label string) (ed25519.PublicKey, error) {
	if len(data) == 0 || len(data) > maxPublicKeyBytes {
		return nil, fmt.Errorf("parse %s: invalid size", label)
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "PUBLIC KEY" || len(block.Headers) != 0 || len(rest) != 0 {
		return nil, fmt.Errorf("parse %s: require one canonical PUBLIC KEY PEM block", label)
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", label, err)
	}
	key, ok := parsed.(ed25519.PublicKey)
	if !ok || len(key) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("parse %s: require Ed25519 public key", label)
	}
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return nil, fmt.Errorf("parse %s: encode canonical key: %w", label, err)
	}
	canonical := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	if !bytes.Equal(data, canonical) {
		return nil, fmt.Errorf("parse %s: PEM is not canonical", label)
	}
	return slices.Clone(key), nil
}

func parseChecksums(data []byte, expected []string) (map[string][sha256.Size]byte, error) {
	if len(data) == 0 || data[len(data)-1] != '\n' || bytes.ContainsRune(data, '\r') {
		return nil, errors.New("checksums.txt must use canonical LF-terminated lines")
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != len(expected) {
		return nil, fmt.Errorf("checksums.txt has %d lines, require %d", len(lines), len(expected))
	}
	result := make(map[string][sha256.Size]byte, len(lines))
	for index, line := range lines {
		digest, name, found := strings.Cut(line, "  ")
		if !found || strings.Contains(name, "  ") || name != expected[index] ||
			len(digest) != sha256.Size*2 || digest != strings.ToLower(digest) {
			return nil, fmt.Errorf("checksums.txt line %d is not canonical", index+1)
		}
		decoded, err := hex.DecodeString(digest)
		if err != nil || len(decoded) != sha256.Size {
			return nil, fmt.Errorf("checksums.txt line %d has invalid SHA-256", index+1)
		}
		result[name] = [sha256.Size]byte(decoded)
	}
	return result, nil
}

func readAuthenticatedArtifact(
	ctx context.Context,
	directory string,
	name string,
	expected [sha256.Size]byte,
) ([]byte, error) {
	data, err := readBoundedRegularFile(directory, name, maxArtifactBytes)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("read library artifact %q: %w", name, err)
	}
	if actual := sha256.Sum256(data); actual != expected {
		return nil, fmt.Errorf("library artifact %q has a SHA-256 mismatch", name)
	}
	return data, nil
}

func readBoundedRegularFile(directory, name string, maximum int64) (result []byte, resultErr error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, fmt.Errorf("open library artifact root: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	info, err := root.Lstat(name)
	if err != nil {
		return nil, fmt.Errorf("inspect library artifact %q: %w", name, err)
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maximum {
		return nil, fmt.Errorf("library artifact %q is not a regular file bounded to %d bytes", name, maximum)
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, fmt.Errorf("open library artifact %q: %w", name, err)
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	opened, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect library artifact %q: %w", name, err)
	}
	if !opened.Mode().IsRegular() || opened.Size() < 0 || opened.Size() > maximum ||
		!os.SameFile(info, opened) {
		return nil, fmt.Errorf("library artifact %q is not a regular file bounded to %d bytes", name, maximum)
	}
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, fmt.Errorf("read library artifact %q: %w", name, err)
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("library artifact %q exceeds %d bytes", name, maximum)
	}
	return data, nil
}
