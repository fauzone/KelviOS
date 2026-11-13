package storage

// DriverInfo describes a storage driver known to the registry.
type DriverInfo struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"` // why unavailable
	Caps      Caps   `json:"caps"`
}

// DriverRegistry returns the list of all known drivers in priority order:
// single, mergerfs, btrfs, zfs.
func DriverRegistry() []DriverInfo {
	singleCaps := (&SingleDriver{}).Capabilities()

	return []DriverInfo{
		{
			Name:      "single",
			Available: true,
			Caps:      singleCaps,
		},
		{
			Name:      "mergerfs",
			Available: false,
			Reason:    "MergerFS + SnapRAID tooling on device (hardware phase)",
			Caps: Caps{
				MixSizes:         true,
				Parity:           ParityScheduled,
				Mirror:           false,
				Stripe:           false,
				Snapshot:         false,
				Checksum:         false,
				EncryptionNative: false,
				PowerControl:     false,
			},
		},
		{
			Name:      "btrfs",
			Available: false,
			Reason:    "Btrfs tooling on device (hardware phase)",
			Caps: Caps{
				MixSizes:         false,
				Parity:           ParityRealtime,
				Mirror:           true,
				Stripe:           true,
				Snapshot:         true,
				Checksum:         true,
				EncryptionNative: false,
				PowerControl:     false,
			},
		},
		{
			Name:      "zfs",
			Available: false,
			Reason:    "optional driver installed separately on device, not included in base image",
			Caps: Caps{
				MixSizes:         false,
				Parity:           ParityRealtime,
				Mirror:           true,
				Stripe:           true,
				Snapshot:         true,
				Checksum:         true,
				EncryptionNative: true,
				PowerControl:     false,
			},
		},
	}
}

// LookupDriver finds a driver by exact name match.
func LookupDriver(name string) (DriverInfo, bool) {
	for _, d := range DriverRegistry() {
		if d.Name == name {
			return d, true
		}
	}
	return DriverInfo{}, false
}

// DriverRegistryWithProbe returns the driver registry with availability
// determined by the provided ToolProbe. This allows callers (e.g. hardware
// detection) to get accurate availability without changing the default
// DriverRegistry() behavior which must remain deterministic for tests.
// MergerFS driver becomes Available=true ONLY if BOTH mergerfs and snapraid
// binaries are found via the probe. Other drivers' availability is unchanged.
func DriverRegistryWithProbe(probe ToolProbe) []DriverInfo {
	registry := DriverRegistry()

	// Check mergerfs availability via probe
	if probe != nil {
		mergerfsPath, _ := probe.LookPath("mergerfs")
		snapraidPath, _ := probe.LookPath("snapraid")
		if mergerfsPath != "" && snapraidPath != "" {
			// Both found: mergerfs is available
			for i := range registry {
				if registry[i].Name == "mergerfs" {
					registry[i].Available = true
					registry[i].Reason = ""
					break
				}
			}
		}
	}

	return registry
}

// LookupDriverWithProbe finds a driver by name with probe-aware availability.
func LookupDriverWithProbe(name string, probe ToolProbe) (DriverInfo, bool) {
	for _, d := range DriverRegistryWithProbe(probe) {
		if d.Name == name {
			return d, true
		}
	}
	return DriverInfo{}, false
}
