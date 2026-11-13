package ota

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Manifest represents the update package manifest.
type Manifest struct {
	Version    string    `json:"version"`
	Channel    string    `json:"channel"`
	Artifact   string    `json:"artifact"`
	SHA256     string    `json:"sha256"`
	CreatedAt  string    `json:"created_at,omitempty"`
	Notes      string    `json:"notes,omitempty"`
	Kind       string    `json:"kind,omitempty"` // "core" or "platform"
}

// ValidChannel returns true if the channel is valid.
func ValidChannel(c string) bool {
	return c == "stable" || c == "beta"
}

// ValidKind returns true if the kind is valid.
func ValidKind(k string) bool {
	return k == "core" || k == "platform" || k == ""
}

// NormalizeKind returns the normalized kind (defaults to "core").
func NormalizeKind(k string) string {
	if k == "platform" {
		return "platform"
	}
	return "core"
}

// LoadManifest reads and parses manifest.json from the given directory.
func LoadManifest(dir string) (*Manifest, error) {
	path := filepath.Join(dir, "manifest.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &m, nil
}

// VerifySignature verifies the ed25519 signature of the manifest.
func VerifySignature(manifestPath, sigPath string, pubKey ed25519.PublicKey) error {
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("read manifest for verify: %w", err)
	}
	sigData, err := os.ReadFile(sigPath)
	if err != nil {
		return fmt.Errorf("read signature: %w", err)
	}
	if len(sigData) != ed25519.SignatureSize {
		return errors.New("invalid signature length")
	}
	if !ed25519.Verify(pubKey, manifestData, sigData) {
		return errors.New("invalid signature")
	}
	return nil
}

// VerifyArtifactHash verifies the artifact's SHA256 matches the manifest.
func VerifyArtifactHash(artifactPath, expectedHex string) error {
	f, err := os.Open(artifactPath)
	if err != nil {
		return fmt.Errorf("open artifact: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hash artifact: %w", err)
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if actual != expectedHex {
		return fmt.Errorf("hash mismatch: expected %s, got %s", expectedHex, actual)
	}
	return nil
}

// DecodePublicKey decodes a base64-encoded ed25519 public key.
func DecodePublicKey(b64 string) (ed25519.PublicKey, error) {
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("decode base64: %w", err)
	}
	if len(data) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid public key length: got %d, want %d", len(data), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(data), nil
}

// LoadPublicKeyFromFile loads a base64-encoded ed25519 public key from a file.
func LoadPublicKeyFromFile(path string) (ed25519.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read public key file: %w", err)
	}
	return DecodePublicKey(string(data))
}

// PackageInfo holds validated package information.
type PackageInfo struct {
	Manifest *Manifest
	Dir      string
	ArtifactPath string
}

// ValidatePackage validates a package directory and returns PackageInfo.
func ValidatePackage(dir string, pubKey ed25519.PublicKey, activeChannel string) (*PackageInfo, error) {
	// Load manifest
	m, err := LoadManifest(dir)
	if err != nil {
		return nil, err
	}

	// Validate required fields
	if m.Version == "" {
		return nil, errors.New("manifest: version is required")
	}
	if !ValidChannel(m.Channel) {
		return nil, fmt.Errorf("manifest: invalid channel %q", m.Channel)
	}
	if m.Artifact == "" {
		return nil, errors.New("manifest: artifact is required")
	}
	if m.SHA256 == "" {
		return nil, errors.New("manifest: sha256 is required")
	}

	// Channel match
	if m.Channel != activeChannel {
		return nil, fmt.Errorf("channel_mismatch: package channel %q != active channel %q", m.Channel, activeChannel)
	}

	// Normalize kind
	m.Kind = NormalizeKind(m.Kind)

	// Verify signature
	sigPath := filepath.Join(dir, "manifest.sig")
	if err := VerifySignature(filepath.Join(dir, "manifest.json"), sigPath, pubKey); err != nil {
		return nil, fmt.Errorf("invalid_signature: %w", err)
	}

	// Verify artifact hash
	artifactPath := filepath.Join(dir, m.Artifact)
	if err := VerifyArtifactHash(artifactPath, m.SHA256); err != nil {
		return nil, fmt.Errorf("hash_mismatch: %w", err)
	}

	return &PackageInfo{
		Manifest:     m,
		Dir:          dir,
		ArtifactPath: artifactPath,
	}, nil
}

// ParseCreatedAt parses the created_at field as RFC3339.
func (m *Manifest) ParseCreatedAt() (time.Time, error) {
	if m.CreatedAt == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, m.CreatedAt)
}