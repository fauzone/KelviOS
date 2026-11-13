package ups

import (
	"os"
	"testing"
	"time"
)

func TestBoot_MarkCleanShutdown_CheckBoot(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/boot-marker"

	// Initially no marker -> unclean
	clean, err := CheckBoot(path)
	if err != nil {
		t.Fatalf("CheckBoot error: %v", err)
	}
	if clean {
		t.Fatal("expected clean=false for missing marker")
	}

	// Mark clean shutdown
	err = MarkCleanShutdown(path)
	if err != nil {
		t.Fatalf("MarkCleanShutdown error: %v", err)
	}

	// CheckBoot should return true and remove marker
	clean, err = CheckBoot(path)
	if err != nil {
		t.Fatalf("CheckBoot error: %v", err)
	}
	if !clean {
		t.Fatal("expected clean=true after MarkCleanShutdown")
	}

	// Marker should be gone now
	_, err = os.Stat(path)
	if !os.IsNotExist(err) {
		t.Errorf("marker file should be removed after CheckBoot, err=%v", err)
	}

	// Second CheckBoot -> false (marker consumed)
	clean, err = CheckBoot(path)
	if err != nil {
		t.Fatalf("CheckBoot error: %v", err)
	}
	if clean {
		t.Fatal("expected clean=false after marker consumed")
	}
}

func TestBoot_MarkCleanShutdown_TimestampFormat(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/boot-marker"

	err := MarkCleanShutdown(path)
	if err != nil {
		t.Fatalf("MarkCleanShutdown error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile error: %v", err)
	}
	ts := string(data)
	// Should be RFC3339 format (parseable by time.Parse)
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Errorf("timestamp %q not valid RFC3339: %v", ts, err)
	}
	if parsed.IsZero() {
		t.Errorf("parsed time is zero")
	}
	// Should be recent (within last 10 seconds)
	if time.Since(parsed) > 10*time.Second {
		t.Errorf("timestamp too old: %v", parsed)
	}
}

func TestBoot_CheckBoot_ReadError(t *testing.T) {
	// Create a directory with same name as marker file to cause read error
	dir := t.TempDir()
	path := dir + "/boot-marker"
	err := os.Mkdir(path, 0o755)
	if err != nil {
		t.Fatalf("Mkdir error: %v", err)
	}

	// CheckBoot on a directory should return error (not IsNotExist)
	_, err = CheckBoot(path)
	if err == nil {
		t.Fatal("expected error for directory")
	}
	if os.IsNotExist(err) {
		t.Errorf("error should not be IsNotExist for directory")
	}
}

func TestBoot_FirstBootIsUnclean(t *testing.T) {
	// First boot (no marker ever created) should be unclean
	dir := t.TempDir()
	path := dir + "/boot-marker"

	clean, err := CheckBoot(path)
	if err != nil {
		t.Fatalf("CheckBoot error: %v", err)
	}
	if clean {
		t.Fatal("first boot should be unclean (clean=false)")
	}
}
