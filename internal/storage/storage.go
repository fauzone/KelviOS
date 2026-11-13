// Package storage defines KelviOS's core storage types and the Storage
// Driver contract (ARCHITECTURE §5). The core knows no specific filesystem:
// every pool is created through a driver that reports its capabilities, and
// the UI renders choices based on those capabilities.
//
// WS3 (Phase 1 foundation) scope: types + driver contract + disk inventory
// + a metadata-only single-disk stub driver. Real pool operations land in
// later workstreams.
package storage

import "time"

// DefaultNodeID is carried by every disk and pool until the multi-node
// registry lands; ARCHITECTURE §5 requires a node_id from day one so the
// schema never has to be retrofitted.
const DefaultNodeID = "local"

// Tier is the mandatory Hot–Warm–Cold classification of a pool
// (ARCHITECTURE §6). There is no silent default: a pool without a tier
// cannot be created.
type Tier string

const (
	TierHot  Tier = "hot"
	TierWarm Tier = "warm"
	TierCold Tier = "cold"
)

// Mechanism names the protection/layout mechanism of a pool, independent
// of the driver that implements it.
type Mechanism string

const (
	MechanismSingle          Mechanism = "single"
	MechanismMirror          Mechanism = "mirror"
	MechanismParityScheduled Mechanism = "parity-scheduled"
	MechanismStripe          Mechanism = "stripe" // RAID0: no protection
	MechanismZFS             Mechanism = "zfs"
)

// ParityMode describes how (and whether) a driver protects pool data.
type ParityMode string

const (
	ParityNone      ParityMode = "none"
	ParityRealtime  ParityMode = "parity-realtime"
	ParityScheduled ParityMode = "parity-scheduled"
)

// PowerState is what the core knows about a disk's power without ever
// waking it. Inventory is passive (ARCHITECTURE §6.1.5): it never probes
// a disk in a way that spins it up, so the state stays "unknown" until
// the power engine tracks transitions it caused itself.
type PowerState string

const (
	PowerUnknown PowerState = "unknown"
	PowerActive  PowerState = "active"
	PowerStandby PowerState = "standby"
	PowerOff     PowerState = "off"
)

// Disk is one physical block device as reported by inventory.
type Disk struct {
	Name       string     `json:"name"` // kernel name, e.g. "sda"
	Path       string     `json:"path"` // device path, e.g. "/dev/sda"
	Model      string     `json:"model"`
	Serial     string     `json:"serial"`
	SizeBytes  int64      `json:"size_bytes"`
	Rotational bool       `json:"rotational"` // true = spinning HDD, false = SSD/NVMe
	Mounted    bool       `json:"mounted"`
	Mountpoint string     `json:"mountpoint,omitempty"`
	PowerState PowerState `json:"power_state"`
	PoolID     string     `json:"pool_id,omitempty"`
	NodeID     string     `json:"node_id"`
}

// Caps is the capability set a Storage Driver reports to the core
// (ARCHITECTURE §5).
type Caps struct {
	MixSizes         bool       `json:"mix_sizes"` // disks of different sizes in one pool
	Parity           ParityMode `json:"parity"`
	Mirror           bool       `json:"mirror"`
	Stripe           bool       `json:"stripe"`
	Snapshot         bool       `json:"snapshot"`
	Checksum         bool       `json:"checksum"`
	EncryptionNative bool       `json:"encryption_native"`
	PowerControl     bool       `json:"power_control"` // per-disk spin-down / power-off
}

// Pool is a storage pool created through a driver.
type Pool struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	NodeID     string    `json:"node_id"`
	Tier       Tier      `json:"tier"`
	Mechanism  Mechanism `json:"mechanism"`
	Driver     string    `json:"driver"`
	DiskNames  []string  `json:"disk_names"`
	Mountpoint string    `json:"mountpoint"`
	SizeBytes  int64     `json:"size_bytes"`
	UsedBytes  int64     `json:"used_bytes"`
	Encrypted  bool      `json:"encrypted"`
}

// PoolConfig is the input to Driver.CreatePool. Tier is mandatory.
type PoolConfig struct {
	Name       string
	Tier       Tier
	Mechanism  Mechanism
	Disks      []string // kernel names, e.g. ["sda", "sdb"]
	Mountpoint string
	Encrypted  bool
}

// HealthReport is a driver's answer to a pool health query.
type HealthReport struct {
	PoolID  string `json:"pool_id"`
	Status  string `json:"status"` // "ok" | "degraded" | "failed" | "unknown"
	Message string `json:"message,omitempty"`
}

// Collection is a named data set inside a pool. On Cold pools placement
// is per collection per disk (ARCHITECTURE §6.2), so one request wakes
// only one disk.
type Collection struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	PoolID         string    `json:"pool_id"`
	Path           string    `json:"path"` // logical path inside the pool
	DiskName       string    `json:"disk_name,omitempty"`
	SizeBytes      int64     `json:"size_bytes"`
	LastAccessedAt time.Time `json:"last_accessed_at,omitempty"`
}

// Driver is the storage driver contract from ARCHITECTURE §5. The core
// only ever talks to drivers through this interface.
type Driver interface {
	// Name returns the stable driver identifier, e.g. "single".
	Name() string
	// Capabilities reports what this driver can do; the UI renders pool
	// choices from it.
	Capabilities() Caps
	CreatePool(cfg PoolConfig) (*Pool, error)
	AddDisk(poolID, dev string) error
	RemoveDisk(poolID, dev string) error
	Health(poolID string) (HealthReport, error)
	Scrub(poolID string) error
	DestroyPool(poolID string) error
}
