package release

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"strings"
)

func parsePrivateKey(data []byte) (ed25519.PrivateKey, error) {
	if block, _ := pem.Decode(data); block != nil {
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse PKCS#8 release signing key: %w", err)
		}
		key, ok := parsed.(ed25519.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("parse release signing key: require Ed25519, got %T", parsed)
		}
		return append(ed25519.PrivateKey(nil), key...), nil
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, fmt.Errorf("parse release signing key: require PKCS#8 PEM or base64: %w", err)
	}
	switch len(decoded) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(decoded), nil
	case ed25519.PrivateKeySize:
		return append(ed25519.PrivateKey(nil), decoded...), nil
	default:
		return nil, fmt.Errorf("parse release signing key: decoded length %d, require %d-byte seed or %d-byte private key", len(decoded), ed25519.SeedSize, ed25519.PrivateKeySize)
	}
}

func signChecksums(checksums, privateKeyData []byte) ([]byte, []byte, error) {
	privateKey, err := parsePrivateKey(privateKeyData)
	if err != nil {
		return nil, nil, err
	}
	signature := ed25519.Sign(privateKey, checksums)
	publicKey, ok := privateKey.Public().(ed25519.PublicKey)
	if !ok {
		return nil, nil, fmt.Errorf("encode release public key: unexpected key type")
	}
	encoded, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return nil, nil, fmt.Errorf("encode release public key: %w", err)
	}
	return signature, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: encoded}), nil
}
