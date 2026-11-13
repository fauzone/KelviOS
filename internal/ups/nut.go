package ups

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"time"
)

// Client is a NUT (Network UPS Tools) client using stdlib only.
type Client struct {
	Addr    string        // default "127.0.0.1:3493"
	Timeout time.Duration // default 5s
	UPSName string        // UPS name for LIST VAR
}

// VarSource is the interface for fetching UPS variables.
// Client implements this; tests can provide a fake.
type VarSource interface {
	GetVars() (map[string]string, error)
}

const (
	defaultNUTAddr    = "127.0.0.1:3493"
	defaultNUTTimeout = 5 * time.Second
)

// dial connects to the NUT server with timeout.
func (c *Client) dial() (net.Conn, error) {
	addr := c.Addr
	if addr == "" {
		addr = defaultNUTAddr
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = defaultNUTTimeout
	}
	dialer := &net.Dialer{Timeout: timeout}
	return dialer.Dial("tcp", addr)
}

// sendCommand sends a command and reads the response until END marker or error.
func (c *Client) sendCommand(cmd string, beginMarker, endMarker string) ([]string, error) {
	conn, err := c.dial()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// Set deadline for the entire operation
	deadline := time.Now().Add(c.Timeout)
	if c.Timeout <= 0 {
		deadline = time.Now().Add(defaultNUTTimeout)
	}
	_ = conn.SetDeadline(deadline)

	// Send command
	if _, err := conn.Write([]byte(cmd)); err != nil {
		return nil, err
	}

	// Read response lines
	var lines []string
	scanner := bufio.NewScanner(conn)
	inBlock := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "ERR") {
			return nil, fmt.Errorf("NUT error: %s", strings.TrimSpace(line))
		}
		if strings.HasPrefix(line, beginMarker) {
			inBlock = true
			continue
		}
		if strings.HasPrefix(line, endMarker) {
			break
		}
		if inBlock {
			lines = append(lines, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return lines, nil
}

// ListUPS returns a list of UPS names known to the NUT server.
func (c *Client) ListUPS() ([]string, error) {
	lines, err := c.sendCommand("LIST UPS\n", "BEGIN LIST UPS", "END LIST UPS")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Format: UPS <name> <description>
		parts := strings.SplitN(line, " ", 3)
		if len(parts) >= 2 && parts[0] == "UPS" {
			names = append(names, parts[1])
		}
	}
	return names, nil
}

// GetVars fetches all variables for the configured UPS.
// Returns a map of variable name -> value (quotes stripped).
func (c *Client) GetVars() (map[string]string, error) {
	if c.UPSName == "" {
		return nil, fmt.Errorf("UPSName not set")
	}
	cmd := "LIST VAR " + c.UPSName + "\n"
	lines, err := c.sendCommand(cmd, "BEGIN LIST VAR "+c.UPSName, "END LIST VAR "+c.UPSName)
	if err != nil {
		return nil, err
	}
	vars := make(map[string]string)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Format: VAR <ups> <var> "value"
		// Value may contain spaces inside quotes.
		prefix := "VAR " + c.UPSName + " "
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		rest := strings.TrimPrefix(line, prefix)
		// Find first space to separate var name from quoted value
		idx := strings.Index(rest, " ")
		if idx < 0 {
			continue
		}
		varName := rest[:idx]
		value := strings.TrimSpace(rest[idx+1:])
		// Strip surrounding quotes
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			value = value[1 : len(value)-1]
		}
		vars[varName] = value
	}
	return vars, nil
}

// Ensure Client implements VarSource
var _ VarSource = (*Client)(nil)
