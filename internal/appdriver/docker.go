package appdriver

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/fauzone/kelvios/internal/apps"
)

// DockerDriver implements Driver using Docker Engine API over Unix socket.
type DockerDriver struct {
	cli       *client.Client
	networkID string
	ramLimit  int64 // MB, 0 = no limit
}

// NewDockerDriver creates a new Docker driver.
// socketPath: path to Docker Unix socket (e.g., "/var/run/docker.sock").
// networkName: Docker network to attach containers to (created if not exists).
// ramLimitMB: system RAM limit in MB (0 = no limit).
func NewDockerDriver(socketPath, networkName string, ramLimitMB int64) (*DockerDriver, error) {
	cli, err := client.NewClientWithOpts(
		client.WithHost("unix://"+socketPath),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, ErrDriverUnavailable("create client: " + err.Error())
	}

	// Ensure network exists
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	networkID, err := ensureNetwork(ctx, cli, networkName)
	if err != nil {
		return nil, ErrDriverUnavailable("ensure network: " + err.Error())
	}

	return &DockerDriver{
		cli:       cli,
		networkID: networkID,
		ramLimit:  ramLimitMB,
	}, nil
}

func ensureNetwork(ctx context.Context, cli *client.Client, name string) (string, error) {
	result, err := cli.NetworkList(ctx, client.NetworkListOptions{})
	if err != nil {
		return "", err
	}
	for _, n := range result.Items {
		if n.Name == name {
			return n.ID, nil
		}
	}
	created, err := cli.NetworkCreate(ctx, name, client.NetworkCreateOptions{
		Driver: "bridge",
	})
	if err != nil {
		return "", err
	}
	return created.ID, nil
}

func (d *DockerDriver) Name() string {
	return "docker"
}

func (d *DockerDriver) Install(app apps.App) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Check if container already exists
	existing, err := d.findContainer(ctx, app.ID)
	if err != nil {
		return err
	}
	if existing != "" {
		// Container exists, ensure it's running
		return d.Start(app.ID)
	}

	// Check port conflicts on host
	for _, p := range app.Ports {
		if inUse, _ := isPortInUse(p); inUse {
			return &ErrPortConflict{Port: p}
		}
	}

	// Check system RAM (T5)
	if d.ramLimit > 0 {
		sysRAM, err := GetSystemRAMMB()
		if err != nil {
			return fmt.Errorf("get system RAM: %w", err)
		}
		if int64(app.RAMMB) > sysRAM {
			return fmt.Errorf("app RAM %d MB exceeds system RAM %d MB", app.RAMMB, sysRAM)
		}
	}

	// Pull image if needed
	if err := d.ensureImage(ctx, app.Image); err != nil {
		return err
	}

	// Create container
	hostConfig := &container.HostConfig{
		NetworkMode: container.NetworkMode(d.networkID),
		PortBindings: network.PortMap{},
		RestartPolicy: container.RestartPolicy{
			Name: "unless-stopped",
		},
		Mounts: []mount.Mount{
			{
				Type:   mount.TypeBind,
				Source: "/var/lib/kelvios/appdata/" + app.ID,
				Target: "/appdata",
			},
		},
	}
	// Unconditional memory limit (T5)
	if app.RAMMB > 0 {
		memBytes := int64(app.RAMMB) * 1024 * 1024
		hostConfig.Resources.Memory = memBytes
		hostConfig.Resources.MemorySwap = memBytes
	}

	exposedPorts := network.PortSet{}
	for _, p := range app.Ports {
		port := network.MustParsePort(strconv.Itoa(p) + "/tcp")
		exposedPorts[port] = struct{}{}
		hostIP := netip.MustParseAddr("0.0.0.0")
		hostConfig.PortBindings[port] = []network.PortBinding{
			{HostIP: hostIP, HostPort: strconv.Itoa(p)},
		}
	}

	// Store ports in labels for Health check (T4)
	portsLabel := ""
	for i, p := range app.Ports {
		if i > 0 {
			portsLabel += ","
		}
		portsLabel += strconv.Itoa(p)
	}

	containerName := "kelvios-" + app.ID
	createOpts := client.ContainerCreateOptions{
		Config: &container.Config{
			Image:        app.Image,
			ExposedPorts: exposedPorts,
			Env:          buildEnv(app),
			Labels: map[string]string{
				"kelvios.app.id":    app.ID,
				"kelvios.app.name":  app.Name,
				"kelvios.app.ports": portsLabel,
			},
		},
		HostConfig:       hostConfig,
		NetworkingConfig: &network.NetworkingConfig{},
		Name:             containerName,
	}
	resp, err := d.cli.ContainerCreate(ctx, createOpts)
	if err != nil {
		if strings.Contains(err.Error(), "port is already allocated") {
			// Try to detect which port
			for _, p := range app.Ports {
				if inUse, _ := isPortInUse(p); inUse {
					return &ErrPortConflict{Port: p}
				}
			}
		}
		return fmt.Errorf("create container: %w", err)
	}

	// Start container
	_, err = d.cli.ContainerStart(ctx, resp.ID, client.ContainerStartOptions{})
	if err != nil {
		// Cleanup on start failure
		_, _ = d.cli.ContainerRemove(ctx, resp.ID, client.ContainerRemoveOptions{Force: true})
		return fmt.Errorf("start container: %w", err)
	}

	return nil
}

func (d *DockerDriver) Start(appID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	id, err := d.findContainer(ctx, appID)
	if err != nil {
		return err
	}
	if id == "" {
		return ErrContainerNotFound(appID)
	}

	_, err = d.cli.ContainerStart(ctx, id, client.ContainerStartOptions{})
	return err
}

func (d *DockerDriver) Stop(appID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	id, err := d.findContainer(ctx, appID)
	if err != nil {
		return err
	}
	if id == "" {
		return ErrContainerNotFound(appID)
	}

	timeout := 10
	_, err = d.cli.ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: &timeout})
	return err
}

func (d *DockerDriver) Remove(appID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	id, err := d.findContainer(ctx, appID)
	if err != nil {
		return err
	}
	if id == "" {
		return ErrContainerNotFound(appID)
	}

	_, err = d.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	return err
}

func (d *DockerDriver) Health(appID string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	id, err := d.findContainer(ctx, appID)
	if err != nil {
		return false, err
	}
	if id == "" {
		return false, ErrContainerNotFound(appID)
	}

	inspectResult, err := d.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return false, err
	}

	inspect := inspectResult.Container
	if inspect.State == nil || inspect.State.Status != "running" {
		return false, nil
	}

	// Check all declared ports via TCP connect (T4: read ports from labels)
	portsLabel := inspect.Config.Labels["kelvios.app.ports"]
	if portsLabel == "" {
		// No ports declared, consider healthy if running
		return true, nil
	}

	for _, portStr := range strings.Split(portsLabel, ",") {
		p, err := strconv.Atoi(portStr)
		if err != nil {
			continue
		}
		if !tcpConnect("localhost", p, 2*time.Second) {
			return false, nil
		}
	}

	return true, nil
}

func (d *DockerDriver) Status(appID string) (ContainerStatus, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	id, err := d.findContainer(ctx, appID)
	if err != nil {
		return ContainerStatusUnknown, err
	}
	if id == "" {
		return ContainerStatusUnknown, ErrContainerNotFound(appID)
	}

	inspectResult, err := d.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return ContainerStatusUnknown, err
	}

	inspect := inspectResult.Container
	if inspect.State == nil {
		return ContainerStatusUnknown, nil
	}

	switch inspect.State.Status {
	case "created":
		return ContainerStatusCreated, nil
	case "running":
		return ContainerStatusRunning, nil
	case "exited", "dead":
		return ContainerStatusStopped, nil
	case "paused":
		return ContainerStatusStopped, nil
	case "removing":
		return ContainerStatusRemoved, nil
	default:
		return ContainerStatusUnknown, nil
	}
}

func (d *DockerDriver) findContainer(ctx context.Context, appID string) (string, error) {
	filters := client.Filters{}
	filters.Add("label", "kelvios.app.id="+appID)
	result, err := d.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: filters,
	})
	if err != nil {
		return "", err
	}
	if len(result.Items) > 0 {
		return result.Items[0].ID, nil
	}
	return "", nil
}

func (d *DockerDriver) getAppFromLabels(labels map[string]string) (apps.App, error) {
	appID := labels["kelvios.app.id"]
	appName := labels["kelvios.app.name"]
	if appID == "" {
		return apps.App{}, fmt.Errorf("missing kelvios.app.id label")
	}
	// We don't have full app info from labels alone; caller should provide app
	// For health check, we need ports. This is a limitation - we'll need to store ports in labels or pass app.
	return apps.App{ID: appID, Name: appName}, nil
}

func (d *DockerDriver) ensureImage(ctx context.Context, imageRef string) error {
	_, err := d.cli.ImageInspect(ctx, imageRef)
	if err == nil {
		return nil // already present
	}
	// Check if error is "not found" - use error message matching since no IsErrNotFound
	if !strings.Contains(err.Error(), "not found") && !strings.Contains(err.Error(), "No such image") {
		return fmt.Errorf("inspect image: %w", err)
	}

	// Pull image
	resp, err := d.cli.ImagePull(ctx, imageRef, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pull image: %w", err)
	}
	defer resp.Close()
	_, _ = io.Copy(io.Discard, resp)
	return nil
}

func buildEnv(app apps.App) []string {
	env := []string{
		"KELVIOS_APP_ID=" + app.ID,
		"KELVIOS_APP_NAME=" + app.Name,
	}
	// App struct doesn't have Env field in catalog
	return env
}

func isPortInUse(port int) (bool, error) {
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		if opErr, ok := err.(*net.OpError); ok && opErr.Err.Error() == "address already in use" {
			return true, nil
		}
		return false, err
	}
	ln.Close()
	return false, nil
}

func tcpConnect(host string, port int, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// Close closes the Docker client connection.
func (d *DockerDriver) Close() error {
	if d.cli != nil {
		return d.cli.Close()
	}
	return nil
}

// Kill forcefully stops and removes the container (T6).
func (d *DockerDriver) Kill(appID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	id, err := d.findContainer(ctx, appID)
	if err != nil {
		return err
	}
	if id == "" {
		return ErrContainerNotFound(appID)
	}

	// Force remove container
	_, err = d.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	return err
}

// Update updates the app container to a new target image (WS55).
// Renames current container to <name>-prev, creates new container with target image.
func (d *DockerDriver) Update(app apps.App, targetImage string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	id, err := d.findContainer(ctx, app.ID)
	if err != nil {
		return err
	}
	if id == "" {
		return ErrContainerNotFound(app.ID)
	}

	// Inspect current container to get its config
	inspectResult, err := d.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect current container: %w", err)
	}
	currentContainer := inspectResult.Container

	// Check port conflicts for new ports (excluding ports already used by this container)
	for _, p := range app.Ports {
		if inUse, _ := isPortInUse(p); inUse {
			// Check if it's our own container using the port
			isOurs := false
			for _, portBinding := range currentContainer.HostConfig.PortBindings {
				for _, binding := range portBinding {
					if binding.HostPort == strconv.Itoa(p) {
						isOurs = true
						break
					}
				}
			}
			if !isOurs {
				return &ErrPortConflict{Port: p}
			}
		}
	}

	// Check system RAM if limit set
	if d.ramLimit > 0 {
		sysRAM, err := GetSystemRAMMB()
		if err != nil {
			return fmt.Errorf("get system RAM: %w", err)
		}
		if int64(app.RAMMB) > sysRAM {
			return fmt.Errorf("app RAM %d MB exceeds system RAM %d MB", app.RAMMB, sysRAM)
		}
	}

	// Pull target image if needed
	if err := d.ensureImage(ctx, targetImage); err != nil {
		return err
	}

	// Rename current container to <name>-prev
	prevName := "kelvios-" + app.ID + "-prev"
	_, err = d.cli.ContainerRename(ctx, id, client.ContainerRenameOptions{NewName: prevName})
	if err != nil {
		return fmt.Errorf("rename container to prev: %w", err)
	}

	// Create new container with target image
	hostConfig := &container.HostConfig{
		NetworkMode: container.NetworkMode(d.networkID),
		PortBindings: network.PortMap{},
		RestartPolicy: container.RestartPolicy{
			Name: "unless-stopped",
		},
		Mounts: []mount.Mount{
			{
				Type:   mount.TypeBind,
				Source: "/var/lib/kelvios/appdata/" + app.ID,
				Target: "/appdata",
			},
		},
	}
	if app.RAMMB > 0 {
		memBytes := int64(app.RAMMB) * 1024 * 1024
		hostConfig.Resources.Memory = memBytes
		hostConfig.Resources.MemorySwap = memBytes
	}

	exposedPorts := network.PortSet{}
	for _, p := range app.Ports {
		port := network.MustParsePort(strconv.Itoa(p) + "/tcp")
		exposedPorts[port] = struct{}{}
		hostIP := netip.MustParseAddr("0.0.0.0")
		hostConfig.PortBindings[port] = []network.PortBinding{
			{HostIP: hostIP, HostPort: strconv.Itoa(p)},
		}
	}

	// Store ports in labels for Health check
	portsLabel := ""
	for i, p := range app.Ports {
		if i > 0 {
			portsLabel += ","
		}
		portsLabel += strconv.Itoa(p)
	}

	containerName := "kelvios-" + app.ID
	createOpts := client.ContainerCreateOptions{
		Config: &container.Config{
			Image:        targetImage,
			ExposedPorts: exposedPorts,
			Env:          buildEnv(app),
			Labels: map[string]string{
				"kelvios.app.id":    app.ID,
				"kelvios.app.name":  app.Name,
				"kelvios.app.ports": portsLabel,
			},
		},
		HostConfig:       hostConfig,
		NetworkingConfig: &network.NetworkingConfig{},
		Name:             containerName,
	}
	resp, err := d.cli.ContainerCreate(ctx, createOpts)
	if err != nil {
		// Try to rename prev back on failure
		_, _ = d.cli.ContainerRename(ctx, prevName, client.ContainerRenameOptions{NewName: containerName})
		if strings.Contains(err.Error(), "port is already allocated") {
			for _, p := range app.Ports {
				if inUse, _ := isPortInUse(p); inUse {
					return &ErrPortConflict{Port: p}
				}
			}
		}
		return fmt.Errorf("create container: %w", err)
	}

	// Start new container
	_, err = d.cli.ContainerStart(ctx, resp.ID, client.ContainerStartOptions{})
	if err != nil {
		// Cleanup on start failure
		_, _ = d.cli.ContainerRemove(ctx, resp.ID, client.ContainerRemoveOptions{Force: true})
		// Rename prev back
		_, _ = d.cli.ContainerRename(ctx, prevName, client.ContainerRenameOptions{NewName: containerName})
		return fmt.Errorf("start container: %w", err)
	}

	return nil
}

// RollbackUpdate rolls back the app container to the previous image (WS55).
// Removes new container, renames -prev back to original name, starts it.
func (d *DockerDriver) RollbackUpdate(appID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Find the new container (current)
	currentName := "kelvios-" + appID
	currentID, err := d.findContainer(ctx, appID)
	if err != nil {
		return err
	}
	if currentID == "" {
		return ErrContainerNotFound(appID)
	}

	// Find the prev container
	prevName := "kelvios-" + appID + "-prev"
	filters := client.Filters{}
	filters.Add("name", prevName)
	prevList, err := d.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: filters,
	})
	if err != nil {
		return err
	}
	if len(prevList.Items) == 0 {
		return ErrContainerNotFound(appID) // no previous container to rollback to
	}
	prevID := prevList.Items[0].ID

	// Remove the new container
	_, err = d.cli.ContainerRemove(ctx, currentID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	if err != nil {
		return fmt.Errorf("remove new container: %w", err)
	}

	// Rename prev back to original name
	_, err = d.cli.ContainerRename(ctx, prevID, client.ContainerRenameOptions{NewName: currentName})
	if err != nil {
		return fmt.Errorf("rename prev back: %w", err)
	}

	// Start the rolled-back container
	_, err = d.cli.ContainerStart(ctx, prevID, client.ContainerStartOptions{})
	if err != nil {
		return fmt.Errorf("start rolled-back container: %w", err)
	}

	return nil
}

// GetSystemRAMMB returns total system RAM in MB (Linux only).
func GetSystemRAMMB() (int64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, _ := strconv.ParseInt(fields[1], 10, 64)
				return kb / 1024, nil
			}
		}
	}
	return 0, fmt.Errorf("MemTotal not found in /proc/meminfo")
}