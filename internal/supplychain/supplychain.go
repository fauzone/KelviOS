// Package supplychain implements the KelviOS Phase 1 supply-chain integrity
// core logic (FR-1.14, acceptance A1.8).
//
// Policy only, NO I/O, NO network/registry calls. The SignatureStatus input
// is how a later step feeds a real verifier's result in. No DB migration.
package supplychain

import (
	"errors"
	"regexp"
	"strings"
)

// SourceKind identifies the origin of an App Library entry.
type SourceKind string

const (
	SourceKindOfficial SourceKind = "official"
	SourceKindCustom   SourceKind = "custom"
)

// SignatureKind identifies the upstream signature mechanism.
type SignatureKind string

const (
	SignatureKindNone         SignatureKind = "none"
	SignatureKindCosign       SignatureKind = "cosign"
	SignatureKindContentTrust SignatureKind = "content-trust"
)

// SignatureStatus is the result of verifying an upstream signature.
type SignatureStatus string

const (
	SignatureStatusVerified    SignatureStatus = "verified"
	SignatureStatusFailed      SignatureStatus = "failed"
	SignatureStatusNotProvided SignatureStatus = "not_provided"
)

// Entry represents a single App Library catalog entry.
type Entry struct {
	Name                string        `json:"name"`
	Publisher           string        `json:"publisher"`
	SourceRepo          string        `json:"source_repo"`
	ImageRef            string        `json:"image_ref"`
	PinnedDigest        string        `json:"pinned_digest"`
	SignatureKind       SignatureKind `json:"signature_kind"`
	SourceKind          SourceKind    `json:"source_kind"`
	UnverifiedConfirmed bool          `json:"unverified_confirmed"`
}

// Decision is the install-gate result.
type Decision struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason"`
}

// Sentinel errors for ValidateEntry.
var (
	ErrMissingField         = errors.New("missing required field")
	ErrInvalidDigest        = errors.New("invalid pinned digest")
	ErrInvalidSignatureKind = errors.New("invalid signature kind")
	ErrInvalidSourceKind    = errors.New("invalid source kind")
)

var digestRegex = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// ValidDigest returns true iff s is exactly "sha256:" followed by 64
// lowercase hex characters. No uppercase, no other algorithm, no whitespace.
func ValidDigest(s string) bool {
	return digestRegex.MatchString(s)
}

// ValidateEntry validates an Entry. Checks run in this order:
// 1. Missing required fields (Name, Publisher, SourceRepo, ImageRef)
// 2. Invalid SourceKind
// 3. Invalid SignatureKind
// 4. Invalid PinnedDigest (must pass ValidDigest; floating tag never allowed)
// Returns the FIRST failing sentinel error only.
func ValidateEntry(e Entry) error {
	if strings.TrimSpace(e.Name) == "" ||
		strings.TrimSpace(e.Publisher) == "" ||
		strings.TrimSpace(e.SourceRepo) == "" ||
		strings.TrimSpace(e.ImageRef) == "" {
		return ErrMissingField
	}
	if e.SourceKind != SourceKindOfficial && e.SourceKind != SourceKindCustom {
		return ErrInvalidSourceKind
	}
	if e.SignatureKind != SignatureKindNone &&
		e.SignatureKind != SignatureKindCosign &&
		e.SignatureKind != SignatureKindContentTrust {
		return ErrInvalidSignatureKind
	}
	if !ValidDigest(e.PinnedDigest) {
		return ErrInvalidDigest
	}
	return nil
}

// VerifyInstall is the A1.8 install gate. It NEVER creates anything, only
// decides. Evaluation order (first match wins):
// 1. servedDigest fails ValidDigest -> refuse, reason "invalid_served_digest"
// 2. servedDigest != e.PinnedDigest -> refuse, reason "digest_mismatch"
// 3. sig == failed -> refuse, reason "signature_failed" (applies to every source kind)
// 4. e.SignatureKind != none && sig != verified -> refuse, reason "signature_missing"
// 5. e.SourceKind == custom && !e.UnverifiedConfirmed -> refuse, reason "unverified_unconfirmed"
// 6. Otherwise -> allow, reason ""
func VerifyInstall(e Entry, servedDigest string, sig SignatureStatus) Decision {
	if !ValidDigest(servedDigest) {
		return Decision{Allowed: false, Reason: "invalid_served_digest"}
	}
	if servedDigest != e.PinnedDigest {
		return Decision{Allowed: false, Reason: "digest_mismatch"}
	}
	if sig == SignatureStatusFailed {
		return Decision{Allowed: false, Reason: "signature_failed"}
	}
	if e.SignatureKind != SignatureKindNone && sig != SignatureStatusVerified {
		return Decision{Allowed: false, Reason: "signature_missing"}
	}
	if e.SourceKind == SourceKindCustom && !e.UnverifiedConfirmed {
		return Decision{Allowed: false, Reason: "unverified_unconfirmed"}
	}
	return Decision{Allowed: true, Reason: ""}
}
