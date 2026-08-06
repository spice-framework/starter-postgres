package libraryrelease

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spice-framework/development/internal/process"
)

const (
	maximumSourceTreeBytes = 16 << 20
	maximumSourceBlobBytes = 128 << 20
	maximumSourceDataBytes = 256 << 20
	maximumGitDiagnostic   = 32 << 10
)

type gitTreeEntry struct {
	mode string
	hash string
	name string
	data []byte
}

type committedTree struct {
	entries []gitTreeEntry
	files   map[string][]byte
}

func readCommittedTree(
	ctx context.Context,
	repositoryRoot string,
	commit string,
) (committedTree, error) {
	tree, err := gitBytes(ctx, repositoryRoot, maximumSourceTreeBytes, "ls-tree", "-rz", "--full-tree", commit)
	if err != nil {
		return committedTree{}, fmt.Errorf("list release source tree: %w", err)
	}
	entries, err := parseGitTree(tree)
	if err != nil {
		return committedTree{}, err
	}
	blobs, err := gitBlobs(ctx, repositoryRoot, entries)
	if err != nil {
		return committedTree{}, err
	}
	files := make(map[string][]byte, len(entries))
	for index := range entries {
		entries[index].data = blobs[index]
		files[entries[index].name] = blobs[index]
	}
	return committedTree{entries: entries, files: files}, nil
}

func validateCommitEpoch(
	ctx context.Context,
	repositoryRoot string,
	plan Plan,
) error {
	content, err := gitBytes(
		ctx,
		repositoryRoot,
		128,
		"show",
		"-s",
		"--format=%ct",
		plan.Commit,
	)
	if err != nil {
		return fmt.Errorf("read renderer commit epoch: %w", err)
	}
	epoch, err := strconv.ParseInt(strings.TrimSpace(string(content)), 10, 64)
	if err != nil || epoch != plan.SourceDateEpoch {
		return fmt.Errorf(
			"renderer commit epoch %q does not match plan %d",
			strings.TrimSpace(string(content)),
			plan.SourceDateEpoch,
		)
	}
	return nil
}

func parseGitTree(content []byte) ([]gitTreeEntry, error) {
	if len(content) == 0 {
		return nil, errors.New("release source commit has no tracked files")
	}
	if len(content) > maximumSourceTreeBytes {
		return nil, fmt.Errorf("release source tree exceeds %d bytes", maximumSourceTreeBytes)
	}
	records := bytes.Split(bytes.TrimSuffix(content, []byte{0}), []byte{0})
	entries := make([]gitTreeEntry, 0, len(records))
	seen := make(map[string]struct{}, len(records))
	portable := make(map[string]string, len(records))
	for _, record := range records {
		metadata, nameBytes, found := bytes.Cut(record, []byte{'\t'})
		fields := strings.Fields(string(metadata))
		name := string(nameBytes)
		if !found || len(fields) != 3 || fields[1] != "blob" {
			return nil, fmt.Errorf("release source has unsupported tree entry %q", record)
		}
		if err := validateArchivePath(name); err != nil {
			return nil, err
		}
		if !commitPattern.MatchString(fields[2]) {
			return nil, fmt.Errorf("release source object %q is not a full object ID", fields[2])
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("release source path %q is duplicated", name)
		}
		seen[name] = struct{}{}
		key := strings.ToLower(name)
		if prior, collision := portable[key]; collision {
			left, right := prior, name
			if strings.Compare(left, right) > 0 {
				left, right = right, left
			}
			return nil, fmt.Errorf(
				"release source paths %q and %q collide on case-insensitive filesystems",
				left,
				right,
			)
		}
		portable[key] = name
		entries = append(entries, gitTreeEntry{mode: fields[0], hash: fields[2], name: name})
	}
	slices.SortFunc(entries, func(left, right gitTreeEntry) int {
		return strings.Compare(left.name, right.name)
	})
	return entries, nil
}

func gitBlobs(
	ctx context.Context,
	repositoryRoot string,
	entries []gitTreeEntry,
) ([][]byte, error) {
	var input strings.Builder
	for _, entry := range entries {
		input.WriteString(entry.hash)
		input.WriteByte('\n')
	}
	// #nosec G204 -- executable and arguments are fixed; object IDs came from a
	// validated Git tree and no shell is involved.
	command := exec.CommandContext(ctx, "git", "cat-file", "--batch")
	command.Dir = repositoryRoot
	command.Env = process.IndependentEnvironment()
	command.Stdin = strings.NewReader(input.String())
	var stdout limitedBuffer
	stdout.maximum = maximumSourceDataBytes
	var stderr limitedBuffer
	stderr.maximum = maximumGitDiagnostic
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("read committed release objects: %w: %s", err, stderr.String())
	}
	if stdout.truncated {
		return nil, fmt.Errorf("committed release objects exceed %d bytes", maximumSourceDataBytes)
	}
	return parseGitBlobs(stdout.Bytes(), entries)
}

func parseGitBlobs(content []byte, entries []gitTreeEntry) ([][]byte, error) {
	reader := bufio.NewReader(bytes.NewReader(content))
	result := make([][]byte, 0, len(entries))
	for _, entry := range entries {
		header, err := reader.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("read release object %q header: %w", entry.name, err)
		}
		fields := strings.Fields(header)
		if len(fields) != 3 || fields[0] != entry.hash || fields[1] != "blob" {
			return nil, fmt.Errorf("release object %q has invalid header %q", entry.name, strings.TrimSpace(header))
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || size < 0 || size > maximumSourceBlobBytes {
			return nil, fmt.Errorf("release object %q has invalid size %q", entry.name, fields[2])
		}
		data := make([]byte, size)
		if _, err := io.ReadFull(reader, data); err != nil {
			return nil, fmt.Errorf("read release object %q: %w", entry.name, err)
		}
		terminator, err := reader.ReadByte()
		if err != nil || terminator != '\n' {
			return nil, fmt.Errorf("release object %q has invalid terminator", entry.name)
		}
		result = append(result, data)
	}
	if _, err := reader.ReadByte(); !errors.Is(err, io.EOF) {
		return nil, errors.New("committed release objects have trailing output")
	}
	return result, nil
}

func buildSourceArchive(plan Plan, entries []gitTreeEntry) ([]byte, error) {
	epoch := time.Unix(plan.SourceDateEpoch, 0).UTC()
	prefix := plan.Repository + "_" + strings.TrimPrefix(plan.Version, "v") + "/"
	return buildSourceArchiveWithPrefix(prefix, epoch, entries)
}

func buildSourceArchiveWithPrefix(
	prefix string,
	epoch time.Time,
	entries []gitTreeEntry,
) ([]byte, error) {
	var output bytes.Buffer
	gzipWriter, err := gzip.NewWriterLevel(&output, gzip.BestCompression)
	if err != nil {
		return nil, fmt.Errorf("construct release gzip writer: %w", err)
	}
	gzipWriter.ModTime = epoch
	gzipWriter.OS = 255
	tarWriter := tar.NewWriter(gzipWriter)
	for _, entry := range entries {
		header, content, err := archiveEntry(prefix, epoch, entry)
		if err != nil {
			return nil, errors.Join(err, tarWriter.Close(), gzipWriter.Close())
		}
		if err := tarWriter.WriteHeader(&header); err != nil {
			return nil, errors.Join(fmt.Errorf("write release header %q: %w", entry.name, err), tarWriter.Close(), gzipWriter.Close())
		}
		if _, err := tarWriter.Write(content); err != nil {
			return nil, errors.Join(fmt.Errorf("write release content %q: %w", entry.name, err), tarWriter.Close(), gzipWriter.Close())
		}
	}
	if err := tarWriter.Close(); err != nil {
		return nil, errors.Join(fmt.Errorf("close release tar: %w", err), gzipWriter.Close())
	}
	if err := gzipWriter.Close(); err != nil {
		return nil, fmt.Errorf("close release gzip: %w", err)
	}
	return output.Bytes(), nil
}

func archiveEntry(prefix string, epoch time.Time, entry gitTreeEntry) (tar.Header, []byte, error) {
	if err := validateArchivePath(entry.name); err != nil {
		return tar.Header{}, nil, err
	}
	header := tar.Header{
		Name: prefix + entry.name, ModTime: epoch, AccessTime: epoch,
		ChangeTime: epoch, Format: tar.FormatPAX,
	}
	content := entry.data
	switch entry.mode {
	case "100644":
		header.Mode, header.Typeflag, header.Size = 0o644, tar.TypeReg, int64(len(content))
	case "100755":
		header.Mode, header.Typeflag, header.Size = 0o755, tar.TypeReg, int64(len(content))
	case "120000":
		header.Mode, header.Typeflag, header.Linkname = 0o777, tar.TypeSymlink, string(content)
		header.Size, content = 0, nil
		if err := validateSymlink(entry.name, header.Linkname); err != nil {
			return tar.Header{}, nil, err
		}
	default:
		return tar.Header{}, nil, fmt.Errorf("release source uses unsupported Git mode %q for %q", entry.mode, entry.name)
	}
	return header, content, nil
}

func validateArchivePath(name string) error {
	clean := path.Clean(name)
	if name == "" || clean == "." || clean == ".." || clean != name ||
		strings.HasPrefix(clean, "../") || path.IsAbs(clean) || strings.Contains(name, "\\") {
		return fmt.Errorf("release source contains unsafe path %q", name)
	}
	if err := validatePortablePath(name, false); err != nil {
		return fmt.Errorf("release source contains unsafe path %q: %w", name, err)
	}
	return nil
}

func validateSymlink(name string, target string) error {
	if target == "" || path.IsAbs(target) || strings.Contains(target, "\\") ||
		path.Clean(target) != target {
		return fmt.Errorf("release symlink %q has unsafe target %q", name, target)
	}
	if err := validatePortablePath(target, true); err != nil {
		return fmt.Errorf("release symlink %q has unsafe target %q: %w", name, target, err)
	}
	resolved := path.Clean(path.Join(path.Dir(name), target))
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return fmt.Errorf("release symlink %q escapes archive root", name)
	}
	return nil
}

func validatePortablePath(name string, allowTraversal bool) error {
	if !utf8.ValidString(name) {
		return errors.New("path is not valid UTF-8")
	}
	for index := range len(name) {
		character := name[index]
		if character < 0x20 || character > 0x7e {
			return errors.New("path contains a byte outside printable ASCII")
		}
		if strings.ContainsRune(`<>:"|?*`, rune(character)) {
			return errors.New("path contains a character unsupported on Windows")
		}
	}
	for component := range strings.SplitSeq(name, "/") {
		if component == "." || component == ".." {
			if allowTraversal {
				continue
			}
			return errors.New("path contains a traversal component")
		}
		if component == "" || strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") {
			return errors.New("path contains an empty or non-portable component")
		}
		base, _, _ := strings.Cut(component, ".")
		switch strings.ToUpper(base) {
		case "CON", "PRN", "AUX", "NUL",
			"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
			"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
			return errors.New("path contains a Windows reserved device name")
		}
	}
	return nil
}

func gitBytes(
	ctx context.Context,
	repositoryRoot string,
	maximum int,
	arguments ...string,
) ([]byte, error) {
	// #nosec G204 -- executable is fixed; arguments are validated commit IDs and
	// repository-owned literals, and no shell is involved.
	command := exec.CommandContext(ctx, "git", arguments...)
	command.Dir = repositoryRoot
	command.Env = process.IndependentEnvironment()
	var stdout limitedBuffer
	stdout.maximum = maximum
	var stderr limitedBuffer
	stderr.maximum = maximumGitDiagnostic
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(arguments, " "), err, stderr.String())
	}
	if stdout.truncated {
		return nil, fmt.Errorf("git %s output exceeds %d bytes", strings.Join(arguments, " "), maximum)
	}
	return bytes.Clone(stdout.Bytes()), nil
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
