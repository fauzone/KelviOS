// Command teststub is a minimal smartctl/snapraid stand-in for tests:
// it implements just enough of the CLI (smartctl -H, smartctl -t long, snapraid sync) with
// canned output and an exit-1 mode driven by env flag STUB_FAIL_STEP.
// It is test tooling, not production code.
package main

import (
	"fmt"
	"os"
	"strings"
)

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "ERROR: "+format+"\n", a...)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 {
		fail("no command")
	}

	// Determine which binary we're emulating based on argv[0] or env
	binName := os.Args[0]
	if strings.Contains(binName, "smartctl") || (len(os.Args) > 1 && (os.Args[1] == "-H" || os.Args[1] == "-t")) {
		runSmartctl(os.Args[1:])
	} else if strings.Contains(binName, "snapraid") || (len(os.Args) > 1 && os.Args[1] == "sync") {
		runSnapraid(os.Args[1:])
	} else {
		fail("unknown binary: %s", binName)
	}
}

func runSmartctl(args []string) {
	// Check for fail mode
	if os.Getenv("STUB_FAIL_STEP") == "smart" {
		fmt.Fprintln(os.Stderr, "SMART overall-health self-assessment test result: FAILED")
		os.Exit(1)
	}

	// Expect: smartctl -H /dev/<disk> OR smartctl -t long /dev/<disk>
	if len(args) < 2 {
		fail("smartctl: expected -H <device> or -t long <device>")
	}

	if args[0] == "-H" {
		// Health check
		_ = args[1] // device path, not used in stub output
		fmt.Printf("smartctl 7.4 2023-08-01 r5530 [x86_64-w64-mingw32] (local build)\n")
		fmt.Printf("Copyright (C) 2002-23, Bruce Allen, Christian Franke, www.smartmontools.org\n\n")
		fmt.Printf("=== START OF READ SMART DATA SECTION ===\n")
		fmt.Printf("SMART overall-health self-assessment test result: PASSED\n")
		fmt.Printf("\n")
		fmt.Printf("=== START OF READ SMART DATA SECTION ===\n")
		fmt.Printf("Device Model:     WDC WD40EFRX-68WT0N0\n")
		fmt.Printf("Serial Number:    WD-STUB-SMART\n")
		fmt.Printf("Firmware Version: 82.00A82\n")
		fmt.Printf("User Capacity:    4,000,787,030,016 bytes [4.00 TB]\n")
		fmt.Printf("Sector Size:      512 bytes logical/physical\n")
		fmt.Printf("Rotation Rate:    5400 rpm\n")
		fmt.Printf("Device is:        Not in smartctl database [for details use: -P showall]\n")
		fmt.Printf("ATA Version is:   ACS-3 T13/2161-D revision 5\n")
		fmt.Printf("SATA Version is:  SATA 3.2, 6.0 Gb/s (current: 6.0 Gb/s)\n")
		fmt.Printf("Local Time is:    Mon Nov  3 17:30:00 2026 UTC\n")
		fmt.Printf("SMART support is: Available - device has SMART capability.\n")
		fmt.Printf("SMART support is: Enabled\n")
		fmt.Printf("\n")
		fmt.Printf("=== START OF READ SMART DATA SECTION ===\n")
		fmt.Printf("SMART overall-health self-assessment test result: PASSED\n")
	} else if args[0] == "-t" && len(args) >= 3 && args[1] == "long" {
		// SMART long test initiation
		_ = args[2] // device path
		fmt.Printf("smartctl 7.4 2023-08-01 r5530 [x86_64-w64-mingw32] (local build)\n")
		fmt.Printf("Copyright (C) 2002-23, Bruce Allen, Christian Franke, www.smartmontools.org\n\n")
		fmt.Printf("=== START OF OFFLINE IMMEDIATE AND SELF-TEST SECTION ===\n")
		fmt.Printf("Sending command: \"Execute SMART Extended self-test routine immediately in off-line mode\"\n")
		fmt.Printf("Drive command \"Execute SMART Extended self-test routine immediately in off-line mode\" successful.\n")
		fmt.Printf("Testing has begun.\n")
		fmt.Printf("Please wait 180 minutes for test to complete.\n")
		fmt.Printf("Test will complete after Mon Nov  3 20:30:00 2026\n")
		fmt.Printf("Use smartctl -X to abort test.\n")
	} else {
		fail("smartctl: expected -H <device> or -t long <device>, got %v", args)
	}
}

func runSnapraid(args []string) {
	// Check for fail mode
	if os.Getenv("STUB_FAIL_STEP") == "parity" {
		fmt.Fprintln(os.Stderr, "snapraid: sync failed")
		os.Exit(1)
	}

	// Expect: snapraid [-c conf] sync
	if len(args) < 1 || args[0] != "sync" {
		fail("snapraid: expected sync command")
	}

	fmt.Printf("Self test...\n")
	fmt.Printf("Loading state from /tmp/snapraid.content...\n")
	fmt.Printf("Scanning...\n")
	fmt.Printf("All files are intact.\n")
	fmt.Printf("Nothing to do. All files are already in sync.\n")
	fmt.Printf("SnapRAID status: SUCCESS\n")
}