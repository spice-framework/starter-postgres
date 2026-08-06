package libraryrelease

import (
	"bytes"
	"crypto/ed25519"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

const maximumSigningKeyBytes = 64 << 10

// SigningFiles identifies caller-controlled signing material. Private key
// contents never enter a release plan, command argument, result, or artifact.
type SigningFiles struct {
	PrivateKey       string
	TrustedPublicKey string
}

type signingMaterial struct {
	privateKey ed25519.PrivateKey
	publicKey  ed25519.PublicKey
	publicPEM  []byte
}

func loadSigningMaterial(files SigningFiles) (signingMaterial, error) {
	privateData, err := readSigningFile(files.PrivateKey, maximumSigningKeyBytes, true)
	if err != nil {
		clear(privateData)
		return signingMaterial{}, fmt.Errorf("read release private key: %w", err)
	}
	defer clear(privateData)
	privateKey, err := parsePrivateKey(privateData)
	if err != nil {
		return signingMaterial{}, err
	}
	keepPrivate := false
	defer func() {
		if !keepPrivate {
			clear(privateKey)
		}
	}()

	publicData, err := readSigningFile(files.TrustedPublicKey, maximumSigningKeyBytes, false)
	if err != nil {
		return signingMaterial{}, fmt.Errorf("read trusted release public key: %w", err)
	}
	publicKey, err := parsePublicKey(publicData)
	if err != nil {
		return signingMaterial{}, err
	}
	derived, ok := privateKey.Public().(ed25519.PublicKey)
	if !ok || subtle.ConstantTimeCompare(derived, publicKey) != 1 {
		return signingMaterial{}, errors.New("release private key does not match trusted public key")
	}
	keepPrivate = true
	return signingMaterial{
		privateKey: privateKey,
		publicKey:  append(ed25519.PublicKey(nil), publicKey...),
		publicPEM:  append([]byte(nil), publicData...),
	}, nil
}

func (material *signingMaterial) clear() {
	if material == nil {
		return
	}
	clear(material.privateKey)
	clear(material.publicKey)
	clear(material.publicPEM)
	*material = signingMaterial{}
}

func (material signingMaterial) sign(checksums []byte) ([]byte, []byte, error) {
	if len(material.privateKey) != ed25519.PrivateKeySize ||
		len(material.publicKey) != ed25519.PublicKeySize || len(material.publicPEM) == 0 {
		return nil, nil, errors.New("release signing material is incomplete")
	}
	signature := ed25519.Sign(material.privateKey, checksums)
	if len(signature) != ed25519.SignatureSize ||
		!ed25519.Verify(material.publicKey, checksums, signature) {
		return nil, nil, errors.New("verify release checksum signature")
	}
	return signature, append([]byte(nil), material.publicPEM...), nil
}

func readSigningFile(filename string, maximum int64, private bool) (_ []byte, resultErr error) {
	if filename == "" {
		return nil, errors.New("file path is required")
	}
	absolute, err := filepath.Abs(filename)
	if err != nil {
		return nil, fmt.Errorf("resolve file: %w", err)
	}
	root, err := os.OpenRoot(filepath.Dir(absolute))
	if err != nil {
		return nil, fmt.Errorf("open file parent: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	name := filepath.Base(absolute)
	info, err := root.Lstat(name)
	if err != nil {
		return nil, fmt.Errorf("inspect file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maximum {
		return nil, fmt.Errorf("file must be regular and bounded to %d bytes", maximum)
	}
	if private && runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("private key file must not grant group or other permissions")
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, fmt.Errorf("open file: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect open file: %w", err)
	}
	if !openedInfo.Mode().IsRegular() || openedInfo.Size() < 0 || openedInfo.Size() > maximum ||
		!os.SameFile(info, openedInfo) {
		return nil, errors.New("file changed while it was opened")
	}
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	if int64(len(data)) > maximum {
		clear(data)
		return nil, fmt.Errorf("file exceeds %d bytes", maximum)
	}
	return data, nil
}

func parsePrivateKey(data []byte) (ed25519.PrivateKey, error) {
	if len(data) == 0 {
		return nil, errors.New("parse release signing key: key is empty")
	}
	if block, rest := pem.Decode(data); block != nil {
		defer clear(block.Bytes)
		if block.Type != "PRIVATE KEY" || len(block.Headers) != 0 || len(rest) != 0 {
			return nil, errors.New("parse release signing key: require one canonical PRIVATE KEY PEM block")
		}
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse PKCS#8 release signing key: %w", err)
		}
		key, ok := parsed.(ed25519.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("parse release signing key: require Ed25519, got %T", parsed)
		}
		defer clear(key)
		validated, err := validatedPrivateKey(key)
		if err != nil {
			return nil, err
		}
		encoded, err := x509.MarshalPKCS8PrivateKey(validated)
		if err != nil {
			clear(validated)
			return nil, fmt.Errorf("encode canonical release signing key: %w", err)
		}
		defer clear(encoded)
		canonical := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})
		defer clear(canonical)
		if !bytes.Equal(data, canonical) {
			clear(validated)
			return nil, errors.New("parse release signing key: PRIVATE KEY PEM is not canonical")
		}
		return validated, nil
	}
	decoded := make([]byte, base64.StdEncoding.DecodedLen(len(data)))
	decodedLength, err := base64.StdEncoding.Decode(decoded, data)
	if err != nil {
		clear(decoded)
		return nil, fmt.Errorf("parse release signing key: require canonical PKCS#8 PEM or base64: %w", err)
	}
	decoded = decoded[:decodedLength]
	defer clear(decoded)
	canonical := make([]byte, base64.StdEncoding.EncodedLen(len(decoded)))
	defer clear(canonical)
	base64.StdEncoding.Encode(canonical, decoded)
	if !bytes.Equal(canonical, data) {
		return nil, errors.New("parse release signing key: base64 is not canonical")
	}
	switch len(decoded) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(decoded), nil
	case ed25519.PrivateKeySize:
		return validatedPrivateKey(ed25519.PrivateKey(decoded))
	default:
		return nil, fmt.Errorf(
			"parse release signing key: decoded length %d, require %d-byte seed or %d-byte private key",
			len(decoded),
			ed25519.SeedSize,
			ed25519.PrivateKeySize,
		)
	}
}

func validatedPrivateKey(key ed25519.PrivateKey) (ed25519.PrivateKey, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf(
			"parse release signing key: private key length is %d, require %d",
			len(key),
			ed25519.PrivateKeySize,
		)
	}
	derived := ed25519.NewKeyFromSeed(key[:ed25519.SeedSize])
	defer clear(derived)
	if subtle.ConstantTimeCompare(key, derived) != 1 {
		return nil, errors.New("parse release signing key: public key does not match private seed")
	}
	return append(ed25519.PrivateKey(nil), key...), nil
}

func parsePublicKey(data []byte) (ed25519.PublicKey, error) {
	if len(data) == 0 || len(data) > maximumSigningKeyBytes {
		return nil, errors.New("parse trusted release public key: invalid size")
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "PUBLIC KEY" || len(block.Headers) != 0 || len(rest) != 0 {
		return nil, errors.New("parse trusted release public key: require one canonical PUBLIC KEY PEM block")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse trusted release public key: %w", err)
	}
	key, ok := parsed.(ed25519.PublicKey)
	if !ok || len(key) != ed25519.PublicKeySize {
		return nil, errors.New("parse trusted release public key: require Ed25519 public key")
	}
	encoded, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return nil, fmt.Errorf("encode canonical trusted release public key: %w", err)
	}
	canonical := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: encoded})
	if !bytes.Equal(data, canonical) {
		return nil, errors.New("parse trusted release public key: PEM is not canonical")
	}
	return append(ed25519.PublicKey(nil), key...), nil
}
