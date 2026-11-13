package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	HTTP     HTTPConfig     `toml:"http"`
	Database DatabaseConfig `toml:"database"`
	UPS      UPSConfig      `toml:"ups"`
	Docker   DockerConfig   `toml:"docker"`
	OTA      OTAConfig      `toml:"ota"`
}

type HTTPConfig struct {
	Addr         string        `toml:"addr"`
	ReadTimeout  time.Duration `toml:"read_timeout"`
	WriteTimeout time.Duration `toml:"write_timeout"`
	IdleTimeout  time.Duration `toml:"idle_timeout"`
}

type DatabaseConfig struct {
	Path string `toml:"path"`
}

type UPSConfig struct {
	Addr             string   `toml:"addr"`
	UPSName          string   `toml:"ups_name"`
	CriticalPercent  int      `toml:"critical_percent"`
	ShutdownCommand  []string `toml:"shutdown_command"`
	MarkerPath       string   `toml:"marker_path"`
}

type DockerConfig struct {
	SocketPath string `toml:"socket_path"`
}

type OTAConfig struct {
	PublicKey     string `toml:"public_key"`
	PublicKeyFile string `toml:"public_key_file"`
	Channel       string `toml:"channel"`
	DropDir       string `toml:"drop_dir"`
}

func defaultConfig() *Config {
	home, _ := os.UserHomeDir()
	defaultDBPath := filepath.Join(home, ".kelvios", "catalog.db")

	return &Config{
		HTTP: HTTPConfig{
			Addr:         ":8080",
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  60 * time.Second,
		},
		Database: DatabaseConfig{
			Path: defaultDBPath,
		},
		UPS:    UPSConfig{},
		Docker: DockerConfig{},
		OTA: OTAConfig{
			Channel: "stable",
		},
	}
}

func Load() (*Config, error) {
	cfg := defaultConfig()

	configPaths := []string{
		"/etc/kelvios/config.toml",
		filepath.Join(os.Getenv("HOME"), ".config", "kelvios", "config.toml"),
		"./config.toml",
	}

	for _, path := range configPaths {
		if _, err := os.Stat(path); err == nil {
			if _, err := toml.DecodeFile(path, cfg); err != nil {
				return nil, err
			}
			break
		}
	}

	if addr := os.Getenv("KELVIOS_HTTP_ADDR"); addr != "" {
		cfg.HTTP.Addr = addr
	}
	if path := os.Getenv("KELVIOS_DB_PATH"); path != "" {
		cfg.Database.Path = path
	}
	if addr := os.Getenv("KELVIOS_UPS_ADDR"); addr != "" {
		cfg.UPS.Addr = addr
	}
	if name := os.Getenv("KELVIOS_UPS_NAME"); name != "" {
		cfg.UPS.UPSName = name
	}
	if cp := os.Getenv("KELVIOS_UPS_CRITICAL_PERCENT"); cp != "" {
		if v, err := strconv.Atoi(cp); err == nil {
			cfg.UPS.CriticalPercent = v
		}
	}
	// Note: the env form is split on plain spaces (no quoting/escaping),
	// so command arguments containing spaces are not supported here -
	// use the TOML ups.shutdown_command list for those. (Audit Pass 1 rec #3.)
	if cmd := os.Getenv("KELVIOS_UPS_SHUTDOWN_COMMAND"); cmd != "" {
		cfg.UPS.ShutdownCommand = strings.Split(cmd, " ")
	}
	if mp := os.Getenv("KELVIOS_UPS_MARKER_PATH"); mp != "" {
		cfg.UPS.MarkerPath = mp
	}
	if socket := os.Getenv("KELVIOS_DOCKER_SOCKET"); socket != "" {
		cfg.Docker.SocketPath = socket
	}
	if pk := os.Getenv("KELVIOS_OTA_PUBLIC_KEY"); pk != "" {
		cfg.OTA.PublicKey = pk
	}
	if pkf := os.Getenv("KELVIOS_OTA_PUBLIC_KEY_FILE"); pkf != "" {
		cfg.OTA.PublicKeyFile = pkf
	}
	if ch := os.Getenv("KELVIOS_OTA_CHANNEL"); ch != "" {
		cfg.OTA.Channel = ch
	}
	if dd := os.Getenv("KELVIOS_OTA_DROP_DIR"); dd != "" {
		cfg.OTA.DropDir = dd
	}

	return cfg, nil
}