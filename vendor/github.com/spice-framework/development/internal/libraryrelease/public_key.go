package libraryrelease

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// WritePublicKey derives and atomically publishes a canonical Ed25519 PKIX
// public-key PEM from caller-provided private signing material. It never
// creates private material and never replaces an existing output path.
func WritePublicKey(
	ctx context.Context,
	privateKeyFile string,
	outputFile string,
) (_ string, resultErr error) {
	if ctx == nil {
		return "", errors.New("derive release public key: context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("derive release public key: %w", err)
	}
	privateData, err := readSigningFile(privateKeyFile, maximumSigningKeyBytes, true)
	if err != nil {
		clear(privateData)
		return "", fmt.Errorf("read release private key: %w", err)
	}
	defer clear(privateData)
	privateKey, err := parsePrivateKey(privateData)
	if err != nil {
		return "", err
	}
	defer clear(privateKey)
	publicKey, ok := privateKey.Public().(ed25519.PublicKey)
	if !ok || len(publicKey) != ed25519.PublicKeySize {
		clear(publicKey)
		return "", errors.New("derive release public key: Ed25519 public key is invalid")
	}
	defer clear(publicKey)
	encoded, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return "", fmt.Errorf("encode release public key: %w", err)
	}
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: encoded})
	if len(publicPEM) == 0 {
		return "", errors.New("encode release public key: PEM encoding failed")
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("derive release public key: %w", err)
	}
	return writePublicKeyNoReplace(ctx, outputFile, publicPEM)
}

func writePublicKeyNoReplace(
	ctx context.Context,
	configuredOutput string,
	content []byte,
) (_ string, resultErr error) {
	if strings.TrimSpace(configuredOutput) == "" {
		return "", errors.New("release public-key output file is required")
	}
	output, err := filepath.Abs(configuredOutput)
	if err != nil {
		return "", fmt.Errorf("resolve release public-key output: %w", err)
	}
	parent := filepath.Dir(output)
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return "", fmt.Errorf("inspect release public-key output parent: %w", err)
	}
	if parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() {
		return "", fmt.Errorf("release public-key output parent %q is not a real directory", parent)
	}
	if err := rejectExistingPublicKeyOutput(output); err != nil {
		return "", err
	}
	stagingFile, err := os.CreateTemp(parent, ".spice-public-key-*")
	if err != nil {
		return "", fmt.Errorf("create release public-key staging file: %w", err)
	}
	staging := stagingFile.Name()
	closed := false
	defer func() {
		if !closed {
			resultErr = errors.Join(resultErr, stagingFile.Close())
		}
		if removeErr := os.Remove(staging); removeErr != nil &&
			!errors.Is(removeErr, os.ErrNotExist) {
			resultErr = errors.Join(resultErr, removeErr)
		}
	}()
	written, writeErr := stagingFile.Write(content)
	if writeErr == nil && written != len(content) {
		writeErr = io.ErrShortWrite
	}
	if err := errors.Join(writeErr, stagingFile.Sync(), stagingFile.Close()); err != nil {
		closed = true
		return "", fmt.Errorf("write release public-key staging file: %w", err)
	}
	closed = true
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("derive release public key: %w", err)
	}
	if err := renameNoReplace(staging, output); err != nil {
		if existingErr := rejectExistingPublicKeyOutput(output); existingErr != nil {
			return "", existingErr
		}
		return "", fmt.Errorf("commit release public key without replacement: %w", err)
	}
	return output, nil
}

func rejectExistingPublicKeyOutput(output string) error {
	info, err := os.Lstat(output)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect release public-key output: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("release public-key output %q is an existing symlink", output)
	}
	return fmt.Errorf("release public-key output %q already exists", output)
}
