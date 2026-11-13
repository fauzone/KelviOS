package ups

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeNUTServer starts a fake NUT server that responds with canned responses.
// Returns the address and a cleanup function.
func fakeNUTServer(t *testing.T, responses map[string][]string) (string, func()) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()

	done := make(chan struct{})
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				select {
				case <-done:
					return
				default:
					continue
				}
			}
			go func(c net.Conn) {
				defer c.Close()
				scanner := bufio.NewScanner(c)
				for scanner.Scan() {
					cmd := strings.TrimSpace(scanner.Text())
					if resp, ok := responses[cmd]; ok {
						for _, line := range resp {
							c.Write([]byte(line + "\n"))
						}
					} else {
						c.Write([]byte("ERR unknown command\n"))
					}
				}
			}(conn)
		}
	}()

	cleanup := func() {
		close(done)
		l.Close()
	}
	return addr, cleanup
}

func TestClient_ListUPS(t *testing.T) {
	responses := map[string][]string{
		"LIST UPS": {
			"BEGIN LIST UPS",
			"UPS ups1 \"Primary UPS\"",
			"UPS ups2 \"Secondary UPS\"",
			"END LIST UPS",
		},
	}
	addr, cleanup := fakeNUTServer(t, responses)
	defer cleanup()

	client := &Client{Addr: addr, Timeout: 2 * time.Second, UPSName: "ups1"}
	names, err := client.ListUPS()
	if err != nil {
		t.Fatalf("ListUPS error: %v", err)
	}
	if len(names) != 2 || names[0] != "ups1" || names[1] != "ups2" {
		t.Errorf("ListUPS = %v, want [ups1 ups2]", names)
	}
}

func TestClient_ListUPS_Error(t *testing.T) {
	responses := map[string][]string{
		"LIST UPS": {
			"ERR ACCESS DENIED",
		},
	}
	addr, cleanup := fakeNUTServer(t, responses)
	defer cleanup()

	client := &Client{Addr: addr, Timeout: 2 * time.Second, UPSName: "ups1"}
	_, err := client.ListUPS()
	if err == nil {
		t.Fatal("expected error for ERR response")
	}
	if !strings.Contains(err.Error(), "ACCESS DENIED") {
		t.Errorf("error = %v, want contains ACCESS DENIED", err)
	}
}

func TestClient_GetVars(t *testing.T) {
	responses := map[string][]string{
		"LIST VAR ups1": {
			"BEGIN LIST VAR ups1",
			`VAR ups1 ups.status "OL"`,
			`VAR ups1 battery.charge "100.0"`,
			`VAR ups1 battery.runtime "3600"`,
			`VAR ups1 ups.model "Smart-UPS 1500"`,
			`VAR ups1 ups.serial "ABC123"`,
			"END LIST VAR ups1",
		},
	}
	addr, cleanup := fakeNUTServer(t, responses)
	defer cleanup()

	client := &Client{Addr: addr, Timeout: 2 * time.Second, UPSName: "ups1"}
	vars, err := client.GetVars()
	if err != nil {
		t.Fatalf("GetVars error: %v", err)
	}
	expected := map[string]string{
		"ups.status":      "OL",
		"battery.charge":  "100.0",
		"battery.runtime": "3600",
		"ups.model":       "Smart-UPS 1500",
		"ups.serial":      "ABC123",
	}
	for k, v := range expected {
		if vars[k] != v {
			t.Errorf("vars[%q] = %q, want %q", k, vars[k], v)
		}
	}
}

func TestClient_GetVars_QuotedValueWithSpaces(t *testing.T) {
	responses := map[string][]string{
		"LIST VAR ups1": {
			"BEGIN LIST VAR ups1",
			`VAR ups1 ups.model "Smart UPS 1500 VA"`,
			"END LIST VAR ups1",
		},
	}
	addr, cleanup := fakeNUTServer(t, responses)
	defer cleanup()

	client := &Client{Addr: addr, Timeout: 2 * time.Second, UPSName: "ups1"}
	vars, err := client.GetVars()
	if err != nil {
		t.Fatalf("GetVars error: %v", err)
	}
	if vars["ups.model"] != "Smart UPS 1500 VA" {
		t.Errorf("vars[ups.model] = %q, want \"Smart UPS 1500 VA\"", vars["ups.model"])
	}
}

func TestClient_GetVars_Error(t *testing.T) {
	responses := map[string][]string{
		"LIST VAR ups1": {
			"ERR UNKNOWN UPS",
		},
	}
	addr, cleanup := fakeNUTServer(t, responses)
	defer cleanup()

	client := &Client{Addr: addr, Timeout: 2 * time.Second, UPSName: "ups1"}
	_, err := client.GetVars()
	if err == nil {
		t.Fatal("expected error for ERR response")
	}
	if !strings.Contains(err.Error(), "UNKNOWN UPS") {
		t.Errorf("error = %v, want contains UNKNOWN UPS", err)
	}
}

func TestClient_GetVars_Timeout(t *testing.T) {
	// Server that accepts but never responds
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	done := make(chan struct{})
	go func() {
		conn, _ := l.Accept()
		if conn != nil {
			// Hold connection open, never respond
			<-done
			conn.Close()
		}
	}()
	defer func() { close(done); l.Close() }()

	client := &Client{Addr: addr, Timeout: 500 * time.Millisecond, UPSName: "ups1"}
	_, err = client.GetVars()
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timeout") && !strings.Contains(err.Error(), "i/o timeout") && !strings.Contains(err.Error(), "deadline") {
		t.Logf("timeout error (acceptable): %v", err)
	}
}

func TestClient_GetVars_EmptyVars(t *testing.T) {
	responses := map[string][]string{
		"LIST VAR ups1": {
			"BEGIN LIST VAR ups1",
			"END LIST VAR ups1",
		},
	}
	addr, cleanup := fakeNUTServer(t, responses)
	defer cleanup()

	client := &Client{Addr: addr, Timeout: 2 * time.Second, UPSName: "ups1"}
	vars, err := client.GetVars()
	if err != nil {
		t.Fatalf("GetVars error: %v", err)
	}
	if len(vars) != 0 {
		t.Errorf("vars = %v, want empty map", vars)
	}
}

func TestClient_Defaults(t *testing.T) {
	// Test that defaults are used when not set
	client := &Client{UPSName: "test"}
	if client.Addr != "" {
		t.Errorf("default Addr should be empty before dial, got %q", client.Addr)
	}
	if client.Timeout != 0 {
		t.Errorf("default Timeout should be 0 before dial, got %v", client.Timeout)
	}
	// We can't easily test the actual default values without a server,
	// but we verify the constants exist.
	_ = defaultNUTAddr
	_ = defaultNUTTimeout
}

func TestClient_UPSNameRequired(t *testing.T) {
	client := &Client{Addr: "127.0.0.1:3493", Timeout: time.Second}
	_, err := client.GetVars()
	if err == nil {
		t.Fatal("expected error for empty UPSName")
	}
	if !strings.Contains(err.Error(), "UPSName not set") {
		t.Errorf("error = %v, want contains UPSName not set", err)
	}
}
