// Command teststub is a minimal restic stand-in for tests: it implements
// just enough of the restic CLI (init, backup, snapshots, restore) with
// password validation, so HTTP and wrapper tests can run without a real
// restic binary on any platform. It is test tooling, not production code.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func repoFlag(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--repo" {
			return args[i+1]
		}
	}
	return ""
}

func flagValue(args []string, name string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	return ""
}

func validPassword(repo string) bool {
	stored, err := os.ReadFile(filepath.Join(repo, ".password"))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(stored)) == os.Getenv("RESTIC_PASSWORD")
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "ERROR: "+format+"\n", a...)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 {
		fail("no command")
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	repo := repoFlag(args)

	switch cmd {
	case "init":
		if repo == "" {
			fail("--repo required")
		}
		if _, err := os.Stat(filepath.Join(repo, "config")); err == nil {
			fail("repository already exists")
		}
		if err := os.MkdirAll(repo, 0o755); err != nil {
			fail("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(repo, "config"), []byte(`{"version":1,"id":"stub"}`), 0o644); err != nil {
			fail("write config: %v", err)
		}
		if err := os.WriteFile(filepath.Join(repo, ".password"), []byte(os.Getenv("RESTIC_PASSWORD")), 0o600); err != nil {
			fail("write password: %v", err)
		}
	case "backup":
		if repo == "" || !validPassword(repo) {
			fail("invalid password or repo")
		}
		source := ""
		for _, a := range args {
			if !strings.HasPrefix(a, "--") && a != repo {
				source = a
				break
			}
		}
		snapID := fmt.Sprintf("snap-%d", time.Now().UnixNano())
		dir := filepath.Join(repo, "snapshots")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fail("mkdir snapshots: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, snapID+".txt"), []byte(source), 0o644); err != nil {
			fail("write snapshot meta: %v", err)
		}
		out, _ := json.Marshal(map[string]any{"snapshot_id": snapID})
		fmt.Println(string(out))
	case "snapshots":
		if repo == "" || !validPassword(repo) {
			fail("invalid password or repo")
		}
		entries, _ := os.ReadDir(filepath.Join(repo, "snapshots"))
		list := []map[string]any{}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".txt") {
				id := strings.TrimSuffix(e.Name(), ".txt")
				list = append(list, map[string]any{"id": id, "short_id": id})
			}
		}
		out, _ := json.Marshal(list)
		fmt.Println(string(out))
	case "restore":
		if len(args) < 1 {
			fail("snapshot id required")
		}
		snapID := args[0]
		target := flagValue(args, "--target")
		include := flagValue(args, "--include")
		if repo == "" || target == "" || include == "" || !validPassword(repo) {
			fail("invalid restore args or password")
		}
		meta, err := os.ReadFile(filepath.Join(repo, "snapshots", snapID+".txt"))
		if err != nil {
			fail("snapshot not found")
		}
		src := filepath.Join(string(meta), include)
		content, err := os.ReadFile(src)
		if err != nil {
			fail("read source: %v", err)
		}
		dst := filepath.Join(target, include)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			fail("mkdir target: %v", err)
		}
		if err := os.WriteFile(dst, content, 0o644); err != nil {
			fail("write target: %v", err)
		}
	default:
		fail("unknown command %s", cmd)
	}
}
