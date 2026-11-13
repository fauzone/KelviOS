// Package supplychain tests for KelviOS Phase 1 WS18 Langkah-A (FR-1.14, A1.8).
package supplychain

import (
	"errors"
	"strings"
	"testing"
)

var (
	validDigestA = "sha256:" + strings.Repeat("a", 64)
	validDigestB = "sha256:" + strings.Repeat("b", 64)
)

func TestValidDigest(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{"valid lowercase", validDigestA, true},
		{"uppercase hex rejected", "sha256:" + strings.Repeat("A", 64), false},
		{"63 chars rejected", "sha256:" + strings.Repeat("a", 63), false},
		{"65 chars rejected", "sha256:" + strings.Repeat("a", 65), false},
		{"sha512 rejected", "sha512:" + strings.Repeat("a", 128), false},
		{"bare tag latest rejected", "latest", false},
		{"empty rejected", "", false},
		{"whitespace rejected", " sha256:" + strings.Repeat("a", 64), false},
		{"trailing whitespace rejected", "sha256:" + strings.Repeat("a", 64) + " ", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidDigest(tc.input); got != tc.expected {
				t.Fatalf("ValidDigest(%q) = %v, want %v", tc.input, got, tc.expected)
			}
		})
	}
}

func TestValidateEntry(t *testing.T) {
	baseOfficial := Entry{
		Name:                "test-app",
		Publisher:           "publisher",
		SourceRepo:          "https://github.com/org/repo",
		ImageRef:            "registry.io/app:latest",
		PinnedDigest:        validDigestA,
		SignatureKind:       SignatureKindNone,
		SourceKind:          SourceKindOfficial,
		UnverifiedConfirmed: false,
	}
	baseCustom := baseOfficial
	baseCustom.SourceKind = SourceKindCustom
	baseCustom.UnverifiedConfirmed = true

	tests := []struct {
		name      string
		entry     Entry
		wantErr   error
		wantErrIs error // for errors.Is check
	}{
		{"valid official", baseOfficial, nil, nil},
		{"valid custom confirmed", baseCustom, nil, nil},
		{"missing name", Entry{Name: "", Publisher: "p", SourceRepo: "r", ImageRef: "i", PinnedDigest: validDigestA, SignatureKind: SignatureKindNone, SourceKind: SourceKindOfficial}, ErrMissingField, ErrMissingField},
		{"missing publisher", Entry{Name: "n", Publisher: "", SourceRepo: "r", ImageRef: "i", PinnedDigest: validDigestA, SignatureKind: SignatureKindNone, SourceKind: SourceKindOfficial}, ErrMissingField, ErrMissingField},
		{"missing source_repo", Entry{Name: "n", Publisher: "p", SourceRepo: "", ImageRef: "i", PinnedDigest: validDigestA, SignatureKind: SignatureKindNone, SourceKind: SourceKindOfficial}, ErrMissingField, ErrMissingField},
		{"missing image_ref", Entry{Name: "n", Publisher: "p", SourceRepo: "r", ImageRef: "", PinnedDigest: validDigestA, SignatureKind: SignatureKindNone, SourceKind: SourceKindOfficial}, ErrMissingField, ErrMissingField},
		{"whitespace-only name", Entry{Name: "  ", Publisher: "p", SourceRepo: "r", ImageRef: "i", PinnedDigest: validDigestA, SignatureKind: SignatureKindNone, SourceKind: SourceKindOfficial}, ErrMissingField, ErrMissingField},
		{"floating tag digest", Entry{Name: "n", Publisher: "p", SourceRepo: "r", ImageRef: "i", PinnedDigest: "latest", SignatureKind: SignatureKindNone, SourceKind: SourceKindOfficial}, ErrInvalidDigest, ErrInvalidDigest},
		{"empty digest", Entry{Name: "n", Publisher: "p", SourceRepo: "r", ImageRef: "i", PinnedDigest: "", SignatureKind: SignatureKindNone, SourceKind: SourceKindOfficial}, ErrInvalidDigest, ErrInvalidDigest},
		{"invalid signature kind", Entry{Name: "n", Publisher: "p", SourceRepo: "r", ImageRef: "i", PinnedDigest: validDigestA, SignatureKind: "bad", SourceKind: SourceKindOfficial}, ErrInvalidSignatureKind, ErrInvalidSignatureKind},
		{"invalid source kind", Entry{Name: "n", Publisher: "p", SourceRepo: "r", ImageRef: "i", PinnedDigest: validDigestA, SignatureKind: SignatureKindNone, SourceKind: "bad"}, ErrInvalidSourceKind, ErrInvalidSourceKind},
		// Order: missing field -> source kind -> signature kind -> digest
		{"missing field before bad source kind", Entry{Name: "", Publisher: "p", SourceRepo: "r", ImageRef: "i", PinnedDigest: validDigestA, SignatureKind: SignatureKindNone, SourceKind: "bad"}, ErrMissingField, ErrMissingField},
		{"bad source kind before bad signature kind", Entry{Name: "n", Publisher: "p", SourceRepo: "r", ImageRef: "i", PinnedDigest: validDigestA, SignatureKind: "bad", SourceKind: "bad"}, ErrInvalidSourceKind, ErrInvalidSourceKind},
		{"bad signature kind before bad digest", Entry{Name: "n", Publisher: "p", SourceRepo: "r", ImageRef: "i", PinnedDigest: "bad", SignatureKind: "bad", SourceKind: SourceKindOfficial}, ErrInvalidSignatureKind, ErrInvalidSignatureKind},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateEntry(tc.entry)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("ValidateEntry: got error %v, want nil", err)
				}
			} else {
				if err == nil {
					t.Fatalf("ValidateEntry: got nil, want error")
				}
				if !errors.Is(err, tc.wantErrIs) {
					t.Fatalf("ValidateEntry: error %v is not %v", err, tc.wantErrIs)
				}
			}
		})
	}
}

func TestVerifyInstall(t *testing.T) {
	baseOfficial := Entry{
		Name:                "test-app",
		Publisher:           "publisher",
		SourceRepo:          "https://github.com/org/repo",
		ImageRef:            "registry.io/app:latest",
		PinnedDigest:        validDigestA,
		SignatureKind:       SignatureKindNone,
		SourceKind:          SourceKindOfficial,
		UnverifiedConfirmed: false,
	}
	baseOfficialCosign := baseOfficial
	baseOfficialCosign.SignatureKind = SignatureKindCosign

	baseCustom := baseOfficial
	baseCustom.SourceKind = SourceKindCustom
	baseCustom.UnverifiedConfirmed = true

	baseCustomUnconfirmed := baseCustom
	baseCustomUnconfirmed.UnverifiedConfirmed = false

	tests := []struct {
		name         string
		entry        Entry
		servedDigest string
		sig          SignatureStatus
		wantAllowed  bool
		wantReason   string
	}{
		// Happy paths
		{"official none + not_provided -> allowed", baseOfficial, validDigestA, SignatureStatusNotProvided, true, ""},
		{"official cosign + verified -> allowed", baseOfficialCosign, validDigestA, SignatureStatusVerified, true, ""},
		{"custom confirmed + matching digest -> allowed", baseCustom, validDigestA, SignatureStatusNotProvided, true, ""},

		// Digest checks (order 1, 2)
		{"invalid served digest -> invalid_served_digest", baseOfficial, "not-a-digest", SignatureStatusNotProvided, false, "invalid_served_digest"},
		{"digest mismatch -> digest_mismatch", baseOfficial, validDigestB, SignatureStatusNotProvided, false, "digest_mismatch"},

		// Signature failed (order 3) - applies to every source kind
		{"official signature failed -> signature_failed", baseOfficialCosign, validDigestA, SignatureStatusFailed, false, "signature_failed"},
		{"custom confirmed signature failed -> signature_failed", baseCustom, validDigestA, SignatureStatusFailed, false, "signature_failed"},

		// Signature missing when kind != none (order 4)
		{"official cosign + not_provided -> signature_missing", baseOfficialCosign, validDigestA, SignatureStatusNotProvided, false, "signature_missing"},
		{"official content-trust + not_provided -> signature_missing", func() Entry { e := baseOfficial; e.SignatureKind = SignatureKindContentTrust; return e }(), validDigestA, SignatureStatusNotProvided, false, "signature_missing"},

		// Custom unconfirmed (order 5)
		{"custom unconfirmed + matching digest + no sig -> unverified_unconfirmed", baseCustomUnconfirmed, validDigestA, SignatureStatusNotProvided, false, "unverified_unconfirmed"},

		// Precedence: custom unconfirmed AND digest mismatch -> digest_mismatch (proves ordering)
		{"custom unconfirmed + digest mismatch -> digest_mismatch (precedence)", baseCustomUnconfirmed, validDigestB, SignatureStatusNotProvided, false, "digest_mismatch"},
		{"custom unconfirmed + invalid served digest -> invalid_served_digest (precedence)", baseCustomUnconfirmed, "not-a-digest", SignatureStatusNotProvided, false, "invalid_served_digest"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dec := VerifyInstall(tc.entry, tc.servedDigest, tc.sig)
			if dec.Allowed != tc.wantAllowed {
				t.Fatalf("VerifyInstall: Allowed = %v, want %v", dec.Allowed, tc.wantAllowed)
			}
			if dec.Reason != tc.wantReason {
				t.Fatalf("VerifyInstall: Reason = %q, want %q", dec.Reason, tc.wantReason)
			}
		})
	}
}
