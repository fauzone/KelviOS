package vault

// KelviOS Fase 1 WS11 Langkah-B: vault unit tests (stdlib only).
//
// Secret VALUES never appear in test failure output — assertions compare
// only names, statuses, and sentinel errors.

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/fauzone/kelvios/internal/catalog"
)

const (
	testVaultPass  = "vault-passphrase-1"
	testVaultWrong = "wrong-passphrase-1"
	testSecretName = "api-key"
	testSecretVal  = "s3cr3t-value-xyz"
)

func openVaultDB(t *testing.T, path string) *Vault {
	t.Helper()
	db, err := catalog.Open(path)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	v := New(db)
	if err := v.LoadVaultConfig(); err != nil {
		t.Fatalf("load vault config: %v", err)
	}
	return v
}

func TestVaultSetupUnlockLockStatus(t *testing.T) {
	v := openVaultDB(t, ":memory:")

	if st := v.Status(); st.Exists || st.Unlocked {
		t.Fatalf("fresh status: got %+v, want {false false}", st)
	}

	if err := v.Setup("short"); !errors.Is(err, ErrPassphraseTooShort) {
		t.Errorf("short passphrase: got %v, want ErrPassphraseTooShort", err)
	}

	if err := v.Setup(testVaultPass); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if st := v.Status(); !st.Exists || !st.Unlocked {
		t.Errorf("after setup: got %+v, want {true true}", st)
	}

	if err := v.Setup(testVaultPass); !errors.Is(err, ErrVaultExists) {
		t.Errorf("second setup: got %v, want ErrVaultExists", err)
	}

	v.Lock()
	if st := v.Status(); !st.Exists || st.Unlocked {
		t.Errorf("after lock: got %+v, want {true false}", st)
	}
	v.Lock() // idempotent, must not panic

	if err := v.Unlock(testVaultWrong); !errors.Is(err, ErrInvalidPassphrase) {
		t.Errorf("wrong passphrase: got %v, want ErrInvalidPassphrase", err)
	}
	if st := v.Status(); st.Unlocked {
		t.Errorf("after wrong unlock: got %+v, want locked", st)
	}

	if err := v.Unlock(testVaultPass); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if st := v.Status(); !st.Exists || !st.Unlocked {
		t.Errorf("after unlock: got %+v, want {true true}", st)
	}
}

func TestVaultUnlockMissing(t *testing.T) {
	v := openVaultDB(t, ":memory:")
	if err := v.Unlock(testVaultPass); !errors.Is(err, ErrVaultNotFound) {
		t.Errorf("unlock without vault: got %v, want ErrVaultNotFound", err)
	}
}

func TestVaultPutGetDeleteList(t *testing.T) {
	v := openVaultDB(t, ":memory:")
	if err := v.Setup(testVaultPass); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if err := v.Put(testSecretName, testSecretVal); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := v.Get(testSecretName)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != testSecretVal {
		t.Errorf("round-trip mismatch: got %q", got)
	}

	names, err := v.ListNames()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(names) != 1 || names[0].Name != testSecretName {
		t.Fatalf("list mismatch: %+v", names)
	}

	if err := v.Delete("no-such-secret"); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("delete missing: got %v, want ErrSecretNotFound", err)
	}
	if err := v.Delete(testSecretName); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := v.Get(testSecretName); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("get after delete: got %v, want ErrSecretNotFound", err)
	}
}

func TestVaultLockedOpsFail(t *testing.T) {
	v := openVaultDB(t, ":memory:")
	if err := v.Setup(testVaultPass); err != nil {
		t.Fatalf("setup: %v", err)
	}
	v.Lock()

	if err := v.Put("k", "v"); !errors.Is(err, ErrVaultLocked) {
		t.Errorf("put locked: got %v, want ErrVaultLocked", err)
	}
	if _, err := v.Get(testSecretName); !errors.Is(err, ErrVaultLocked) {
		t.Errorf("get locked: got %v, want ErrVaultLocked", err)
	}
	if err := v.Delete(testSecretName); !errors.Is(err, ErrVaultLocked) {
		t.Errorf("delete locked: got %v, want ErrVaultLocked", err)
	}
	// ListNames works while locked (no key needed) — must not error.
	if _, err := v.ListNames(); err != nil {
		t.Errorf("list locked: %v", err)
	}
}

func TestVaultPersistAcrossReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "catalog.db")

	db1, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatalf("open db1: %v", err)
	}
	if err := catalog.Migrate(db1); err != nil {
		db1.Close()
		t.Fatalf("migrate db1: %v", err)
	}
	v1 := New(db1)
	if err := v1.LoadVaultConfig(); err != nil {
		db1.Close()
		t.Fatalf("load config db1: %v", err)
	}
	if err := v1.Setup(testVaultPass); err != nil {
		db1.Close()
		t.Fatalf("setup: %v", err)
	}
	if err := v1.Put(testSecretName, testSecretVal); err != nil {
		db1.Close()
		t.Fatalf("put: %v", err)
	}
	if err := db1.Close(); err != nil {
		t.Fatalf("close db1: %v", err)
	}

	// Reopen the SAME file with a fresh handle — must start LOCKED.
	db2, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatalf("open db2: %v", err)
	}
	defer db2.Close()
	if err := catalog.Migrate(db2); err != nil {
		t.Fatalf("migrate db2: %v", err)
	}
	v2 := New(db2)
	if err := v2.LoadVaultConfig(); err != nil {
		t.Fatalf("load config db2: %v", err)
	}
	if st := v2.Status(); !st.Exists || st.Unlocked {
		t.Fatalf("after reopen: got %+v, want {true false}", st)
	}
	if _, err := v2.Get(testSecretName); !errors.Is(err, ErrVaultLocked) {
		t.Fatalf("get before unlock: got %v, want ErrVaultLocked", err)
	}
	if err := v2.Unlock(testVaultPass); err != nil {
		t.Fatalf("unlock after reopen: %v", err)
	}
	got, err := v2.Get(testSecretName)
	if err != nil {
		t.Fatalf("get after reopen+unlock: %v", err)
	}
	if got != testSecretVal {
		t.Errorf("persisted value mismatch: got %q", got)
	}
}
