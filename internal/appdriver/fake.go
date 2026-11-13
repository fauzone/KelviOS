package appdriver

import (
	"context"
	"sync"
	"time"

	"github.com/fauzone/kelvios/internal/apps"
)

// FakeDriver is an in-memory driver for tests and local development.
// It simulates container lifecycle without Docker.
type FakeDriver struct {
	mu         sync.RWMutex
	containers map[string]*fakeContainer
	ports      map[int]string // port -> appID
	ramUsed    int64
	ramLimit   int64
}

type fakeContainer struct {
	app               apps.App
	status            ContainerStatus
	created           time.Time
	started           time.Time
	healthy           bool
	previousImage     string // for rollback (WS55)
	previousPorts     []int  // previous ports for rollback
	previousRAMMB     int    // previous RAM for rollback
}

// NewFakeDriver creates a new fake driver with optional RAM limit (MB).
// If ramLimitMB <= 0, no RAM limit is enforced.
func NewFakeDriver(ramLimitMB int64) *FakeDriver {
	return &FakeDriver{
		containers: make(map[string]*fakeContainer),
		ports:      make(map[int]string),
		ramLimit:   ramLimitMB,
	}
}

func (d *FakeDriver) Name() string {
	return "fake"
}

func (d *FakeDriver) Install(app apps.App) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Check if already exists
	if _, exists := d.containers[app.ID]; exists {
		return nil // idempotent
	}

	// Check port conflicts
	for _, p := range app.Ports {
		if owner, ok := d.ports[p]; ok && owner != app.ID {
			return &ErrPortConflict{Port: p}
		}
	}

	// Check RAM limit
	appRAMMB := int64(app.RAMMB)
	if d.ramLimit > 0 {
		if d.ramUsed+appRAMMB > d.ramLimit {
			return &ErrResourceExhausted{RequestedMB: appRAMMB, AvailableMB: d.ramLimit - d.ramUsed}
		}
	}

	// Reserve ports
	for _, p := range app.Ports {
		d.ports[p] = app.ID
	}
	d.ramUsed += appRAMMB

	// Create container
	d.containers[app.ID] = &fakeContainer{
		app:     app,
		status:  ContainerStatusCreated,
		created: time.Now(),
		healthy: true,
	}
	return nil
}

func (d *FakeDriver) Start(appID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	c, ok := d.containers[appID]
	if !ok {
		return ErrContainerNotFound(appID)
	}
	if c.status == ContainerStatusRunning {
		return nil // idempotent
	}
	c.status = ContainerStatusRunning
	c.started = time.Now()
	return nil
}

func (d *FakeDriver) Stop(appID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	c, ok := d.containers[appID]
	if !ok {
		return ErrContainerNotFound(appID)
	}
	if c.status == ContainerStatusStopped || c.status == ContainerStatusRemoved {
		return nil // idempotent
	}
	// Release RAM on stop (T6)
	appRAMMB := int64(c.app.RAMMB)
	d.ramUsed -= appRAMMB
	if d.ramUsed < 0 {
		d.ramUsed = 0
	}
	c.status = ContainerStatusStopped
	c.healthy = false
	return nil
}

// Kill forcefully stops and removes the container, releasing resources (T6).
func (d *FakeDriver) Kill(appID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	c, ok := d.containers[appID]
	if !ok {
		return ErrContainerNotFound(appID)
	}

	// Release ports
	for _, p := range c.app.Ports {
		if d.ports[p] == appID {
			delete(d.ports, p)
		}
	}
	// Release RAM
	appRAMMB := int64(c.app.RAMMB)
	d.ramUsed -= appRAMMB
	if d.ramUsed < 0 {
		d.ramUsed = 0
	}

	c.status = ContainerStatusRemoved
	delete(d.containers, appID)
	return nil
}

func (d *FakeDriver) Remove(appID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	c, ok := d.containers[appID]
	if !ok {
		return nil // idempotent: already removed
	}

	// Release ports
	for _, p := range c.app.Ports {
		if d.ports[p] == appID {
			delete(d.ports, p)
		}
	}
	appRAMMB := int64(c.app.RAMMB)
	d.ramUsed -= appRAMMB
	if d.ramUsed < 0 {
		d.ramUsed = 0
	}

	c.status = ContainerStatusRemoved
	delete(d.containers, appID)
	return nil
}

// Update updates the app container to a new target image (WS55).
// Retains previous image/ports/RAM for rollback (one generation).
func (d *FakeDriver) Update(app apps.App, targetImage string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	c, ok := d.containers[app.ID]
	if !ok {
		return ErrContainerNotFound(app.ID)
	}

	// Check port conflicts for new ports
	for _, p := range app.Ports {
		if owner, ok := d.ports[p]; ok && owner != app.ID {
			return &ErrPortConflict{Port: p}
		}
	}

	// Check RAM limit
	appRAMMB := int64(app.RAMMB)
	if d.ramLimit > 0 {
		// Calculate delta: new RAM - old RAM
		oldRAMMB := int64(c.app.RAMMB)
		if d.ramUsed-oldRAMMB+appRAMMB > d.ramLimit {
			return &ErrResourceExhausted{RequestedMB: appRAMMB, AvailableMB: d.ramLimit - (d.ramUsed - oldRAMMB)}
		}
	}

	// Store previous state for rollback
	c.previousImage = c.app.Image
	c.previousPorts = c.app.Ports
	c.previousRAMMB = c.app.RAMMB

	// Release old ports
	for _, p := range c.app.Ports {
		if d.ports[p] == app.ID {
			delete(d.ports, p)
		}
	}
	// Reserve new ports
	for _, p := range app.Ports {
		d.ports[p] = app.ID
	}

	// Update RAM usage
	d.ramUsed -= int64(c.app.RAMMB)
	d.ramUsed += appRAMMB

	// Update container to new image
	c.app.Image = targetImage
	c.app.Ports = app.Ports
	c.app.RAMMB = app.RAMMB
	c.healthy = true // new image starts healthy
	c.status = ContainerStatusRunning // update restarts container

	return nil
}

// RollbackUpdate rolls back the app container to the previous image (WS55).
func (d *FakeDriver) RollbackUpdate(appID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	c, ok := d.containers[appID]
	if !ok {
		return ErrContainerNotFound(appID)
	}

	if c.previousImage == "" {
		return ErrContainerNotFound(appID) // no previous image to rollback to
	}

	// Release current ports
	for _, p := range c.app.Ports {
		if d.ports[p] == appID {
			delete(d.ports, p)
		}
	}

	// Restore previous state
	c.app.Image = c.previousImage
	c.app.Ports = c.previousPorts
	c.app.RAMMB = c.previousRAMMB

	// Reserve previous ports
	for _, p := range c.previousPorts {
		d.ports[p] = appID
	}

	// Update RAM usage
	d.ramUsed -= int64(c.app.RAMMB)
	d.ramUsed += int64(c.previousRAMMB)

	// Clear rollback state (one generation)
	c.previousImage = ""
	c.previousPorts = nil
	c.previousRAMMB = 0
	c.healthy = true

	return nil
}

func (d *FakeDriver) Health(appID string) (bool, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	c, ok := d.containers[appID]
	if !ok {
		return false, ErrContainerNotFound(appID)
	}
	if c.status != ContainerStatusRunning {
		return false, nil
	}
	return c.healthy, nil
}

func (d *FakeDriver) Status(appID string) (ContainerStatus, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	c, ok := d.containers[appID]
	if !ok {
		return ContainerStatusUnknown, ErrContainerNotFound(appID)
	}
	return c.status, nil
}

// SetHealthy allows tests to toggle health state.
func (d *FakeDriver) SetHealthy(appID string, healthy bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c, ok := d.containers[appID]; ok {
		c.healthy = healthy
		if !healthy && c.status == ContainerStatusRunning {
			c.status = ContainerStatusUnhealthy
		}
		if healthy && c.status == ContainerStatusUnhealthy {
			c.status = ContainerStatusRunning
		}
	}
}

// GetContainer returns the fake container for inspection (tests only).
func (d *FakeDriver) GetContainer(appID string) (*fakeContainer, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	c, ok := d.containers[appID]
	return c, ok
}

// GetRAMUsed returns the current RAM usage in MB (tests only).
func (d *FakeDriver) GetRAMUsed() int64 {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.ramUsed
}

// GetPortOwner returns the appID that owns the given port (tests only).
func (d *FakeDriver) GetPortOwner(port int) (string, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	owner, ok := d.ports[port]
	return owner, ok
}

// GetContainerImage returns the current image of the container (tests only).
func (d *FakeDriver) GetContainerImage(appID string) (string, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	c, ok := d.containers[appID]
	if !ok {
		return "", false
	}
	return c.app.Image, true
}

// GetContainerPreviousImage returns the previous image for rollback (tests only).
func (d *FakeDriver) GetContainerPreviousImage(appID string) (string, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	c, ok := d.containers[appID]
	if !ok {
		return "", false
	}
	return c.previousImage, true
}

// GetContainerPorts returns the current ports of the container (tests only).
func (d *FakeDriver) GetContainerPorts(appID string) ([]int, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	c, ok := d.containers[appID]
	if !ok {
		return nil, false
	}
	return c.app.Ports, true
}

// GetContainerRAMMB returns the current RAM of the container (tests only).
func (d *FakeDriver) GetContainerRAMMB(appID string) (int, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	c, ok := d.containers[appID]
	if !ok {
		return 0, false
	}
	return c.app.RAMMB, true
}

// Context-aware variants for future compatibility.
func (d *FakeDriver) InstallContext(ctx context.Context, app apps.App) error {
	return d.Install(app)
}
func (d *FakeDriver) StartContext(ctx context.Context, appID string) error {
	return d.Start(appID)
}
func (d *FakeDriver) StopContext(ctx context.Context, appID string) error {
	return d.Stop(appID)
}
func (d *FakeDriver) RemoveContext(ctx context.Context, appID string) error {
	return d.Remove(appID)
}
func (d *FakeDriver) HealthContext(ctx context.Context, appID string) (bool, error) {
	return d.Health(appID)
}
func (d *FakeDriver) StatusContext(ctx context.Context, appID string) (ContainerStatus, error) {
	return d.Status(appID)
}
func (d *FakeDriver) KillContext(ctx context.Context, appID string) error {
	return d.Kill(appID)
}
func (d *FakeDriver) UpdateContext(ctx context.Context, app apps.App, targetImage string) error {
	return d.Update(app, targetImage)
}
func (d *FakeDriver) RollbackUpdateContext(ctx context.Context, appID string) error {
	return d.RollbackUpdate(appID)
}
