package ota

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fauzone/kelvios/internal/catalog"
	_ "modernc.org/sqlite"
)

func setupTestDB(t *testing.T) (*sql.DB, func()) {
	t.Helper()
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	return db, func() { db.Close() }
}

func generateTestKeyPair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return pub, priv
}

func createTestPackage(t *testing.T, dropDir, name, version, channel, artifactName string, pubKey ed25519.PublicKey, privKey ed25519.PrivateKey, corruptSig bool, corruptHash bool) string {
	t.Helper()
	pkgDir := filepath.Join(dropDir, name)
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatalf("mkdir pkg: %v", err)
	}

	// Create artifact
	artifactPath := filepath.Join(pkgDir, artifactName)
	artifactContent := []byte("test artifact content for " + name)
	// Compute SHA256 of the ORIGINAL content (for valid packages)
	// For corruptHash test, we'll write corrupted content but keep the original hash in manifest
	originalContent := artifactContent
	if corruptHash {
		artifactContent = []byte("corrupted content")
	}
	if err := os.WriteFile(artifactPath, artifactContent, 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}

	// Compute SHA256 of original content (so corruptHash test will have mismatch)
	h := sha256.Sum256(originalContent)
	sha256Hex := hex.EncodeToString(h[:])

	// Create manifest
	manifest := Manifest{
		Version:   version,
		Channel:   channel,
		Artifact:  artifactName,
		SHA256:    sha256Hex,
		CreatedAt: time.Now().Format(time.RFC3339),
		Notes:     "Test package",
		Kind:      "core",
	}
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	manifestPath := filepath.Join(pkgDir, "manifest.json")
	if err := os.WriteFile(manifestPath, manifestData, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	// Sign manifest
	sig := ed25519.Sign(privKey, manifestData)
	if corruptSig {
		sig[0] ^= 0xff
	}
	sigPath := filepath.Join(pkgDir, "manifest.sig")
	if err := os.WriteFile(sigPath, sig, 0o644); err != nil {
		t.Fatalf("write sig: %v", err)
	}

	return pkgDir
}

func TestValidatePackage_Success(t *testing.T) {
	pub, priv := generateTestKeyPair(t)
	dropDir := t.TempDir()
	createTestPackage(t, dropDir, "pkg1", "1.0.0", "stable", "artifact.bin", pub, priv, false, false)

	pkg, err := ValidatePackage(filepath.Join(dropDir, "pkg1"), pub, "stable")
	if err != nil {
		t.Fatalf("ValidatePackage: %v", err)
	}
	if pkg.Manifest.Version != "1.0.0" {
		t.Errorf("version: got %s, want 1.0.0", pkg.Manifest.Version)
	}
	if pkg.Manifest.Channel != "stable" {
		t.Errorf("channel: got %s, want stable", pkg.Manifest.Channel)
	}
}

func TestValidatePackage_InvalidSignature(t *testing.T) {
	pub, priv := generateTestKeyPair(t)
	dropDir := t.TempDir()
	createTestPackage(t, dropDir, "pkg1", "1.0.0", "stable", "artifact.bin", pub, priv, true, false)

	_, err := ValidatePackage(filepath.Join(dropDir, "pkg1"), pub, "stable")
	if err == nil {
		t.Fatal("expected error for invalid signature")
	}
	if !contains(err.Error(), "invalid_signature") {
		t.Errorf("error: got %q, want invalid_signature", err.Error())
	}
}

func TestValidatePackage_HashMismatch(t *testing.T) {
	pub, priv := generateTestKeyPair(t)
	dropDir := t.TempDir()
	createTestPackage(t, dropDir, "pkg1", "1.0.0", "stable", "artifact.bin", pub, priv, false, true)

	_, err := ValidatePackage(filepath.Join(dropDir, "pkg1"), pub, "stable")
	if err == nil {
		t.Fatal("expected error for hash mismatch")
	}
	if !contains(err.Error(), "hash_mismatch") {
		t.Errorf("error: got %q, want hash_mismatch", err.Error())
	}
}

func TestValidatePackage_ChannelMismatch(t *testing.T) {
	pub, priv := generateTestKeyPair(t)
	dropDir := t.TempDir()
	createTestPackage(t, dropDir, "pkg1", "1.0.0", "beta", "artifact.bin", pub, priv, false, false)

	_, err := ValidatePackage(filepath.Join(dropDir, "pkg1"), pub, "stable")
	if err == nil {
		t.Fatal("expected error for channel mismatch")
	}
	if !contains(err.Error(), "channel_mismatch") {
		t.Errorf("error: got %q, want channel_mismatch", err.Error())
	}
}

func TestValidatePackage_ChannelMismatchReverse(t *testing.T) {
	pub, priv := generateTestKeyPair(t)
	dropDir := t.TempDir()
	createTestPackage(t, dropDir, "pkg1", "1.0.0", "stable", "artifact.bin", pub, priv, false, false)

	_, err := ValidatePackage(filepath.Join(dropDir, "pkg1"), pub, "beta")
	if err == nil {
		t.Fatal("expected error for channel mismatch (stable package on beta channel)")
	}
	if !contains(err.Error(), "channel_mismatch") {
		t.Errorf("error: got %q, want channel_mismatch", err.Error())
	}
}

func TestValidatePackage_MissingManifest(t *testing.T) {
	pub, _ := generateTestKeyPair(t)
	dropDir := t.TempDir()
	pkgDir := filepath.Join(dropDir, "pkg1")
	os.MkdirAll(pkgDir, 0o755)
	// No manifest.json

	_, err := ValidatePackage(pkgDir, pub, "stable")
	if err == nil {
		t.Fatal("expected error for missing manifest")
	}
}

func TestValidatePackage_MissingArtifact(t *testing.T) {
	pub, priv := generateTestKeyPair(t)
	dropDir := t.TempDir()
	pkgDir := filepath.Join(dropDir, "pkg1")
	os.MkdirAll(pkgDir, 0o755)
	manifest := Manifest{Version: "1.0.0", Channel: "stable", Artifact: "missing.bin", SHA256: "abc"}
	data, _ := json.Marshal(manifest)
	os.WriteFile(filepath.Join(pkgDir, "manifest.json"), data, 0o644)
	sig := ed25519.Sign(priv, data)
	os.WriteFile(filepath.Join(pkgDir, "manifest.sig"), sig, 0o644)

	_, err := ValidatePackage(pkgDir, pub, "stable")
	if err == nil {
		t.Fatal("expected error for missing artifact")
	}
}

func TestValidatePackage_MissingSignature(t *testing.T) {
	pub, _ := generateTestKeyPair(t)
	dropDir := t.TempDir()
	pkgDir := filepath.Join(dropDir, "pkg1")
	os.MkdirAll(pkgDir, 0o755)
	artifactPath := filepath.Join(pkgDir, "artifact.bin")
	os.WriteFile(artifactPath, []byte("content"), 0o644)
	h := sha256.Sum256([]byte("content"))
	manifest := Manifest{Version: "1.0.0", Channel: "stable", Artifact: "artifact.bin", SHA256: hex.EncodeToString(h[:])}
	data, _ := json.Marshal(manifest)
	os.WriteFile(filepath.Join(pkgDir, "manifest.json"), data, 0o644)
	// No manifest.sig

	_, err := ValidatePackage(pkgDir, pub, "stable")
	if err == nil {
		t.Fatal("expected error for missing signature")
	}
}

func TestService_FullFlow_Success(t *testing.T) {
	pub, priv := generateTestKeyPair(t)
	db, cleanup := setupTestDB(t)
	defer cleanup()

	dataDir := t.TempDir()
	dropDir := filepath.Join(dataDir, "ota-drop")
	os.MkdirAll(dropDir, 0o755)

	createTestPackage(t, dropDir, "pkg1", "1.0.0", "stable", "artifact.bin", pub, priv, false, false)

	healthCalls := 0
	healthCheck := func(ctx context.Context) error {
		healthCalls++
		return nil
	}

	svc, err := NewService(ServiceConfig{
		DropDir:       dropDir,
		PublicKey:     pub,
		ActiveChannel: "stable",
		DataDir:       dataDir,
		DB:            db,
	}, healthCheck)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	ctx := context.Background()

	// Stage
	rec, err := svc.Stage(ctx, "pkg1", "testuser")
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if rec.State != StateStaged {
		t.Errorf("state after stage: got %s, want %s", rec.State, StateStaged)
	}
	if rec.SlotTarget == "" {
		t.Error("slot_target should be set after stage")
	}

	// Verify artifact in inactive slot
	inactiveSlot := svc.Switcher().InactiveSlot()
	if !svc.Switcher().(*DirSwitcher).HasArtifact(inactiveSlot) {
		t.Error("artifact should be in inactive slot after stage")
	}

	// Apply
	rec, err = svc.Apply(ctx, rec.ID, "testuser")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if rec.State != StateApplied {
		t.Errorf("state after apply: got %s, want %s", rec.State, StateApplied)
	}
	if healthCalls != 1 {
		t.Errorf("health check calls: got %d, want 1", healthCalls)
	}

	// Verify active slot switched
	if svc.ActiveSlot() != rec.SlotTarget {
		t.Errorf("active slot: got %s, want %s", svc.ActiveSlot(), rec.SlotTarget)
	}
	if !svc.Switcher().(*DirSwitcher).HasArtifact(svc.ActiveSlot()) {
		t.Error("artifact should be in active slot after apply")
	}
}

func TestService_FullFlow_HealthCheckFails_Rollback(t *testing.T) {
	pub, priv := generateTestKeyPair(t)
	db, cleanup := setupTestDB(t)
	defer cleanup()

	dataDir := t.TempDir()
	dropDir := filepath.Join(dataDir, "ota-drop")
	os.MkdirAll(dropDir, 0o755)

	createTestPackage(t, dropDir, "pkg1", "1.0.0", "stable", "artifact.bin", pub, priv, false, false)

	healthCheck := func(ctx context.Context) error {
		return errors.New("health check failed")
	}

	svc, err := NewService(ServiceConfig{
		DropDir:       dropDir,
		PublicKey:     pub,
		ActiveChannel: "stable",
		DataDir:       dataDir,
		DB:            db,
	}, healthCheck)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	ctx := context.Background()
	originalSlot := svc.ActiveSlot()

	// Stage
	rec, err := svc.Stage(ctx, "pkg1", "testuser")
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}

	// Apply - health check fails
	rec, err = svc.Apply(ctx, rec.ID, "testuser")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if rec.State != StateRolledBack {
		t.Errorf("state after failed health check: got %s, want %s", rec.State, StateRolledBack)
	}
	if rec.LastDetail == "" || !contains(rec.LastDetail, "health check failed") {
		t.Errorf("last_detail should contain health check failure: %s", rec.LastDetail)
	}

	// Active slot should be unchanged
	if svc.ActiveSlot() != originalSlot {
		t.Errorf("active slot after rollback: got %s, want %s", svc.ActiveSlot(), originalSlot)
	}
}

func TestService_InvalidTransitions(t *testing.T) {
	pub, priv := generateTestKeyPair(t)
	db, cleanup := setupTestDB(t)
	defer cleanup()

	dataDir := t.TempDir()
	dropDir := filepath.Join(dataDir, "ota-drop")
	os.MkdirAll(dropDir, 0o755)

	createTestPackage(t, dropDir, "pkg1", "1.0.0", "stable", "artifact.bin", pub, priv, false, false)

	svc, err := NewService(ServiceConfig{
		DropDir:       dropDir,
		PublicKey:     pub,
		ActiveChannel: "stable",
		DataDir:       dataDir,
		DB:            db,
	}, func(ctx context.Context) error { return nil })
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	ctx := context.Background()

	// Stage
	rec, err := svc.Stage(ctx, "pkg1", "testuser")
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}

	// Apply from available (not staged) - should fail
	rec2, _ := svc.Store().Create(ctx, &PackageInfo{Manifest: &Manifest{Version: "2.0.0", Channel: "stable", Artifact: "a.bin", SHA256: "abc", Kind: "core"}, Dir: "/tmp", ArtifactPath: ""}, "testuser")
	svc.Store().UpdateState(ctx, rec2.ID, StateAvailable, "testuser", "", "")
	_, err = svc.Apply(ctx, rec2.ID, "testuser")
	if err == nil {
		t.Error("expected error for apply from available")
	}
	if !contains(err.Error(), "invalid_transition") {
		t.Errorf("error: got %q, want invalid_transition", err.Error())
	}

	// Rollback from staged - should fail
	_, err = svc.Rollback(ctx, rec.ID, "testuser")
	if err == nil {
		t.Error("expected error for rollback from staged")
	}
	if !contains(err.Error(), "invalid_transition") {
		t.Errorf("error: got %q, want invalid_transition", err.Error())
	}

	// Apply success
	rec, err = svc.Apply(ctx, rec.ID, "testuser")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Rollback from applied - should succeed
	rec, err = svc.Rollback(ctx, rec.ID, "testuser")
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if rec.State != StateRolledBack {
		t.Errorf("state after rollback: got %s, want %s", rec.State, StateRolledBack)
	}

	// Rollback from rolled_back - should fail
	_, err = svc.Rollback(ctx, rec.ID, "testuser")
	if err == nil {
		t.Error("expected error for rollback from rolled_back")
	}
	if !contains(err.Error(), "invalid_transition") {
		t.Errorf("error: got %q, want invalid_transition", err.Error())
	}
}

func TestService_NotConfigured(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	dataDir := t.TempDir()
	dropDir := filepath.Join(dataDir, "ota-drop")
	os.MkdirAll(dropDir, 0o755)

	svc, err := NewService(ServiceConfig{
		DropDir:       dropDir,
		PublicKey:     nil, // not configured
		ActiveChannel: "stable",
		DataDir:       dataDir,
		DB:            db,
	}, func(ctx context.Context) error { return nil })
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	ctx := context.Background()
	_, err = svc.Stage(ctx, "pkg1", "testuser")
	if err == nil {
		t.Fatal("expected error for not configured")
	}
	if !contains(err.Error(), "ota_not_configured") {
		t.Errorf("error: got %q, want ota_not_configured", err.Error())
	}
}

func TestService_PackageNotFound(t *testing.T) {
	pub, _ := generateTestKeyPair(t)
	db, cleanup := setupTestDB(t)
	defer cleanup()

	dataDir := t.TempDir()
	dropDir := filepath.Join(dataDir, "ota-drop")
	os.MkdirAll(dropDir, 0o755)

	svc, err := NewService(ServiceConfig{
		DropDir:       dropDir,
		PublicKey:     pub,
		ActiveChannel: "stable",
		DataDir:       dataDir,
		DB:            db,
	}, func(ctx context.Context) error { return nil })
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	ctx := context.Background()
	_, err = svc.Stage(ctx, "nonexistent", "testuser")
	if err == nil {
		t.Fatal("expected error for package not found")
	}
	if !contains(err.Error(), "package_not_found") {
		t.Errorf("error: got %q, want package_not_found", err.Error())
	}
}

func TestService_InvalidPackageName(t *testing.T) {
	pub, _ := generateTestKeyPair(t)
	db, cleanup := setupTestDB(t)
	defer cleanup()

	dataDir := t.TempDir()
	dropDir := filepath.Join(dataDir, "ota-drop")
	os.MkdirAll(dropDir, 0o755)

	svc, err := NewService(ServiceConfig{
		DropDir:       dropDir,
		PublicKey:     pub,
		ActiveChannel: "stable",
		DataDir:       dataDir,
		DB:            db,
	}, func(ctx context.Context) error { return nil })
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	ctx := context.Background()
	for _, name := range []string{"../etc/passwd", "..", ".", "/absolute", "sub/../traversal"} {
		_, err = svc.Stage(ctx, name, "testuser")
		if err == nil {
			t.Errorf("expected error for invalid name %q", name)
		}
		if !contains(err.Error(), "invalid_package_name") {
			t.Errorf("error for %q: got %q, want invalid_package_name", name, err.Error())
		}
	}
}

func TestService_PersistAcrossRestart_FileDB(t *testing.T) {
	pub, priv := generateTestKeyPair(t)

	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "catalog.db")
	dropDir := filepath.Join(dataDir, "ota-drop")
	os.MkdirAll(dropDir, 0o755)
	createTestPackage(t, dropDir, "pkg1", "1.0.0", "stable", "artifact.bin", pub, priv, false, false)

	// First process
	db1, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatalf("open db1: %v", err)
	}
	if err := catalog.Migrate(db1); err != nil {
		t.Fatalf("migrate db1: %v", err)
	}
	svc1, err := NewService(ServiceConfig{
		DropDir:       dropDir,
		PublicKey:     pub,
		ActiveChannel: "stable",
		DataDir:       dataDir,
		DB:            db1,
	}, func(ctx context.Context) error { return nil })
	if err != nil {
		t.Fatalf("NewService1: %v", err)
	}

	ctx := context.Background()
	rec, err := svc1.Stage(ctx, "pkg1", "testuser")
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	rec, err = svc1.Apply(ctx, rec.ID, "testuser")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	recID := rec.ID
	db1.Close()

	// Second process
	db2, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatalf("open db2: %v", err)
	}
	if err := catalog.Migrate(db2); err != nil {
		t.Fatalf("migrate db2: %v", err)
	}
	defer db2.Close()

	svc2, err := NewService(ServiceConfig{
		DropDir:       dropDir,
		PublicKey:     pub,
		ActiveChannel: "stable",
		DataDir:       dataDir,
		DB:            db2,
	}, func(ctx context.Context) error { return nil })
	if err != nil {
		t.Fatalf("NewService2: %v", err)
	}

	// Check state persisted
	rec2, err := svc2.Store().Get(ctx, recID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec2 == nil {
		t.Fatal("record not found after restart")
	}
	if rec2.State != StateApplied {
		t.Errorf("state after restart: got %s, want %s", rec2.State, StateApplied)
	}
	// Verify active slot is valid (slot-a or slot-b)
	activeSlot := svc2.ActiveSlot()
	if activeSlot != "slot-a" && activeSlot != "slot-b" {
		t.Errorf("active slot after restart: got %s, want slot-a or slot-b", activeSlot)
	}

	// Check history
	history, err := svc2.Store().List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(history) != 1 {
		t.Errorf("history length: got %d, want 1", len(history))
	}

	// Check schema version
	var version int
	err = db2.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&version)
	if err != nil {
		t.Fatalf("query schema_version: %v", err)
	}
	if version != 41 {
		t.Errorf("schema_version: got %d, want 41", version)
	}
}

func TestService_AuditLogs(t *testing.T) {
	pub, priv := generateTestKeyPair(t)
	db, cleanup := setupTestDB(t)
	defer cleanup()

	dataDir := t.TempDir()
	dropDir := filepath.Join(dataDir, "ota-drop")
	os.MkdirAll(dropDir, 0o755)
	createTestPackage(t, dropDir, "pkg1", "1.0.0", "stable", "artifact.bin", pub, priv, false, false)

	svc, err := NewService(ServiceConfig{
		DropDir:       dropDir,
		PublicKey:     pub,
		ActiveChannel: "stable",
		DataDir:       dataDir,
		DB:            db,
	}, func(ctx context.Context) error { return nil })
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	ctx := context.Background()
	rec, err := svc.Stage(ctx, "pkg1", "testuser")
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	rec, err = svc.Apply(ctx, rec.ID, "testuser")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Check audit logs
	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action LIKE 'ota.%'`).Scan(&count)
	if err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if count < 2 {
		t.Errorf("audit log entries: got %d, want at least 2 (stage + apply)", count)
	}

	// Check actors
	var actors []string
	rows, err := db.Query(`SELECT actor FROM audit_log WHERE action LIKE 'ota.%' ORDER BY created_at`)
	if err != nil {
		t.Fatalf("query audit actors: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var actor string
		rows.Scan(&actor)
		actors = append(actors, actor)
	}
	// First should be testuser (stage), second should be testuser (apply)
	if len(actors) >= 2 && actors[0] != "testuser" {
		t.Errorf("first audit actor: got %s, want testuser", actors[0])
	}
	if len(actors) >= 2 && actors[1] != "testuser" {
		t.Errorf("second audit actor: got %s, want testuser", actors[1])
	}

	// Check no private key material in audit
	var detail string
	err = db.QueryRow(`SELECT detail FROM audit_log WHERE action LIKE 'ota.%' LIMIT 1`).Scan(&detail)
	if err != nil {
		t.Fatalf("query audit detail: %v", err)
	}
	// Private key is base64, check it's not in detail
	pubB64 := base64.StdEncoding.EncodeToString(pub)
	if contains(detail, pubB64) {
		t.Error("public key found in audit detail (should not leak key material)")
	}
}

func TestDirSwitcher_Basic(t *testing.T) {
	dataDir := t.TempDir()
	sw, err := NewDirSwitcher(filepath.Join(dataDir, "ota"))
	if err != nil {
		t.Fatalf("NewDirSwitcher: %v", err)
	}

	if sw.ActiveSlot() != "slot-a" {
		t.Errorf("initial active slot: got %s, want slot-a", sw.ActiveSlot())
	}
	if sw.InactiveSlot() != "slot-b" {
		t.Errorf("initial inactive slot: got %s, want slot-b", sw.InactiveSlot())
	}

	// Create a test artifact
	artifact := filepath.Join(dataDir, "test-artifact")
	os.WriteFile(artifact, []byte("test"), 0o644)

	ctx := context.Background()
	if err := sw.StageToInactive(ctx, artifact); err != nil {
		t.Fatalf("StageToInactive: %v", err)
	}
	if !sw.HasArtifact("slot-b") {
		t.Error("artifact should be in slot-b")
	}

	if err := sw.MarkPendingSwitch(ctx); err != nil {
		t.Fatalf("MarkPendingSwitch: %v", err)
	}
	if sw.PendingSlot() != "slot-b" {
		t.Errorf("pending slot: got %s, want slot-b", sw.PendingSlot())
	}

	if err := sw.CommitSwitch(ctx); err != nil {
		t.Fatalf("CommitSwitch: %v", err)
	}
	if sw.ActiveSlot() != "slot-b" {
		t.Errorf("active slot after commit: got %s, want slot-b", sw.ActiveSlot())
	}
	if sw.PendingSlot() != "" {
		t.Errorf("pending slot after commit: got %s, want empty", sw.PendingSlot())
	}

	// Rollback should clear pending
	if err := sw.MarkPendingSwitch(ctx); err != nil {
		t.Fatalf("MarkPendingSwitch 2: %v", err)
	}
	if err := sw.RollbackSwitch(ctx); err != nil {
		t.Fatalf("RollbackSwitch: %v", err)
	}
	if sw.PendingSlot() != "" {
		t.Errorf("pending slot after rollback: got %s, want empty", sw.PendingSlot())
	}
	if sw.ActiveSlot() != "slot-b" {
		t.Errorf("active slot after rollback: got %s, want slot-b (unchanged)", sw.ActiveSlot())
	}
}

func TestValidTransition(t *testing.T) {
	tests := []struct {
		from, to State
		want     bool
	}{
		{StateAvailable, StateStaged, true},
		{StateAvailable, StateFailed, true},
		{StateAvailable, StateApplied, false},
		{StateStaged, StatePendingSwitch, true},
		{StateStaged, StateFailed, true},
		{StateStaged, StateApplied, false},
		{StatePendingSwitch, StateApplied, true},
		{StatePendingSwitch, StateRolledBack, true},
		{StatePendingSwitch, StateFailed, true},
		{StatePendingSwitch, StateStaged, false},
		{StateApplied, StateRolledBack, true},
		{StateApplied, StateStaged, false},
		{StateRolledBack, StateApplied, false},
		{StateFailed, StateStaged, false},
	}
	for _, tc := range tests {
		if ValidTransition(tc.from, tc.to) != tc.want {
			t.Errorf("ValidTransition(%s, %s) = %v, want %v", tc.from, tc.to, ValidTransition(tc.from, tc.to), tc.want)
		}
	}
}

func TestDecodePublicKey(t *testing.T) {
	pub, _ := generateTestKeyPair(t)
	b64 := base64.StdEncoding.EncodeToString(pub)
	decoded, err := DecodePublicKey(b64)
	if err != nil {
		t.Fatalf("DecodePublicKey: %v", err)
	}
	if string(decoded) != string(pub) {
		t.Error("decoded key doesn't match")
	}

	// Invalid length
	_, err = DecodePublicKey("abc")
	if err == nil {
		t.Error("expected error for invalid key length")
	}
}

func TestLoadPublicKeyFromFile(t *testing.T) {
	pub, _ := generateTestKeyPair(t)
	b64 := base64.StdEncoding.EncodeToString(pub)
	tmpFile := filepath.Join(t.TempDir(), "pubkey.txt")
	os.WriteFile(tmpFile, []byte(b64), 0o644)

	loaded, err := LoadPublicKeyFromFile(tmpFile)
	if err != nil {
		t.Fatalf("LoadPublicKeyFromFile: %v", err)
	}
	if string(loaded) != string(pub) {
		t.Error("loaded key doesn't match")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && (s[:len(substr)] == substr || s[len(s)-len(substr):] == substr || containsMiddle(s, substr)))
}

func containsMiddle(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
