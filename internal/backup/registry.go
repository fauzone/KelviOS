package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/fauzone/kelvios/internal/catalog"
)

// ProviderDef defines a backup provider type and how to build its restic
// repository URL and credential environment from config/secrets.
type ProviderDef struct {
	Type           string
	RequiredConfig []string
	RequiredSecret []string
	// BuildRepoURL constructs the restic repository URL from non-secret config.
	BuildRepoURL func(config map[string]string) (string, error)
	// CredentialEnv returns the environment variables to set for credentials.
	// The secret map is the decrypted JSON from the vault (by secret_name).
	CredentialEnv func(secret map[string]string) map[string]string
}

// secretKeysToReject are keys that must NOT appear in config_json — they
// belong only in the vault secret referenced by secret_name.
var secretKeysToReject = map[string]bool{
	"access_key":  true,
	"secret_key":  true,
	"app_key":     true,
	"password":    true,
	"token":       true,
	"account_id":  true,
	"account_key": true,
}

// ValidateConfigJSON returns an error if config contains any secret keys.
func ValidateConfigJSON(configJSON string) error {
	if configJSON == "" {
		return nil
	}
	var cfg map[string]string
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return fmt.Errorf("invalid config_json: %w", err)
	}
	for k := range cfg {
		if secretKeysToReject[strings.ToLower(k)] {
			return fmt.Errorf("config_json must not contain secret key %q; use secret_name instead", k)
		}
	}
	return nil
}

// Registry holds all known provider definitions.
var Registry = map[string]*ProviderDef{
	"local": {
		Type:           "local",
		RequiredConfig: []string{},
		RequiredSecret: []string{},
		BuildRepoURL: func(config map[string]string) (string, error) {
			// For local, target_path is passed separately (not in config).
			// This should not be called for local; handler uses target_path directly.
			return "", errors.New("local provider uses target_path directly, not BuildRepoURL")
		},
		CredentialEnv: func(secret map[string]string) map[string]string {
			return nil
		},
	},
	"s3": {
		Type:           "s3",
		RequiredConfig: []string{"endpoint", "bucket"},
		RequiredSecret: []string{"access_key", "secret_key"},
		BuildRepoURL: func(config map[string]string) (string, error) {
			endpoint := config["endpoint"]
			bucket := config["bucket"]
			prefix := config["prefix"]
			if endpoint == "" || bucket == "" {
				return "", errors.New("s3 requires endpoint and bucket in config")
			}
			// restic s3 URL format: s3:<endpoint>/<bucket>[/<prefix>]
			// endpoint should be like "s3.amazonaws.com" or "https://s3.amazonaws.com"
			url := fmt.Sprintf("s3:%s/%s", endpoint, bucket)
			if prefix != "" {
				url += "/" + strings.Trim(prefix, "/")
			}
			return url, nil
		},
		CredentialEnv: func(secret map[string]string) map[string]string {
			env := make(map[string]string)
			if v, ok := secret["access_key"]; ok {
				env["AWS_ACCESS_KEY_ID"] = v
			}
			if v, ok := secret["secret_key"]; ok {
				env["AWS_SECRET_ACCESS_KEY"] = v
			}
			return env
		},
	},
	"b2": {
		Type:           "b2",
		RequiredConfig: []string{"bucket"},
		RequiredSecret: []string{"account_id", "app_key"},
		BuildRepoURL: func(config map[string]string) (string, error) {
			bucket := config["bucket"]
			prefix := config["prefix"]
			if bucket == "" {
				return "", errors.New("b2 requires bucket in config")
			}
			// restic b2 URL format: b2:<bucket>[:<prefix>]
			url := fmt.Sprintf("b2:%s", bucket)
			if prefix != "" {
				url += ":" + strings.Trim(prefix, "/")
			}
			return url, nil
		},
		CredentialEnv: func(secret map[string]string) map[string]string {
			env := make(map[string]string)
			if v, ok := secret["account_id"]; ok {
				env["B2_ACCOUNT_ID"] = v
			}
			if v, ok := secret["app_key"]; ok {
				env["B2_ACCOUNT_KEY"] = v
			}
			return env
		},
	},
	"sftp": {
		Type:           "sftp",
		RequiredConfig: []string{"user", "host", "path"},
		RequiredSecret: []string{}, // key-based auth is host's concern in v1
		BuildRepoURL: func(config map[string]string) (string, error) {
			user := config["user"]
			host := config["host"]
			path := config["path"]
			if user == "" || host == "" || path == "" {
				return "", errors.New("sftp requires user, host, and path in config")
			}
			// restic sftp URL format: sftp:<user>@<host>:<path>
			return fmt.Sprintf("sftp:%s@%s:%s", user, host, path), nil
		},
		CredentialEnv: func(secret map[string]string) map[string]string {
			// v1: key-based auth is configured on the host (authorized_keys).
			// No secret env vars required. Documented for future extension.
			return nil
		},
	},
	"gdrive": {
		Type:           "gdrive",
		RequiredConfig: []string{"remote", "path"},
		RequiredSecret: []string{}, // rclone remote configured outside core in v1
		BuildRepoURL: func(config map[string]string) (string, error) {
			remote := config["remote"]
			path := config["path"]
			if remote == "" || path == "" {
				return "", errors.New("gdrive requires remote and path in config")
			}
			// restic rclone backend URL format: rclone:<remote>:<path>
			// The rclone remote (named in config["remote"]) must be pre-configured
			// via `rclone config` outside the core. The rclone binary is invoked
			// via KELVIOS_RCLONE_BIN env (default "rclone"), same adapter pattern
			// as restic/snapraid/smartctl.
			return fmt.Sprintf("rclone:%s:%s", remote, strings.Trim(path, "/")), nil
		},
		CredentialEnv: func(secret map[string]string) map[string]string {
			// v1: rclone remote holds credentials. No additional env needed.
			// If restic's rclone backend needs RCLONE_CONFIG_PASS, that would
			// come from the rclone config file, not from our vault.
			return nil
		},
	},
}

// GetProvider returns the provider definition for the given type,
// or nil if unknown.
func GetProvider(targetType string) *ProviderDef {
	return Registry[strings.ToLower(targetType)]
}

// BuildRepoURLAndEnv builds the repository URL and credential environment
// for a repo row. It fetches the secret from the vault if secret_name is set.
// The vault must be unlocked.
// SecretGetter is the minimal vault surface BuildRepoURLAndEnv needs: fetching
// a secret's JSON value by name. *vault.Vault satisfies it; tests use a mock.
// (Same abstraction pattern as wakecheck.Deps.VaultGetSecret in WS42-A.)
type SecretGetter interface {
	Get(name string) (string, error)
}

func BuildRepoURLAndEnv(repo *catalog.BackupRepoRow, v SecretGetter) (string, map[string]string, error) {
	provider := GetProvider(repo.TargetType)
	if provider == nil {
		return "", nil, fmt.Errorf("unknown target_type %q", repo.TargetType)
	}

	var repoURL string
	var err error

	if repo.TargetType == "local" {
		repoURL = repo.TargetPath
	} else {
		var config map[string]string
		if repo.ConfigJSON != "" {
			if err := json.Unmarshal([]byte(repo.ConfigJSON), &config); err != nil {
				return "", nil, fmt.Errorf("parse config_json: %w", err)
			}
		}
		repoURL, err = provider.BuildRepoURL(config)
		if err != nil {
			return "", nil, err
		}
	}

	var credEnv map[string]string
	if repo.SecretName != "" && v != nil {
		secretVal, err := v.Get(repo.SecretName)
		if err != nil {
			return "", nil, fmt.Errorf("fetch secret %q: %w", repo.SecretName, err)
		}
		var secret map[string]string
		if err := json.Unmarshal([]byte(secretVal), &secret); err != nil {
			return "", nil, fmt.Errorf("parse secret JSON: %w", err)
		}
		credEnv = provider.CredentialEnv(secret)
	}

	return repoURL, credEnv, nil
}