package appdriver

import (
	"github.com/fauzone/kelvios/internal/apps"
)

// Driver is the app container driver contract.
// The core only talks to drivers through this interface.
type Driver interface {
	// Name returns the stable driver identifier, e.g. "docker".
	Name() string
	// Install prepares and starts a container for the app.
	// Returns error if image not found, port conflict, or resource limits exceeded.
	Install(app apps.App) error
	// Start starts an existing container for the app.
	Start(appID string) error
	// Stop stops a running container for the app completely.
	Stop(appID string) error
	// Kill forcefully stops and removes the container for the app.
	Kill(appID string) error
	// Remove removes the container and its runtime data for the app.
	Remove(appID string) error
	// Update updates the app container to a new target image.
	// The driver MUST retain the previous image/container for rollback (one generation).
	// Returns error if image not found, port conflict, or resource limits exceeded.
	Update(app apps.App, targetImage string) error
	// RollbackUpdate rolls back the app container to the previous image.
	// The driver MUST have retained the previous image/container from the last Update.
	RollbackUpdate(appID string) error
	// Health checks if the app container is healthy.
	// Returns (healthy, error). Healthy means container running AND all declared ports accept TCP connections.
	Health(appID string) (bool, error)
	// Status returns the current runtime state of the app container.
	// This is for reconciliation/observability, not for writing app_installs.
	Status(appID string) (ContainerStatus, error)
}

// ContainerStatus represents the runtime state of an app container.
type ContainerStatus string

const (
	ContainerStatusUnknown   ContainerStatus = "unknown"
	ContainerStatusCreated   ContainerStatus = "created"
	ContainerStatusRunning   ContainerStatus = "running"
	ContainerStatusStopped   ContainerStatus = "stopped"
	ContainerStatusRemoved   ContainerStatus = "removed"
	ContainerStatusUnhealthy ContainerStatus = "unhealthy"
)

// ErrDriverUnavailable is returned when the driver cannot connect to the container runtime.
type ErrDriverUnavailable string

func (e ErrDriverUnavailable) Error() string {
	return "driver unavailable: " + string(e)
}

// ErrContainerNotFound is returned when the container for an app does not exist.
type ErrContainerNotFound string

func (e ErrContainerNotFound) Error() string {
	return "container not found: " + string(e)
}

// ErrPortConflict is returned when a declared port is already in use by a non-app process.
type ErrPortConflict struct {
	Port int
}

func (e *ErrPortConflict) Error() string {
	return "port conflict: port " + itoa(e.Port) + " already in use"
}

// ErrResourceExhausted is returned when system resources (RAM) are insufficient.
type ErrResourceExhausted struct {
	RequestedMB int64
	AvailableMB int64
}

func (e *ErrResourceExhausted) Error() string {
	return "resource exhausted: requested " + itoa64(e.RequestedMB) + "MB, available " + itoa64(e.AvailableMB) + "MB"
}

// itoa converts int to string without fmt dependency.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var buf [20]byte
	n := len(buf)
	for i > 0 {
		n--
		buf[n] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		n--
		buf[n] = '-'
	}
	return string(buf[n:])
}

func itoa64(i int64) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var buf [20]byte
	n := len(buf)
	for i > 0 {
		n--
		buf[n] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		n--
		buf[n] = '-'
	}
	return string(buf[n:])
}