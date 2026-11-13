package daemonconfig

import (
	"strings"
	"testing"
)

func TestRenderSMB_GuestOff(t *testing.T) {
	sc := ShareConfig{
		ID:             "share-1",
		Name:           "media",
		Protocol:       "smb",
		ReadOnly:       false,
		CollectionPath: "/media",
		PoolMountpoint: "/mnt/pool1",
	}

	protocol, config, err := RenderShare(sc)
	if err != nil {
		t.Fatalf("RenderShare failed: %v", err)
	}
	if protocol != "smb" {
		t.Errorf("expected protocol smb, got %s", protocol)
	}

	// Guest must be off
	if !strings.Contains(config, "guest ok = no") {
		t.Errorf("SMB config missing 'guest ok = no':\n%s", config)
	}

	// Read-only should be no (since ReadOnly=false)
	if !strings.Contains(config, "read only = no") {
		t.Errorf("SMB config missing 'read only = no':\n%s", config)
	}

	// SMB2 minimum protocol
	if !strings.Contains(config, "server min protocol = SMB2") {
		t.Errorf("SMB config missing 'server min protocol = SMB2':\n%s", config)
	}

	// Encryption/signing flags
	if !strings.Contains(config, "smb encrypt = desired") {
		t.Errorf("SMB config missing 'smb encrypt = desired':\n%s", config)
	}
	if !strings.Contains(config, "client signing = required") {
		t.Errorf("SMB config missing 'client signing = required':\n%s", config)
	}
	if !strings.Contains(config, "server signing = required") {
		t.Errorf("SMB config missing 'server signing = required':\n%s", config)
	}

	// Path should be correct
	if !strings.Contains(config, "path = /mnt/pool1/media") {
		t.Errorf("SMB config has wrong path:\n%s", config)
	}
}

func TestRenderSMB_ReadOnly(t *testing.T) {
	sc := ShareConfig{
		ID:             "share-2",
		Name:           "readonly-share",
		Protocol:       "smb",
		ReadOnly:       true,
		CollectionPath: "/data",
		PoolMountpoint: "/mnt/pool1",
	}

	_, config, err := RenderShare(sc)
	if err != nil {
		t.Fatalf("RenderShare failed: %v", err)
	}

	if !strings.Contains(config, "read only = yes") {
		t.Errorf("SMB config should have 'read only = yes' for read-only share:\n%s", config)
	}
}

func TestRenderNFS_ReadOnlyDefault(t *testing.T) {
	// Even with ReadOnly=false, NFS defaults to read-only (ro)
	sc := ShareConfig{
		ID:             "share-3",
		Name:           "nfs-share",
		Protocol:       "nfs",
		ReadOnly:       false, // Not forced read-only, but still defaults to ro
		CollectionPath: "/nfs-data",
		PoolMountpoint: "/mnt/pool1",
		Metadata:       `{"allowed_ips": "192.168.1.0/24"}`,
	}

	_, config, err := RenderShare(sc)
	if err != nil {
		t.Fatalf("RenderShare failed: %v", err)
	}

	// Should contain 'ro' (read-only) by default
	if !strings.Contains(config, "ro") {
		t.Errorf("NFS config should default to 'ro':\n%s", config)
	}

	// Should contain the allowed IPs
	if !strings.Contains(config, "192.168.1.0/24") {
		t.Errorf("NFS config should contain allowed IPs:\n%s", config)
	}

	// Should NOT contain wildcard
	if strings.Contains(config, "*(") || strings.Contains(config, " *") {
		t.Errorf("NFS config must not contain wildcard export:\n%s", config)
	}
}

func TestRenderNFS_ReadOnlyExplicit(t *testing.T) {
	sc := ShareConfig{
		ID:             "share-4",
		Name:           "nfs-ro-share",
		Protocol:       "nfs",
		ReadOnly:       true,
		CollectionPath: "/nfs-ro",
		PoolMountpoint: "/mnt/pool1",
		Metadata:       `{"allowed_ips": "10.0.0.0/8"}`,
	}

	_, config, err := RenderShare(sc)
	if err != nil {
		t.Fatalf("RenderShare failed: %v", err)
	}

	if !strings.Contains(config, "ro") {
		t.Errorf("NFS config should have 'ro' for explicit read-only:\n%s", config)
	}
}

func TestRenderNFS_NoMetadata_RestrictedLoopback(t *testing.T) {
	// No allowed_ips metadata -> should render restricted loopback line
	sc := ShareConfig{
		ID:             "share-5",
		Name:           "nfs-no-meta",
		Protocol:       "nfs",
		ReadOnly:       false,
		CollectionPath: "/nfs-nometa",
		PoolMountpoint: "/mnt/pool1",
		Metadata:       "",
	}

	_, config, err := RenderShare(sc)
	if err != nil {
		t.Fatalf("RenderShare failed: %v", err)
	}

	// Should contain loopback with RESTRICTED comment
	if !strings.Contains(config, "127.0.0.1") {
		t.Errorf("NFS config should contain loopback when no metadata:\n%s", config)
	}
	if !strings.Contains(config, "RESTRICTED") {
		t.Errorf("NFS config should have RESTRICTED marker when no metadata:\n%s", config)
	}

	// Should NOT contain wildcard
	if strings.Contains(config, "*(") {
		t.Errorf("NFS config must not contain wildcard:\n%s", config)
	}
}

func TestRenderNFS_IPRestrictionHonoured(t *testing.T) {
	sc := ShareConfig{
		ID:             "share-6",
		Name:           "nfs-multi-ip",
		Protocol:       "nfs",
		ReadOnly:       false,
		CollectionPath: "/nfs-multi",
		PoolMountpoint: "/mnt/pool1",
		Metadata:       `{"allowed_ips": "192.168.1.10,192.168.1.20,10.0.0.0/8"}`,
	}

	_, config, err := RenderShare(sc)
	if err != nil {
		t.Fatalf("RenderShare failed: %v", err)
	}

	// All three IPs should be present
	for _, ip := range []string{"192.168.1.10", "192.168.1.20", "10.0.0.0/8"} {
		if !strings.Contains(config, ip) {
			t.Errorf("NFS config missing IP %s:\n%s", ip, config)
		}
	}
}

func TestRenderSFTP_KeysOnly(t *testing.T) {
	sc := ShareConfig{
		ID:             "share-7",
		Name:           "sftp-share",
		Protocol:       "sftp",
		ReadOnly:       false,
		CollectionPath: "/sftp-data",
		PoolMountpoint: "/mnt/pool1",
	}

	_, config, err := RenderShare(sc)
	if err != nil {
		t.Fatalf("RenderShare failed: %v", err)
	}

	// Must have keys-only (no password auth)
	if !strings.Contains(config, "PasswordAuthentication no") {
		t.Errorf("SFTP config missing 'PasswordAuthentication no':\n%s", config)
	}
	if !strings.Contains(config, "PubkeyAuthentication yes") {
		t.Errorf("SFTP config missing 'PubkeyAuthentication yes':\n%s", config)
	}

	// Must have ChrootDirectory scoped to collection path
	if !strings.Contains(config, "ChrootDirectory /mnt/pool1/sftp-data") {
		t.Errorf("SFTP config missing correct ChrootDirectory:\n%s", config)
	}

	// Must have internal-sftp
	if !strings.Contains(config, "ForceCommand internal-sftp") {
		t.Errorf("SFTP config missing 'ForceCommand internal-sftp':\n%s", config)
	}
}

func TestRenderSFTP_ReadOnly(t *testing.T) {
	sc := ShareConfig{
		ID:             "share-8",
		Name:           "sftp-ro",
		Protocol:       "sftp",
		ReadOnly:       true,
		CollectionPath: "/sftp-ro",
		PoolMountpoint: "/mnt/pool1",
	}

	_, config, err := RenderShare(sc)
	if err != nil {
		t.Fatalf("RenderShare failed: %v", err)
	}

	// Should have read-only comment
	if !strings.Contains(config, "Read-only") {
		t.Errorf("SFTP config should indicate read-only:\n%s", config)
	}
}

func TestRenderShare_EmptyName_Error(t *testing.T) {
	sc := ShareConfig{
		ID:             "share-9",
		Name:           "",
		Protocol:       "smb",
		ReadOnly:       false,
		CollectionPath: "/data",
		PoolMountpoint: "/mnt/pool1",
	}

	_, _, err := RenderShare(sc)
	if err == nil {
		t.Fatal("expected error for empty name")
	}
	if !IsRenderError(err) {
		t.Errorf("expected RenderError, got %T: %v", err, err)
	}
}

func TestRenderShare_EmptyCollectionPath_Error(t *testing.T) {
	sc := ShareConfig{
		ID:             "share-10",
		Name:           "test",
		Protocol:       "smb",
		ReadOnly:       false,
		CollectionPath: "",
		PoolMountpoint: "/mnt/pool1",
	}

	_, _, err := RenderShare(sc)
	if err == nil {
		t.Fatal("expected error for empty collection path")
	}
	if !IsRenderError(err) {
		t.Errorf("expected RenderError, got %T: %v", err, err)
	}
}

func TestRenderShare_EmptyPoolMountpoint_Fallback(t *testing.T) {
	// With PoolName set, empty PoolMountpoint should fall back to /mnt/<PoolName>
	sc := ShareConfig{
		ID:             "share-11",
		Name:           "test",
		Protocol:       "smb",
		ReadOnly:       false,
		CollectionPath: "/data",
		PoolMountpoint: "",
		PoolName:       "mypool",
	}

	protocol, config, err := RenderShare(sc)
	if err != nil {
		t.Fatalf("RenderShare failed with fallback: %v", err)
	}
	if protocol != "smb" {
		t.Errorf("expected protocol smb, got %s", protocol)
	}
	// Should contain the fallback path
	if !strings.Contains(config, "path = /mnt/mypool/data") {
		t.Errorf("SMB config should use fallback path /mnt/mypool/data:\n%s", config)
	}
}

func TestRenderShare_EmptyPoolMountpointAndName_Error(t *testing.T) {
	// Both PoolMountpoint and PoolName empty -> should error
	sc := ShareConfig{
		ID:             "share-11b",
		Name:           "test",
		Protocol:       "smb",
		ReadOnly:       false,
		CollectionPath: "/data",
		PoolMountpoint: "",
		PoolName:       "",
	}

	_, _, err := RenderShare(sc)
	if err == nil {
		t.Fatal("expected error for empty pool mountpoint and pool name")
	}
	if !IsRenderError(err) {
		t.Errorf("expected RenderError, got %T: %v", err, err)
	}
}

func TestRenderShare_UnknownProtocol_Error(t *testing.T) {
	sc := ShareConfig{
		ID:             "share-12",
		Name:           "test",
		Protocol:       "unknown",
		ReadOnly:       false,
		CollectionPath: "/data",
		PoolMountpoint: "/mnt/pool1",
	}

	_, _, err := RenderShare(sc)
	if err == nil {
		t.Fatal("expected error for unknown protocol")
	}
	if !IsRenderError(err) {
		t.Errorf("expected RenderError, got %T: %v", err, err)
	}
}

func TestRenderStability_ByteIdentical(t *testing.T) {
	sc := ShareConfig{
		ID:             "share-stable",
		Name:           "stable-share",
		Protocol:       "smb",
		ReadOnly:       false,
		CollectionPath: "/stable",
		PoolMountpoint: "/mnt/pool1",
		Metadata:       `{"allowed_ips": "192.168.1.0/24"}`,
	}

	_, config1, err := RenderShare(sc)
	if err != nil {
		t.Fatalf("first render failed: %v", err)
	}

	_, config2, err := RenderShare(sc)
	if err != nil {
		t.Fatalf("second render failed: %v", err)
	}

	if config1 != config2 {
		t.Errorf("render not stable: outputs differ\n--- first ---\n%s\n--- second ---\n%s", config1, config2)
	}
}

func TestRenderAllShares_GroupedByProtocol(t *testing.T) {
	shares := []ShareConfig{
		{
			ID:             "share-1",
			Name:           "smb1",
			Protocol:       "smb",
			ReadOnly:       false,
			CollectionPath: "/smb1",
			PoolMountpoint: "/mnt/pool1",
		},
		{
			ID:             "share-2",
			Name:           "nfs1",
			Protocol:       "nfs",
			ReadOnly:       false,
			CollectionPath: "/nfs1",
			PoolMountpoint: "/mnt/pool1",
			Metadata:       `{"allowed_ips": "192.168.1.0/24"}`,
		},
		{
			ID:             "share-3",
			Name:           "sftp1",
			Protocol:       "sftp",
			ReadOnly:       false,
			CollectionPath: "/sftp1",
			PoolMountpoint: "/mnt/pool1",
		},
		{
			ID:             "share-4",
			Name:           "smb2",
			Protocol:       "smb",
			ReadOnly:       true,
			CollectionPath: "/smb2",
			PoolMountpoint: "/mnt/pool1",
		},
	}

	result := RenderAllShares(shares)

	// Check all three protocols present
	for _, proto := range []string{"smb", "nfs", "sftp"} {
		if _, ok := result.Configs[proto]; !ok {
			t.Errorf("missing protocol %s in result", proto)
		}
		if result.Configs[proto] == "" {
			t.Errorf("protocol %s has empty config", proto)
		}
	}

	// Should have no errors
	if len(result.Errors) != 0 {
		t.Errorf("expected 0 errors, got %d: %+v", len(result.Errors), result.Errors)
	}

	// SMB should have global section only once
	smbGlobalCount := strings.Count(result.Configs["smb"], "[global]")
	if smbGlobalCount != 1 {
		t.Errorf("SMB global section should appear exactly once, got %d:\n%s", smbGlobalCount, result.Configs["smb"])
	}

	// SMB should have both share sections
	if !strings.Contains(result.Configs["smb"], "[smb1]") {
		t.Errorf("SMB config missing [smb1] section")
	}
	if !strings.Contains(result.Configs["smb"], "[smb2]") {
		t.Errorf("SMB config missing [smb2] section")
	}

	// NFS should have both exports (only one in this test)
	if !strings.Contains(result.Configs["nfs"], "nfs1") || !strings.Contains(result.Configs["nfs"], "192.168.1.0/24") {
		t.Errorf("NFS config missing expected export:\n%s", result.Configs["nfs"])
	}

	// SFTP should have both match blocks (only one in this test)
	if !strings.Contains(result.Configs["sftp"], "sftp1") {
		t.Errorf("SFTP config missing expected match block:\n%s", result.Configs["sftp"])
	}
}

func TestRenderAllShares_WithUnrenderableShare(t *testing.T) {
	// One share has empty name (unrenderable), others should still render
	shares := []ShareConfig{
		{
			ID:             "share-good-1",
			Name:           "smb-good",
			Protocol:       "smb",
			ReadOnly:       false,
			CollectionPath: "/smb1",
			PoolMountpoint: "/mnt/pool1",
		},
		{
			ID:             "share-bad",
			Name:           "", // empty name -> unrenderable
			Protocol:       "nfs",
			ReadOnly:       false,
			CollectionPath: "/nfs1",
			PoolMountpoint: "/mnt/pool1",
		},
		{
			ID:             "share-good-2",
			Name:           "sftp-good",
			Protocol:       "sftp",
			ReadOnly:       false,
			CollectionPath: "/sftp1",
			PoolMountpoint: "/mnt/pool1",
		},
	}

	result := RenderAllShares(shares)

	// Should have all three protocols in configs (even if some are empty)
	for _, proto := range []string{"smb", "nfs", "sftp"} {
		if _, ok := result.Configs[proto]; !ok {
			t.Errorf("missing protocol %s in result", proto)
		}
	}

	// SMB should have the good share
	if !strings.Contains(result.Configs["smb"], "[smb-good]") {
		t.Errorf("SMB config missing [smb-good] section:\n%s", result.Configs["smb"])
	}

	// SFTP should have the good share
	if !strings.Contains(result.Configs["sftp"], "sftp-good") {
		t.Errorf("SFTP config missing sftp-good:\n%s", result.Configs["sftp"])
	}

	// Should have exactly one error for the bad share
	if len(result.Errors) != 1 {
		t.Errorf("expected 1 error, got %d: %+v", len(result.Errors), result.Errors)
	}
	if result.Errors[0].ShareID != "share-bad" {
		t.Errorf("error should be for share-bad, got %s", result.Errors[0].ShareID)
	}
	if !strings.Contains(result.Errors[0].Reason, "empty share name") {
		t.Errorf("error reason should mention empty share name: %s", result.Errors[0].Reason)
	}
}