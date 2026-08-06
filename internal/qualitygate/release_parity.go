package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"
)

const (
	developmentModule        = "github.com/spice-framework/development"
	developmentTool          = developmentModule + "/cmd/spice-dev"
	developmentVersion       = "v0.0.0-20260806034648-1856466df09d"
	rehearsalVersion         = "v0.0.0-rehearsal"
	maximumParityArchiveSize = 256 << 20
	maximumParityEntrySize   = 128 << 20
)

func requireReleaseTool(ctx context.Context, root string) error {
	content, err := capture(ctx, root, nil, "go", "mod", "edit", "-json")
	if err != nil {
		return fmt.Errorf("read release tool authorization: %w", err)
	}
	return validateReleaseToolAuthorization([]byte(content))
}

func validateReleaseToolAuthorization(content []byte) error {
	var metadata struct {
		Require []struct {
			Path    string
			Version string
		}
		Tool []struct {
			Path string
		}
	}
	if err := json.Unmarshal(content, &metadata); err != nil {
		return fmt.Errorf("decode release tool authorization: %w", err)
	}
	toolCount := 0
	for _, tool := range metadata.Tool {
		if tool.Path == developmentTool {
			toolCount++
		}
	}
	if toolCount != 1 {
		return fmt.Errorf(
			"go.mod must authorize exactly one %s tool declaration; found %d",
			developmentTool,
			toolCount,
		)
	}
	for _, requirement := range metadata.Require {
		if requirement.Path != developmentModule {
			continue
		}
		if requirement.Version != developmentVersion {
			return fmt.Errorf(
				"go.mod selects release tool %s; require exactly %s",
				requirement.Version,
				developmentVersion,
			)
		}
		return nil
	}
	return fmt.Errorf(
		"go.mod must require %s at exactly %s",
		developmentModule,
		developmentVersion,
	)
}

func releaseParity(ctx context.Context, root string) error {
	parent, err := os.MkdirTemp("", "starter-postgres-release-parity-*")
	if err != nil {
		return fmt.Errorf("create release parity root: %w", err)
	}
	defer removeTree(parent)

	offlineVendor := map[string]string{"GOFLAGS": "-mod=vendor"}
	resolved, err := capture(ctx, root, offlineVendor, "go", "tool", "-n", developmentTool)
	if err != nil {
		return fmt.Errorf("resolve authorized central release tool: %w", err)
	}
	if strings.TrimSpace(resolved) == "" {
		return errors.New("resolve authorized central release tool: empty executable path")
	}

	plan, err := capture(
		ctx,
		root,
		offlineVendor,
		"go",
		"tool",
		developmentTool,
		"library-release",
		"plan",
		"--root="+root,
		"--repo=starter-postgres",
		"--version="+rehearsalVersion,
		"--rehearsal",
	)
	if err != nil {
		return fmt.Errorf("plan central release rehearsal: %w", err)
	}
	planFile := filepath.Join(parent, "plan.json")
	if writeErr := os.WriteFile(planFile, []byte(plan+"\n"), 0o600); writeErr != nil {
		return fmt.Errorf("write central release rehearsal plan: %w", writeErr)
	}

	centralOutputs := []string{
		filepath.Join(parent, "central-first"),
		filepath.Join(parent, "central-second"),
	}
	for _, outputDir := range centralOutputs {
		if commandErr := command(
			ctx,
			root,
			offlineVendor,
			"go",
			"tool",
			developmentTool,
			"library-release",
			"render",
			"--root="+root,
			"--plan="+planFile,
			"--output="+outputDir,
		); commandErr != nil {
			return fmt.Errorf("render central release rehearsal: %w", commandErr)
		}
	}

	retainedOutputs := []string{
		filepath.Join(parent, "retained-first"),
		filepath.Join(parent, "retained-second"),
	}
	for _, outputDir := range retainedOutputs {
		if commandErr := command(
			ctx,
			root,
			offlineVendor,
			"go",
			"run",
			"./cmd/starter-postgres-release",
			"-rehearsal",
			"-version="+rehearsalVersion,
			"-output="+outputDir,
		); commandErr != nil {
			return fmt.Errorf("render retained release rehearsal: %w", commandErr)
		}
	}

	central, err := deterministicReleaseArtifacts("central", centralOutputs)
	if err != nil {
		return err
	}
	retained, err := deterministicReleaseArtifacts("retained", retainedOutputs)
	if err != nil {
		return err
	}
	return validateReleaseParity(centralOutputs[0], central, retainedOutputs[0], retained)
}

func deterministicReleaseArtifacts(
	name string,
	outputs []string,
) (map[string][sha256.Size]byte, error) {
	first, err := treeDigests(outputs[0])
	if err != nil {
		return nil, err
	}
	second, err := treeDigests(outputs[1])
	if err != nil {
		return nil, err
	}
	if !maps.Equal(first, second) {
		return nil, fmt.Errorf("identical %s release rehearsals produced different artifacts", name)
	}
	return first, nil
}

func validateReleaseParity(
	centralRoot string,
	central map[string][sha256.Size]byte,
	retainedRoot string,
	retained map[string][sha256.Size]byte,
) error {
	base := "starter-postgres_" + strings.TrimPrefix(rehearsalVersion, "v")
	archiveName := base + "_source.tar.gz"
	sbomName := base + "_sbom.spdx.json"
	expected := []string{"checksums.txt", sbomName, archiveName}
	for name, artifacts := range map[string]map[string][sha256.Size]byte{
		"central":  central,
		"retained": retained,
	} {
		actual := slices.Sorted(maps.Keys(artifacts))
		if !slices.Equal(actual, expected) {
			return fmt.Errorf(
				"%s release rehearsal artifacts %v do not match %v; signatures are forbidden",
				name,
				actual,
				expected,
			)
		}
	}
	if err := validateReleaseChecksums(centralRoot, central, sbomName, archiveName); err != nil {
		return fmt.Errorf("central release rehearsal: %w", err)
	}
	if err := validateReleaseChecksums(retainedRoot, retained, sbomName, archiveName); err != nil {
		return fmt.Errorf("retained release rehearsal: %w", err)
	}
	if err := validateReleaseArchiveParity(
		filepath.Join(centralRoot, archiveName),
		"starter-postgres_"+strings.TrimPrefix(rehearsalVersion, "v")+"/",
		filepath.Join(retainedRoot, archiveName),
		"starter-postgres-"+strings.TrimPrefix(rehearsalVersion, "v")+"/",
	); err != nil {
		return err
	}
	centralSBOM, err := readReleaseArtifact(centralRoot, sbomName)
	if err != nil {
		return err
	}
	retainedSBOM, err := readReleaseArtifact(retainedRoot, sbomName)
	if err != nil {
		return err
	}
	return validateReleaseSBOMParity(centralSBOM, retainedSBOM)
}

func validateReleaseChecksums(
	root string,
	artifacts map[string][sha256.Size]byte,
	names ...string,
) error {
	content, err := readReleaseArtifact(root, "checksums.txt")
	if err != nil {
		return err
	}
	if len(content) == 0 || content[len(content)-1] != '\n' {
		return errors.New("checksums.txt must end with one newline")
	}
	lines := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
	orderedNames := slices.Clone(names)
	slices.Sort(orderedNames)
	if len(lines) != len(orderedNames) {
		return fmt.Errorf("checksums.txt has %d lines; require %d", len(lines), len(orderedNames))
	}
	for index, name := range orderedNames {
		want := fmt.Sprintf("%x  %s", artifacts[name], name)
		if lines[index] != want {
			return fmt.Errorf("checksums.txt line %d is %q; require canonical %q", index+1, lines[index], want)
		}
	}
	return nil
}

type parityArchive struct {
	Gzip    parityGzipHeader
	Entries []parityArchiveEntry
}

type parityGzipHeader struct {
	Comment string
	Extra   []byte
	ModTime time.Time
	Name    string
	OS      byte
}

type parityArchiveEntry struct {
	Name       string
	Linkname   string
	Size       int64
	Mode       int64
	UID        int
	GID        int
	Uname      string
	Gname      string
	ModTime    time.Time
	AccessTime time.Time
	ChangeTime time.Time
	Typeflag   byte
	Devmajor   int64
	Devminor   int64
	Format     tar.Format
	PAXRecords map[string]string
	Digest     [sha256.Size]byte
}

func validateReleaseArchiveParity(
	centralPath string,
	centralPrefix string,
	retainedPath string,
	retainedPrefix string,
) error {
	central, err := readParityArchive(centralPath, centralPrefix)
	if err != nil {
		return fmt.Errorf("read central source archive: %w", err)
	}
	retained, err := readParityArchive(retainedPath, retainedPrefix)
	if err != nil {
		return fmt.Errorf("read retained source archive: %w", err)
	}
	if !reflect.DeepEqual(central, retained) {
		return errors.New("central and retained source archives differ outside their documented root prefixes")
	}
	return nil
}

func readParityArchive(filename string, expectedPrefix string) (parityArchive, error) {
	content, err := os.ReadFile(filename) // #nosec G304 -- caller supplies a generated temporary artifact path.
	if err != nil {
		return parityArchive{}, err
	}
	if len(content) > maximumParityArchiveSize {
		return parityArchive{}, fmt.Errorf("compressed archive exceeds %d bytes", maximumParityArchiveSize)
	}
	gzipReader, err := gzip.NewReader(bytes.NewReader(content))
	if err != nil {
		return parityArchive{}, err
	}
	gzipReader.Multistream(false)
	result := parityArchive{Gzip: parityGzipHeader{
		Comment: gzipReader.Comment,
		Extra:   bytes.Clone(gzipReader.Extra),
		ModTime: gzipReader.ModTime,
		Name:    gzipReader.Name,
		OS:      gzipReader.OS,
	}}
	entries, err := readParityArchiveEntries(tar.NewReader(gzipReader), expectedPrefix)
	if err != nil {
		return parityArchive{}, errors.Join(err, gzipReader.Close())
	}
	result.Entries = entries
	remaining, err := io.Copy(
		io.Discard,
		io.LimitReader(gzipReader, maximumParityArchiveSize+1),
	)
	if err != nil {
		return parityArchive{}, errors.Join(err, gzipReader.Close())
	}
	if remaining != 0 {
		return parityArchive{}, errors.Join(
			fmt.Errorf("decompressed archive has %d hidden trailing bytes", remaining),
			gzipReader.Close(),
		)
	}
	if err := gzipReader.Close(); err != nil {
		return parityArchive{}, err
	}
	return result, nil
}

func readParityArchiveEntries(
	tarReader *tar.Reader,
	expectedPrefix string,
) ([]parityArchiveEntry, error) {
	seen := make(map[string]struct{})
	entries := make([]parityArchiveEntry, 0)
	var total int64
	for {
		header, nextErr := tarReader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return nil, nextErr
		}
		entry, err := readParityArchiveEntry(tarReader, header, expectedPrefix, seen, &total)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func readParityArchiveEntry(
	tarReader *tar.Reader,
	header *tar.Header,
	expectedPrefix string,
	seen map[string]struct{},
	total *int64,
) (parityArchiveEntry, error) {
	name, found := strings.CutPrefix(header.Name, expectedPrefix)
	if !found || name == "" || path.Clean(name) != name || strings.HasPrefix(name, "../") || path.IsAbs(name) {
		return parityArchiveEntry{}, fmt.Errorf(
			"archive entry %q is outside required root %q",
			header.Name,
			expectedPrefix,
		)
	}
	if _, duplicate := seen[name]; duplicate {
		return parityArchiveEntry{}, fmt.Errorf("archive entry %q is duplicated", name)
	}
	seen[name] = struct{}{}
	if header.Size < 0 || header.Size > maximumParityEntrySize ||
		*total > maximumParityArchiveSize-header.Size {
		return parityArchiveEntry{}, fmt.Errorf("archive entry %q exceeds parity bounds", name)
	}
	*total += header.Size
	digest := sha256.New()
	if _, err := io.CopyN(digest, tarReader, header.Size); err != nil {
		return parityArchiveEntry{}, err
	}
	var contentDigest [sha256.Size]byte
	copy(contentDigest[:], digest.Sum(nil))
	paxRecords := maps.Clone(header.PAXRecords)
	if paxPath, present := paxRecords["path"]; present {
		relativePAXPath, valid := strings.CutPrefix(paxPath, expectedPrefix)
		if !valid || relativePAXPath != name {
			return parityArchiveEntry{}, fmt.Errorf(
				"archive PAX path %q does not match entry %q",
				paxPath,
				header.Name,
			)
		}
		paxRecords["path"] = name
	}
	return parityArchiveEntry{
		Name: name, Linkname: header.Linkname, Size: header.Size, Mode: header.Mode,
		UID: header.Uid, GID: header.Gid, Uname: header.Uname, Gname: header.Gname,
		ModTime: header.ModTime, AccessTime: header.AccessTime, ChangeTime: header.ChangeTime,
		Typeflag: header.Typeflag, Devmajor: header.Devmajor, Devminor: header.Devminor,
		Format: header.Format, PAXRecords: paxRecords, Digest: contentDigest,
	}, nil
}

type releaseSBOM struct {
	SPDXVersion       string             `json:"spdxVersion"`
	DataLicense       string             `json:"dataLicense"`
	SPDXID            string             `json:"SPDXID"`
	Name              string             `json:"name"`
	DocumentNamespace string             `json:"documentNamespace"`
	CreationInfo      sbomCreationInfo   `json:"creationInfo"`
	Packages          []sbomPackage      `json:"packages"`
	Relationships     []sbomRelationship `json:"relationships"`
}

type sbomCreationInfo struct {
	Created  string   `json:"created"`
	Creators []string `json:"creators"`
}

type sbomPackage struct {
	Name             string            `json:"name"`
	SPDXID           string            `json:"SPDXID"`
	VersionInfo      string            `json:"versionInfo"`
	DownloadLocation string            `json:"downloadLocation"`
	FilesAnalyzed    bool              `json:"filesAnalyzed"`
	LicenseConcluded string            `json:"licenseConcluded"`
	LicenseDeclared  string            `json:"licenseDeclared"`
	CopyrightText    string            `json:"copyrightText"`
	ExternalRefs     []sbomExternalRef `json:"externalRefs,omitempty"`
}

type sbomExternalRef struct {
	ReferenceCategory string `json:"referenceCategory"`
	ReferenceType     string `json:"referenceType"`
	ReferenceLocator  string `json:"referenceLocator"`
}

type sbomRelationship struct {
	SPDXElementID      string `json:"spdxElementId"`
	RelationshipType   string `json:"relationshipType"`
	RelatedSPDXElement string `json:"relatedSpdxElement"`
}

func validateReleaseSBOMParity(centralContent, retainedContent []byte) error {
	central, err := decodeReleaseSBOM(centralContent)
	if err != nil {
		return fmt.Errorf("decode central release SBOM: %w", err)
	}
	retained, err := decodeReleaseSBOM(retainedContent)
	if err != nil {
		return fmt.Errorf("decode retained release SBOM: %w", err)
	}
	baseNamespace := "https://github.com/spice-framework/starter-postgres/releases/" +
		rehearsalVersion + "/spdx/"
	if provenanceErr := validateCentralSBOMProvenance(central, baseNamespace); provenanceErr != nil {
		return provenanceErr
	}
	if provenanceErr := validateRetainedSBOMProvenance(retained, baseNamespace); provenanceErr != nil {
		return provenanceErr
	}
	if central.DocumentNamespace == retained.DocumentNamespace {
		return errors.New("central and retained SBOM namespaces must identify their distinct builders")
	}
	rootID, found := releaseRootPackageID(central.Packages)
	if !found {
		return errors.New("central release SBOM has no exact PostgreSQL root package")
	}
	centralRelationships, err := removeCentralDescribes(central.Relationships, rootID)
	if err != nil {
		return err
	}
	for _, relationship := range retained.Relationships {
		if relationship.RelationshipType == "DESCRIBES" {
			return errors.New("retained R1 release SBOM must not contain DESCRIBES")
		}
	}
	central.Relationships = centralRelationships
	central.Name = retained.Name
	central.DocumentNamespace = retained.DocumentNamespace
	central.CreationInfo.Creators = slices.Clone(retained.CreationInfo.Creators)
	sortReleaseSBOM(&central)
	sortReleaseSBOM(&retained)
	if !reflect.DeepEqual(central, retained) {
		return errors.New("central and retained SBOMs differ outside documented R1 provenance, ordering, and DESCRIBES fields")
	}
	return nil
}

func validateCentralSBOMProvenance(document releaseSBOM, baseNamespace string) error {
	if document.Name != "starter-postgres "+rehearsalVersion ||
		!validSBOMNamespace(document.DocumentNamespace, baseNamespace+"v1/") ||
		!slices.Equal(document.CreationInfo.Creators, []string{
			"Organization: Spice Framework",
			"Tool: github.com/spice-framework/development/cmd/spice-dev library-release renderer/v1",
		}) {
		return errors.New("central release SBOM provenance does not match renderer/v1")
	}
	return nil
}

func validateRetainedSBOMProvenance(document releaseSBOM, baseNamespace string) error {
	if document.Name != "Spice PostgreSQL Starter "+rehearsalVersion ||
		!validSBOMNamespace(document.DocumentNamespace, baseNamespace) ||
		strings.HasPrefix(document.DocumentNamespace, baseNamespace+"v1/") ||
		!slices.Equal(document.CreationInfo.Creators, []string{
			"Organization: Spice Framework",
			"Tool: github.com/spice-framework/starter-postgres/cmd/starter-postgres-release",
		}) {
		return errors.New("retained release SBOM provenance does not match the PostgreSQL builder")
	}
	return nil
}

func releaseRootPackageID(packages []sbomPackage) (string, bool) {
	for _, item := range packages {
		if item.Name == modulePath && item.VersionInfo == rehearsalVersion {
			return item.SPDXID, true
		}
	}
	return "", false
}

func removeCentralDescribes(
	relationships []sbomRelationship,
	rootID string,
) ([]sbomRelationship, error) {
	result := make([]sbomRelationship, 0, len(relationships)-1)
	count := 0
	for _, relationship := range relationships {
		if relationship.RelationshipType != "DESCRIBES" {
			result = append(result, relationship)
			continue
		}
		if relationship.SPDXElementID != "SPDXRef-DOCUMENT" ||
			relationship.RelatedSPDXElement != rootID {
			return nil, errors.New("central release SBOM has an invalid DESCRIBES relationship")
		}
		count++
	}
	if count != 1 {
		return nil, fmt.Errorf("central release SBOM has %d DESCRIBES relationships; require one", count)
	}
	return result, nil
}

func sortReleaseSBOM(document *releaseSBOM) {
	slices.SortFunc(document.Packages, func(left, right sbomPackage) int {
		if result := strings.Compare(left.Name, right.Name); result != 0 {
			return result
		}
		if result := strings.Compare(left.VersionInfo, right.VersionInfo); result != 0 {
			return result
		}
		return strings.Compare(left.SPDXID, right.SPDXID)
	})
	slices.SortFunc(document.Relationships, func(left, right sbomRelationship) int {
		leftKey := left.SPDXElementID + "\x00" + left.RelationshipType + "\x00" + left.RelatedSPDXElement
		rightKey := right.SPDXElementID + "\x00" + right.RelationshipType + "\x00" + right.RelatedSPDXElement
		return strings.Compare(leftKey, rightKey)
	})
}

func decodeReleaseSBOM(content []byte) (releaseSBOM, error) {
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var result releaseSBOM
	if err := decoder.Decode(&result); err != nil {
		return releaseSBOM{}, err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return releaseSBOM{}, errors.New("release SBOM has trailing JSON values")
		}
		return releaseSBOM{}, err
	}
	return result, nil
}

func validSBOMNamespace(value, prefix string) bool {
	digest, found := strings.CutPrefix(value, prefix)
	if !found || len(digest) != sha256.Size*2 {
		return false
	}
	for _, character := range digest {
		decimal := character >= '0' && character <= '9'
		hex := character >= 'a' && character <= 'f'
		if !decimal && !hex {
			return false
		}
	}
	return true
}

func readReleaseArtifact(rootPath string, name string) (_ []byte, resultErr error) {
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, fmt.Errorf("open release artifact root: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	content, err := root.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("read release artifact %q: %w", name, err)
	}
	return content, nil
}
