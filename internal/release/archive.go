package release

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"time"
)

type archiveEntry struct {
	name     string
	mode     int64
	data     []byte
	linkname string
}

func writeSourceArchive(filename string, epoch time.Time, entries []archiveEntry) error {
	root, err := os.OpenRoot(filepath.Dir(filename))
	if err != nil {
		return fmt.Errorf("open release archive root: %w", err)
	}
	file, err := root.OpenFile(filepath.Base(filename), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.Join(fmt.Errorf("create release archive %q: %w", filename, err), root.Close())
	}
	writeErr := writeTarGzip(file, epoch, entries)
	closeErr := file.Close()
	return errors.Join(writeErr, closeErr, root.Close())
}

func writeTarGzip(output io.Writer, epoch time.Time, entries []archiveEntry) error {
	gzipWriter, err := gzip.NewWriterLevel(output, gzip.BestCompression)
	if err != nil {
		return fmt.Errorf("construct gzip writer: %w", err)
	}
	gzipWriter.ModTime = epoch
	gzipWriter.OS = 255
	tarWriter := tar.NewWriter(gzipWriter)
	for _, entry := range entries {
		typeflag := byte(tar.TypeReg)
		size := int64(len(entry.data))
		if entry.linkname != "" {
			typeflag = tar.TypeSymlink
			size = 0
		}
		header := &tar.Header{
			Name:       path.Clean(entry.name),
			Mode:       entry.mode,
			Size:       size,
			ModTime:    epoch,
			AccessTime: epoch,
			ChangeTime: epoch,
			Typeflag:   typeflag,
			Linkname:   entry.linkname,
			Format:     tar.FormatPAX,
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			return errors.Join(fmt.Errorf("create tar entry %q: %w", entry.name, err), tarWriter.Close(), gzipWriter.Close())
		}
		if typeflag == tar.TypeReg {
			if _, err := tarWriter.Write(entry.data); err != nil {
				return errors.Join(fmt.Errorf("write tar entry %q: %w", entry.name, err), tarWriter.Close(), gzipWriter.Close())
			}
		}
	}
	if err := tarWriter.Close(); err != nil {
		return errors.Join(fmt.Errorf("close tar archive: %w", err), gzipWriter.Close())
	}
	if err := gzipWriter.Close(); err != nil {
		return fmt.Errorf("close gzip archive: %w", err)
	}
	return nil
}
