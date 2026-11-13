package appdriver

import (
	"testing"

	"github.com/fauzone/kelvios/internal/apps"
)

func TestFakeDriver_InstallStartStopRemove(t *testing.T) {
	d := NewFakeDriver(1024) // 1GB limit

	app := apps.App{
		ID:     "test-app",
		Name:   "Test App",
		Image:  "nginx:alpine",
		Ports:  []int{8080},
		RAMMB:  128,
	}

	// Install
	if err := d.Install(app); err != nil {
		t.Fatalf("Install failed: %v", err)
	}

	// Verify container created
	c, ok := d.GetContainer(app.ID)
	if !ok {
		t.Fatal("container not found after install")
	}
	if c.status != ContainerStatusCreated {
		t.Errorf("expected status created, got %s", c.status)
	}

	// Start
	if err := d.Start(app.ID); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	c, ok = d.GetContainer(app.ID)
	if !ok {
		t.Fatal("container not found after start")
	}
	if c.status != ContainerStatusRunning {
		t.Errorf("expected status running, got %s", c.status)
	}

	// Health check
	healthy, err := d.Health(app.ID)
	if err != nil {
		t.Fatalf("Health failed: %v", err)
	}
	if !healthy {
		t.Error("expected healthy=true")
	}

	// Stop
	if err := d.Stop(app.ID); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	c, ok = d.GetContainer(app.ID)
	if !ok {
		t.Fatal("container not found after stop")
	}
	if c.status != ContainerStatusStopped {
		t.Errorf("expected status stopped, got %s", c.status)
	}

	// Health after stop
	healthy, err = d.Health(app.ID)
	if err != nil {
		t.Fatalf("Health failed: %v", err)
	}
	if healthy {
		t.Error("expected healthy=false after stop")
	}

	// Remove
	if err := d.Remove(app.ID); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	_, ok = d.GetContainer(app.ID)
	if ok {
		t.Error("container should be removed")
	}

	// Health after remove
	_, err = d.Health(app.ID)
	if err == nil {
		t.Error("expected error for removed container")
	}
}

func TestFakeDriver_IdempotentOperations(t *testing.T) {
	d := NewFakeDriver(1024)

	app := apps.App{
		ID:     "idempotent-app",
		Name:   "Idempotent App",
		Image:  "nginx:alpine",
		Ports:  []int{8081},
		RAMMB:  64,
	}

	// Double install
	if err := d.Install(app); err != nil {
		t.Fatalf("first Install failed: %v", err)
	}
	if err := d.Install(app); err != nil {
		t.Fatalf("second Install failed: %v", err)
	}

	// Double start
	if err := d.Start(app.ID); err != nil {
		t.Fatalf("first Start failed: %v", err)
	}
	if err := d.Start(app.ID); err != nil {
		t.Fatalf("second Start failed: %v", err)
	}

	// Double stop
	if err := d.Stop(app.ID); err != nil {
		t.Fatalf("first Stop failed: %v", err)
	}
	if err := d.Stop(app.ID); err != nil {
		t.Fatalf("second Stop failed: %v", err)
	}

	// Double remove
	if err := d.Remove(app.ID); err != nil {
		t.Fatalf("first Remove failed: %v", err)
	}
	if err := d.Remove(app.ID); err != nil {
		t.Fatalf("second Remove failed: %v", err)
	}
}

func TestFakeDriver_PortConflict(t *testing.T) {
	d := NewFakeDriver(1024)

	app1 := apps.App{
		ID:     "app1",
		Name:   "App 1",
		Image:  "nginx:alpine",
		Ports:  []int{9000},
		RAMMB:  64,
	}
	app2 := apps.App{
		ID:     "app2",
		Name:   "App 2",
		Image:  "nginx:alpine",
		Ports:  []int{9000}, // same port
		RAMMB:  64,
	}

	if err := d.Install(app1); err != nil {
		t.Fatalf("Install app1 failed: %v", err)
	}

	err := d.Install(app2)
	if err == nil {
		t.Fatal("expected port conflict error")
	}
	if _, ok := err.(*ErrPortConflict); !ok {
		t.Errorf("expected ErrPortConflict, got %T: %v", err, err)
	}
}

func TestFakeDriver_RAMLimit(t *testing.T) {
	d := NewFakeDriver(256) // 256MB limit

	app1 := apps.App{
		ID:     "ram-app1",
		Name:   "RAM App 1",
		Image:  "nginx:alpine",
		Ports:  []int{9100},
		RAMMB:  200,
	}
	app2 := apps.App{
		ID:     "ram-app2",
		Name:   "RAM App 2",
		Image:  "nginx:alpine",
		Ports:  []int{9101},
		RAMMB:  100, // would exceed 256MB total
	}

	if err := d.Install(app1); err != nil {
		t.Fatalf("Install app1 failed: %v", err)
	}

	err := d.Install(app2)
	if err == nil {
		t.Fatal("expected resource exhausted error")
	}
	if _, ok := err.(*ErrResourceExhausted); !ok {
		t.Errorf("expected ErrResourceExhausted, got %T: %v", err, err)
	}
}

func TestFakeDriver_HealthToggle(t *testing.T) {
	d := NewFakeDriver(1024)

	app := apps.App{
		ID:     "health-app",
		Name:   "Health App",
		Image:  "nginx:alpine",
		Ports:  []int{9200},
		RAMMB:  64,
	}

	if err := d.Install(app); err != nil {
		t.Fatalf("Install failed: %v", err)
	}
	if err := d.Start(app.ID); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	healthy, err := d.Health(app.ID)
	if err != nil || !healthy {
		t.Fatalf("expected healthy, got %v err=%v", healthy, err)
	}

	d.SetHealthy(app.ID, false)

	healthy, err = d.Health(app.ID)
	if err != nil || healthy {
		t.Fatalf("expected unhealthy, got %v err=%v", healthy, err)
	}

	d.SetHealthy(app.ID, true)

	healthy, err = d.Health(app.ID)
	if err != nil || !healthy {
		t.Fatalf("expected healthy again, got %v err=%v", healthy, err)
	}
}

func TestFakeDriver_Status(t *testing.T) {
	d := NewFakeDriver(1024)

	app := apps.App{
		ID:     "status-app",
		Name:   "Status App",
		Image:  "nginx:alpine",
		Ports:  []int{9300},
		RAMMB:  64,
	}

	status, err := d.Status(app.ID)
	if err == nil {
		t.Error("expected error for unknown container")
	}
	if status != ContainerStatusUnknown {
		t.Errorf("expected unknown, got %s", status)
	}

	if err := d.Install(app); err != nil {
		t.Fatalf("Install failed: %v", err)
	}

	status, err = d.Status(app.ID)
	if err != nil || status != ContainerStatusCreated {
		t.Errorf("expected created, got %s err=%v", status, err)
	}

	if err := d.Start(app.ID); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	status, err = d.Status(app.ID)
	if err != nil || status != ContainerStatusRunning {
		t.Errorf("expected running, got %s err=%v", status, err)
	}

	if err := d.Stop(app.ID); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	status, err = d.Status(app.ID)
	if err != nil || status != ContainerStatusStopped {
		t.Errorf("expected stopped, got %s err=%v", status, err)
	}

	if err := d.Remove(app.ID); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	status, err = d.Status(app.ID)
	if err == nil || status != ContainerStatusUnknown {
		t.Errorf("expected unknown after remove, got %s err=%v", status, err)
	}
}

func TestFakeDriver_MultiplePorts(t *testing.T) {
	d := NewFakeDriver(1024)

	app := apps.App{
		ID:     "multi-port",
		Name:   "Multi Port App",
		Image:  "nginx:alpine",
		Ports:  []int{9400, 9401, 9402},
		RAMMB:  128,
	}

	if err := d.Install(app); err != nil {
		t.Fatalf("Install failed: %v", err)
	}
	if err := d.Start(app.ID); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	healthy, err := d.Health(app.ID)
	if err != nil || !healthy {
		t.Fatalf("expected healthy, got %v err=%v", healthy, err)
	}

	// Verify ports are reserved
	d.mu.RLock()
	for _, p := range app.Ports {
		if owner, ok := d.ports[p]; !ok || owner != app.ID {
			t.Errorf("port %d not reserved for app", p)
		}
	}
	d.mu.RUnlock()

	if err := d.Remove(app.ID); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	// Verify ports released
	d.mu.RLock()
	for _, p := range app.Ports {
		if _, ok := d.ports[p]; ok {
			t.Errorf("port %d not released after remove", p)
		}
	}
	d.mu.RUnlock()
}

func TestFakeDriver_ConcurrentAccess(t *testing.T) {
	d := NewFakeDriver(1024)

	app := apps.App{
		ID:     "concurrent-app",
		Name:   "Concurrent App",
		Image:  "nginx:alpine",
		Ports:  []int{9500},
		RAMMB:  64,
	}

	if err := d.Install(app); err != nil {
		t.Fatalf("Install failed: %v", err)
	}

	done := make(chan error, 100)
	for i := 0; i < 50; i++ {
		go func() {
			done <- d.Start(app.ID)
		}()
		go func() {
			done <- d.Stop(app.ID)
		}()
		go func() {
			_, _ = d.Health(app.ID)
			done <- nil
		}()
		go func() {
			_, _ = d.Status(app.ID)
			done <- nil
		}()
	}

	for i := 0; i < 100; i++ {
		if err := <-done; err != nil {
			t.Errorf("concurrent operation error: %v", err)
		}
	}
}

func TestFakeDriver_NoRAMLimit(t *testing.T) {
	d := NewFakeDriver(0) // no limit

	app := apps.App{
		ID:     "no-limit-app",
		Name:   "No Limit App",
		Image:  "nginx:alpine",
		Ports:  []int{9600},
		RAMMB:  10000, // huge, but no limit
	}

	if err := d.Install(app); err != nil {
		t.Fatalf("Install failed: %v", err)
	}
}

// TestFakeDriverRollbackUpdate tests rollback to previous image.
func TestFakeDriverRollbackUpdate(t *testing.T) {
	d := NewFakeDriver(1024)

	app := apps.App{
		ID:     "rollback-app",
		Name:   "Rollback App",
		Image:  "nginx:1.0",
		Ports:  []int{9700},
		RAMMB:  128,
	}

	// Install
	if err := d.Install(app); err != nil {
		t.Fatalf("Install failed: %v", err)
	}
	if err := d.Start(app.ID); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Update to new image
	newApp := app
	newApp.Image = "nginx:2.0"
	newApp.Ports = []int{9701}
	newApp.RAMMB = 256
	if err := d.Update(newApp, "nginx:2.0"); err != nil {
		t.Fatalf("Update failed: %v", err)
	}

	// Verify new image is set
	c, ok := d.GetContainer(app.ID)
	if !ok {
		t.Fatal("container not found after update")
	}
	if c.app.Image != "nginx:2.0" {
		t.Errorf("expected image nginx:2.0, got %s", c.app.Image)
	}
	if c.previousImage != "nginx:1.0" {
		t.Errorf("expected previousImage nginx:1.0, got %s", c.previousImage)
	}

	// Rollback
	if err := d.RollbackUpdate(app.ID); err != nil {
		t.Fatalf("RollbackUpdate failed: %v", err)
	}

	// Verify rolled back to previous image
	c, ok = d.GetContainer(app.ID)
	if !ok {
		t.Fatal("container not found after rollback")
	}
	if c.app.Image != "nginx:1.0" {
		t.Errorf("expected image nginx:1.0 after rollback, got %s", c.app.Image)
	}
	if c.app.Ports[0] != 9700 {
		t.Errorf("expected port 9700 after rollback, got %d", c.app.Ports[0])
	}
	if c.app.RAMMB != 128 {
		t.Errorf("expected RAMMB 128 after rollback, got %d", c.app.RAMMB)
	}
	// Previous image should be cleared (one generation)
	if c.previousImage != "" {
		t.Errorf("expected previousImage empty after rollback, got %s", c.previousImage)
	}

	// Health should be true after rollback
	healthy, err := d.Health(app.ID)
	if err != nil || !healthy {
		t.Fatalf("expected healthy after rollback, got %v err=%v", healthy, err)
	}
}

// TestFakeDriverRollbackUpdateClearsPrevious tests that previous image is cleared after rollback.
func TestFakeDriverRollbackUpdateClearsPrevious(t *testing.T) {
	d := NewFakeDriver(1024)

	app := apps.App{
		ID:     "rollback-clear-app",
		Name:   "Rollback Clear App",
		Image:  "nginx:1.0",
		Ports:  []int{9800},
		RAMMB:  128,
	}

	// Install
	if err := d.Install(app); err != nil {
		t.Fatalf("Install failed: %v", err)
	}
	if err := d.Start(app.ID); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Update to new image
	newApp := app
	newApp.Image = "nginx:2.0"
	newApp.Ports = []int{9801}
	newApp.RAMMB = 256
	if err := d.Update(newApp, "nginx:2.0"); err != nil {
		t.Fatalf("Update failed: %v", err)
	}

	// Verify previous image is set
	c, ok := d.GetContainer(app.ID)
	if !ok {
		t.Fatal("container not found after update")
	}
	if c.previousImage != "nginx:1.0" {
		t.Errorf("expected previousImage nginx:1.0, got %s", c.previousImage)
	}

	// Rollback
	if err := d.RollbackUpdate(app.ID); err != nil {
		t.Fatalf("RollbackUpdate failed: %v", err)
	}

	// Verify previous image is cleared (one generation retention)
	c, ok = d.GetContainer(app.ID)
	if !ok {
		t.Fatal("container not found after rollback")
	}
	if c.previousImage != "" {
		t.Errorf("expected previousImage empty after rollback (one generation), got %s", c.previousImage)
	}
	if c.previousPorts != nil {
		t.Errorf("expected previousPorts nil after rollback, got %v", c.previousPorts)
	}
	if c.previousRAMMB != 0 {
		t.Errorf("expected previousRAMMB 0 after rollback, got %d", c.previousRAMMB)
	}

	// Second rollback should fail (no previous image)
	err := d.RollbackUpdate(app.ID)
	if err == nil {
		t.Fatal("expected error for second rollback with no previous image")
	}
	// Should be ErrContainerNotFound (no previous image)
	if _, ok := err.(ErrContainerNotFound); !ok {
		t.Errorf("expected ErrContainerNotFound for second rollback, got %T: %v", err, err)
	}
}