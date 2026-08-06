package libraryreleaseverify

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spice-framework/toolchain/internal/gitenv"
)

const (
	maxArchiveEntries     = 100_000
	maxGitTreeBytes       = 16 << 20
	maxGitDiagnosticBytes = 32 << 10
)

type sourceIdentity struct {
	epoch          time.Time
	source         string
	repositoryName string
}

type sourceEntry struct {
	path       string
	objectID   string
	mode       os.FileMode
	data       []byte
	linkTarget string
}

type archiveEntry struct {
	name       string
	mode       os.FileMode
	data       []byte
	linkTarget string
}

func validateExactCommit(commit string) error {
	if len(commit) != 40 && len(commit) != 64 {
		return errors.New("verify library release: commit must be an exact 40- or 64-hex object ID")
	}
	if _, err := hex.DecodeString(commit); err != nil {
		return fmt.Errorf("verify library release: commit is not hexadecimal: %w", err)
	}
	if commit != strings.ToLower(commit) {
		return errors.New("verify library release: commit must use lowercase hexadecimal")
	}
	return nil
}

func trustedSource(
	ctx context.Context,
	repository string,
	commit string,
) (sourceIdentity, []sourceEntry, error) {
	resolved, err := gitOutput(ctx, repository, 256, "rev-parse", "--verify", commit+"^{commit}")
	if err != nil {
		return sourceIdentity{}, nil, fmt.Errorf("resolve trusted library commit: %w", err)
	}
	if strings.TrimSpace(string(resolved)) != commit {
		return sourceIdentity{}, nil, errors.New("trusted library commit did not resolve to the exact requested object")
	}
	epochText, err := gitOutput(ctx, repository, 128, "show", "-s", "--format=%ct", commit)
	if err != nil {
		return sourceIdentity{}, nil, fmt.Errorf("read trusted library commit epoch: %w", err)
	}
	seconds, err := strconv.ParseInt(strings.TrimSpace(string(epochText)), 10, 64)
	if err != nil || seconds <= 0 {
		return sourceIdentity{}, nil, fmt.Errorf("parse trusted library commit epoch %q", strings.TrimSpace(string(epochText)))
	}
	remote, err := gitOutput(ctx, repository, 4096, "remote", "get-url", "origin")
	if err != nil {
		return sourceIdentity{}, nil, fmt.Errorf("read trusted library origin: %w", err)
	}
	source, repositoryName, err := canonicalSourceURL(string(remote))
	if err != nil {
		return sourceIdentity{}, nil, fmt.Errorf("parse trusted library origin: %w", err)
	}
	tree, err := gitOutput(ctx, repository, maxGitTreeBytes, "ls-tree", "-rz", "--full-tree", commit)
	if err != nil {
		return sourceIdentity{}, nil, fmt.Errorf("read trusted library commit tree: %w", err)
	}
	entries, err := parseGitTree(tree)
	if err != nil {
		return sourceIdentity{}, nil, err
	}
	if err := readGitBlobs(ctx, repository, entries); err != nil {
		return sourceIdentity{}, nil, err
	}
	return sourceIdentity{
		epoch: time.Unix(seconds, 0).UTC(), source: source, repositoryName: repositoryName,
	}, entries, nil
}

func canonicalSourceURL(value string) (string, string, error) {
	value = strings.TrimSpace(value)
	if before, after, found := strings.Cut(value, ":"); found &&
		!strings.Contains(before, "/") && strings.Contains(before, "@") {
		value = "ssh://" + before + "/" + after
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", "", err
	}
	if !slices.Contains([]string{"https", "ssh"}, parsed.Scheme) || parsed.Hostname() == "" ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", errors.New("require an HTTPS or SSH origin without query or fragment")
	}
	if parsed.Scheme == "https" && parsed.User != nil {
		return "", "", errors.New("HTTPS origin must not contain credentials")
	}
	if parsed.Scheme == "ssh" && (parsed.User == nil || parsed.User.Username() != "git") {
		return "", "", errors.New("SSH origin must use the git user")
	}
	repositoryPath := strings.TrimSuffix(strings.Trim(parsed.Path, "/"), ".git")
	if repositoryPath == "" || path.Clean(repositoryPath) != repositoryPath ||
		repositoryPath == ".." || strings.HasPrefix(repositoryPath, "../") {
		return "", "", errors.New("origin repository path is empty or unsafe")
	}
	host := strings.ToLower(parsed.Hostname())
	if parsed.Port() != "" {
		host += ":" + parsed.Port()
	}
	name := path.Base(repositoryPath)
	if !safeRepositoryName(name) {
		return "", "", fmt.Errorf("origin repository name %q is unsafe", name)
	}
	return "https://" + host + "/" + repositoryPath, name, nil
}

func trustedCanonicalSource(value string) (string, string, error) {
	canonical, repository, err := canonicalSourceURL(value)
	if err != nil {
		return "", "", fmt.Errorf("parse trusted canonical source: %w", err)
	}
	if value != canonical {
		return "", "", fmt.Errorf("trusted canonical source %q must use exact HTTPS form %q", value, canonical)
	}
	return canonical, repository, nil
}

func parseGitTree(data []byte) ([]sourceEntry, error) {
	if len(data) == 0 || len(data) > maxGitTreeBytes {
		return nil, errors.New("trusted library source tree is empty or exceeds limits")
	}
	records := bytes.Split(bytes.TrimSuffix(data, []byte{0}), []byte{0})
	entries := make([]sourceEntry, 0, len(records))
	portable := make(map[string]string, len(records))
	for index, record := range records {
		metadata, name, found := bytes.Cut(record, []byte{'\t'})
		fields := bytes.Fields(metadata)
		if !found || len(fields) != 3 || string(fields[1]) != "blob" {
			return nil, fmt.Errorf("trusted library tree entry %d has unsupported metadata", index)
		}
		filename := string(name)
		if err := validateArchivePath(filename); err != nil {
			return nil, fmt.Errorf("trusted library tree: %w", err)
		}
		entry := sourceEntry{path: filename, objectID: string(fields[2])}
		switch string(fields[0]) {
		case "100644":
			entry.mode = 0o644
		case "100755":
			entry.mode = 0o755
		case "120000":
			entry.mode = 0o777
		default:
			return nil, fmt.Errorf("trusted library path %q has unsupported mode %q", filename, fields[0])
		}
		if err := validateGitObjectID(entry.objectID); err != nil {
			return nil, fmt.Errorf("trusted library path %q: %w", filename, err)
		}
		key := strings.ToLower(filename)
		if prior, found := portable[key]; found {
			return nil, fmt.Errorf("trusted library paths %q and %q collide", prior, filename)
		}
		portable[key] = filename
		entries = append(entries, entry)
	}
	if len(entries) > maxArchiveEntries {
		return nil, errors.New("trusted library source tree exceeds entry limits")
	}
	slices.SortFunc(entries, func(left, right sourceEntry) int {
		return strings.Compare(left.path, right.path)
	})
	return entries, nil
}

func validateGitObjectID(objectID string) error {
	if len(objectID) != 40 && len(objectID) != 64 {
		return fmt.Errorf("git object ID %q has unsupported length", objectID)
	}
	if _, err := hex.DecodeString(objectID); err != nil {
		return fmt.Errorf("git object ID %q is invalid: %w", objectID, err)
	}
	return nil
}

func readGitBlobs(ctx context.Context, repository string, entries []sourceEntry) error {
	var requests strings.Builder
	for _, entry := range entries {
		requests.WriteString(entry.objectID)
		requests.WriteByte('\n')
	}
	// #nosec G204 -- the executable and arguments are fixed; Git object IDs
	// came from a validated exact tree and no shell is involved.
	command := exec.CommandContext(ctx, "git", "cat-file", "--batch")
	command.Dir = repository
	command.Env = append(gitenv.ReadOnly(os.Environ()), "GIT_NO_LAZY_FETCH=1")
	command.Stdin = strings.NewReader(requests.String())
	stdout, err := command.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open trusted library Git blob stream: %w", err)
	}
	var stderr limitedBuffer
	stderr.maximum = maxGitDiagnosticBytes
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return fmt.Errorf("start trusted library Git blob stream: %w", err)
	}
	reader := bufio.NewReader(stdout)
	var total int64
	for index := range entries {
		data, err := readGitBlob(reader, entries[index].objectID)
		if err != nil {
			return stopGitCommand(command, err)
		}
		total += int64(len(data))
		if total > rendererV1MaxSourceExpandedBytes {
			return stopGitCommand(command, errors.New("trusted library source tree exceeds expanded-size limit"))
		}
		if entries[index].mode == 0o777 {
			entries[index].linkTarget = string(data)
			if !safeLinkTarget(entries[index].path, entries[index].linkTarget) {
				return stopGitCommand(command, fmt.Errorf("trusted library symlink %q has unsafe target", entries[index].path))
			}
		} else {
			entries[index].data = data
		}
	}
	if err := command.Wait(); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return fmt.Errorf("read trusted library Git blobs: %w", contextErr)
		}
		return fmt.Errorf("read trusted library Git blobs: %w: %s", err, stderr.String())
	}
	return nil
}

func readGitBlob(reader *bufio.Reader, objectID string) ([]byte, error) {
	header, err := reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("read Git blob %s header: %w", objectID, err)
	}
	fields := strings.Fields(header)
	if len(fields) != 3 || fields[0] != objectID || fields[1] != "blob" {
		return nil, fmt.Errorf("git blob %s returned invalid metadata", objectID)
	}
	size, sizeErr := strconv.ParseInt(fields[2], 10, 64)
	if sizeErr != nil || size < 0 || size > rendererV1MaxSourceEntryBytes {
		return nil, fmt.Errorf("git blob %s exceeds entry-size limit", objectID)
	}
	data := make([]byte, int(size))
	if _, readErr := io.ReadFull(reader, data); readErr != nil {
		return nil, fmt.Errorf("read Git blob %s: %w", objectID, readErr)
	}
	terminator, terminatorErr := reader.ReadByte()
	if terminatorErr != nil || terminator != '\n' {
		return nil, fmt.Errorf("git blob %s has no batch terminator", objectID)
	}
	return data, nil
}

func stopGitCommand(command *exec.Cmd, cause error) error {
	killErr := command.Process.Kill()
	if errors.Is(killErr, os.ErrProcessDone) {
		killErr = nil
	}
	waitErr := command.Wait()
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		waitErr = nil
	}
	return errors.Join(cause, killErr, waitErr)
}

func readSourceArchive(ctx context.Context, data []byte, epoch time.Time) ([]archiveEntry, error) {
	if len(data) == 0 || len(data) > maxArtifactBytes {
		return nil, errors.New("source archive is empty or exceeds the size limit")
	}
	buffered := bufio.NewReader(bytes.NewReader(data))
	gzipReader, gzipErr := gzip.NewReader(buffered)
	if gzipErr != nil {
		return nil, fmt.Errorf("open gzip: %w", gzipErr)
	}
	gzipReader.Multistream(false)
	if !gzipReader.ModTime.Equal(epoch) || gzipReader.OS != 255 ||
		gzipReader.Name != "" || gzipReader.Comment != "" {
		return nil, closeGzip(gzipReader, errors.New("gzip header has noncanonical metadata"))
	}
	tarReader := tar.NewReader(gzipReader)
	entries := make([]archiveEntry, 0)
	seen := make(map[string]struct{})
	var expanded int64
	for {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, closeGzip(gzipReader, fmt.Errorf("read source archive: %w", contextErr))
		}
		header, headerErr := tarReader.Next()
		if errors.Is(headerErr, io.EOF) {
			break
		}
		if headerErr != nil {
			return nil, closeGzip(gzipReader, fmt.Errorf("read tar header: %w", headerErr))
		}
		if len(entries) >= maxArchiveEntries {
			return nil, closeGzip(gzipReader, errors.New("source archive exceeds entry limits"))
		}
		if pathErr := validateArchivePath(header.Name); pathErr != nil {
			return nil, closeGzip(gzipReader, pathErr)
		}
		if _, duplicate := seen[header.Name]; duplicate {
			return nil, closeGzip(gzipReader, fmt.Errorf("source archive contains duplicate path %q", header.Name))
		}
		seen[header.Name] = struct{}{}
		if header.Format != tar.FormatPAX || !header.ModTime.Equal(epoch) ||
			!header.AccessTime.Equal(epoch) || !header.ChangeTime.Equal(epoch) ||
			header.Uid != 0 || header.Gid != 0 || header.Uname != "" || header.Gname != "" {
			return nil, closeGzip(gzipReader, fmt.Errorf("source archive entry %q has noncanonical metadata", header.Name))
		}
		if metadataErr := validateRendererV1TarMetadata(header, epoch); metadataErr != nil {
			return nil, closeGzip(gzipReader, metadataErr)
		}
		if header.Mode < 0 || header.Mode > 0o777 {
			return nil, closeGzip(gzipReader, fmt.Errorf("source archive entry %q has invalid mode", header.Name))
		}
		entry := archiveEntry{name: header.Name, mode: os.FileMode(header.Mode)}
		switch header.Typeflag {
		case tar.TypeReg:
			if header.Size < 0 || header.Size > rendererV1MaxSourceEntryBytes ||
				expanded+header.Size > rendererV1MaxSourceExpandedBytes {
				return nil, closeGzip(gzipReader, fmt.Errorf("source archive entry %q exceeds limits", header.Name))
			}
			expanded += header.Size
			entry.data, headerErr = io.ReadAll(io.LimitReader(tarReader, rendererV1MaxSourceEntryBytes+1))
			if headerErr != nil {
				return nil, closeGzip(gzipReader, fmt.Errorf("read source archive entry %q: %w", header.Name, headerErr))
			}
			if int64(len(entry.data)) != header.Size {
				return nil, closeGzip(gzipReader, fmt.Errorf("source archive entry %q has an invalid expanded size", header.Name))
			}
		case tar.TypeSymlink:
			if header.Size != 0 || !safeLinkTarget(header.Name, header.Linkname) {
				return nil, closeGzip(gzipReader, fmt.Errorf("source archive symlink %q has unsafe metadata", header.Name))
			}
			entry.linkTarget = header.Linkname
		default:
			return nil, closeGzip(gzipReader, fmt.Errorf("source archive entry %q has unsupported type", header.Name))
		}
		entries = append(entries, entry)
	}
	trailing, err := io.ReadAll(io.LimitReader(gzipReader, 1))
	if err != nil {
		return nil, closeGzip(gzipReader, fmt.Errorf("finish source gzip: %w", err))
	}
	if len(trailing) != 0 {
		return nil, closeGzip(gzipReader, errors.New("source archive has hidden decompressed data after the tar end markers"))
	}
	if err := gzipReader.Close(); err != nil {
		return nil, fmt.Errorf("close source gzip: %w", err)
	}
	if _, err := buffered.Peek(1); !errors.Is(err, io.EOF) {
		return nil, errors.New("source archive has trailing or concatenated data")
	}
	return entries, nil
}

func validateRendererV1TarMetadata(header *tar.Header, epoch time.Time) error {
	wantPAX := map[string]string{
		"atime": strconv.FormatInt(epoch.Unix(), 10),
		"ctime": strconv.FormatInt(epoch.Unix(), 10),
	}
	if !asciiTarValue(header.Name) || len(header.Name) > 100 {
		wantPAX["path"] = header.Name
	}
	if header.Typeflag == tar.TypeSymlink &&
		(!asciiTarValue(header.Linkname) || len(header.Linkname) > 100) {
		wantPAX["linkpath"] = header.Linkname
	}
	if len(header.PAXRecords) != len(wantPAX) {
		return fmt.Errorf("source archive entry %q has noncanonical PAX metadata", header.Name)
	}
	for key, want := range wantPAX {
		if header.PAXRecords[key] != want {
			return fmt.Errorf("source archive entry %q has noncanonical PAX metadata", header.Name)
		}
	}
	//nolint:staticcheck // Reject archive/tar's legacy Xattrs view as part of the wire-format boundary.
	if len(header.Xattrs) != 0 || header.Devmajor != 0 || header.Devminor != 0 {
		return fmt.Errorf("source archive entry %q has noncanonical extended metadata", header.Name)
	}
	if header.Typeflag == tar.TypeReg && header.Linkname != "" {
		return fmt.Errorf("source archive regular file %q has a link target", header.Name)
	}
	return nil
}

func asciiTarValue(value string) bool {
	for index := range len(value) {
		if value[index] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

func closeGzip(reader *gzip.Reader, cause error) error {
	return errors.Join(cause, reader.Close())
}

func verifySourceArchive(
	actual []archiveEntry,
	base string,
	expected []sourceEntry,
) (map[string][]byte, error) {
	if len(actual) != len(expected) {
		return nil, fmt.Errorf("source archive has %d entries, require %d", len(actual), len(expected))
	}
	files := make(map[string][]byte, len(actual))
	for index, want := range expected {
		got := actual[index]
		name := path.Join(base, want.path)
		if got.name != name || got.mode != want.mode || got.linkTarget != want.linkTarget ||
			!bytes.Equal(got.data, want.data) {
			return nil, fmt.Errorf("source archive entry %d does not match trusted path %q", index, want.path)
		}
		if want.linkTarget == "" {
			files[want.path] = slices.Clone(want.data)
		}
	}
	for _, required := range []string{
		"LICENSE", "README.md", "go.mod", "go.sum", "spice-compatibility.json", "vendor/modules.txt",
	} {
		if _, found := files[required]; !found {
			return nil, fmt.Errorf("source archive is missing required file %q", required)
		}
	}
	return files, nil
}

func validateArchivePath(name string) error {
	if name == "" || !utf8.ValidString(name) || strings.ContainsRune(name, 0) ||
		strings.Contains(name, "\\") || path.IsAbs(name) || path.Clean(name) != name ||
		name == "." || !filepath.IsLocal(filepath.FromSlash(name)) {
		return fmt.Errorf("archive path %q is unsafe", name)
	}
	for component := range strings.SplitSeq(name, "/") {
		if !portablePathComponent(component) {
			return fmt.Errorf("archive path %q is not portable", name)
		}
	}
	return nil
}

func portablePathComponent(component string) bool {
	if component == "" || component == "." || component == ".." ||
		strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") {
		return false
	}
	for _, character := range component {
		if character < 0x20 || character == 0x7f || strings.ContainsRune(`<>:"|?*`, character) {
			return false
		}
	}
	base, _, _ := strings.Cut(component, ".")
	switch strings.ToUpper(base) {
	case "CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		return false
	default:
		return true
	}
}

func safeLinkTarget(name, target string) bool {
	if target == "" || !utf8.ValidString(target) || strings.ContainsRune(target, 0) ||
		strings.ContainsAny(target, "\\\r\n") || path.IsAbs(target) || path.Clean(target) != target {
		return false
	}
	for component := range strings.SplitSeq(target, "/") {
		if component == "." || component == ".." {
			continue
		}
		if !portablePathComponent(component) {
			return false
		}
	}
	resolved := path.Clean(path.Join(path.Dir(name), target))
	return resolved != ".." && !strings.HasPrefix(resolved, "../") && !path.IsAbs(resolved)
}

func gitOutput(
	ctx context.Context,
	repository string,
	maximum int,
	arguments ...string,
) ([]byte, error) {
	if len(arguments) == 0 {
		return nil, errors.New("run trusted library Git command: no arguments")
	}
	// #nosec G204 -- arguments are fixed except for an exact validated object ID.
	command := exec.CommandContext(ctx, "git", arguments...)
	command.Dir = repository
	command.Env = append(gitenv.ReadOnly(os.Environ()), "GIT_NO_LAZY_FETCH=1")
	var stdout, stderr limitedBuffer
	stdout.maximum = maximum
	stderr.maximum = maxGitDiagnosticBytes
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", arguments[0], err, stderr.String())
	}
	if stdout.truncated {
		return nil, fmt.Errorf("git %s output exceeds %d bytes", arguments[0], maximum)
	}
	return slices.Clone(stdout.Bytes()), nil
}

type limitedBuffer struct {
	bytes.Buffer
	maximum   int
	truncated bool
}

func (buffer *limitedBuffer) Write(content []byte) (int, error) {
	written := len(content)
	remaining := buffer.maximum - buffer.Len()
	if remaining <= 0 {
		buffer.truncated = true
		return written, nil
	}
	if len(content) > remaining {
		content = content[:remaining]
		buffer.truncated = true
	}
	_, _ = buffer.Buffer.Write(content)
	return written, nil
}

func (buffer *limitedBuffer) String() string {
	return strings.TrimSpace(buffer.Buffer.String())
}
