package backup

import (
	"encoding/json"
	"testing"

	"github.com/fauzone/kelvios/internal/catalog"
)

func TestProviderRegistry_GetProvider(t *testing.T) {
	tests := []struct {
		name       string
		targetType string
		wantNil    bool
	}{
		{"local", "local", false},
		{"s3", "s3", false},
		{"b2", "b2", false},
		{"sftp", "sftp", false},
		{"gdrive", "gdrive", false},
		{"unknown", "unknown", true},
		{"case-insensitive", "S3", false},
		{"empty", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := GetProvider(tc.targetType)
			if tc.wantNil {
				if p != nil {
					t.Errorf("GetProvider(%q) = %v, want nil", tc.targetType, p)
				}
			} else {
				if p == nil {
					t.Errorf("GetProvider(%q) = nil, want provider", tc.targetType)
				} else if p.Type != tc.targetType && !(tc.targetType == "S3" && p.Type == "s3") {
					t.Errorf("GetProvider(%q).Type = %q, want %q", tc.targetType, p.Type, tc.targetType)
				}
			}
		})
	}
}

func TestProvider_Local_BuildRepoURL(t *testing.T) {
	p := GetProvider("local")
	if p == nil {
		t.Fatal("local provider not found")
	}
	// Local provider's BuildRepoURL should return an error (uses target_path directly)
	_, err := p.BuildRepoURL(map[string]string{})
	if err == nil {
		t.Error("local BuildRepoURL should return error")
	}
	if err.Error() != "local provider uses target_path directly, not BuildRepoURL" {
		t.Errorf("local BuildRepoURL error = %q, want %q", err.Error(), "local provider uses target_path directly, not BuildRepoURL")
	}
}

func TestProvider_Local_CredentialEnv(t *testing.T) {
	p := GetProvider("local")
	if p == nil {
		t.Fatal("local provider not found")
	}
	env := p.CredentialEnv(map[string]string{})
	if env != nil {
		t.Errorf("local CredentialEnv = %v, want nil", env)
	}
}

func TestProvider_S3_BuildRepoURL(t *testing.T) {
	p := GetProvider("s3")
	if p == nil {
		t.Fatal("s3 provider not found")
	}

	tests := []struct {
		name      string
		config    map[string]string
		wantURL   string
		wantError bool
	}{
		{
			name:    "endpoint and bucket only",
			config:  map[string]string{"endpoint": "s3.amazonaws.com", "bucket": "my-bucket"},
			wantURL: "s3:s3.amazonaws.com/my-bucket",
		},
		{
			name:    "with prefix",
			config:  map[string]string{"endpoint": "https://s3.amazonaws.com", "bucket": "my-bucket", "prefix": "backups"},
			wantURL: "s3:https://s3.amazonaws.com/my-bucket/backups",
		},
		{
			name:    "prefix with leading/trailing slashes",
			config:  map[string]string{"endpoint": "s3.amazonaws.com", "bucket": "my-bucket", "prefix": "/backups/"},
			wantURL: "s3:s3.amazonaws.com/my-bucket/backups",
		},
		{
			name:      "missing endpoint",
			config:    map[string]string{"bucket": "my-bucket"},
			wantError: true,
		},
		{
			name:      "missing bucket",
			config:    map[string]string{"endpoint": "s3.amazonaws.com"},
			wantError: true,
		},
		{
			name:      "empty config",
			config:    map[string]string{},
			wantError: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			url, err := p.BuildRepoURL(tc.config)
			if tc.wantError {
				if err == nil {
					t.Errorf("BuildRepoURL(%v) = %q, want error", tc.config, url)
				}
			} else {
				if err != nil {
					t.Errorf("BuildRepoURL(%v) error = %v, want nil", tc.config, err)
				}
				if url != tc.wantURL {
					t.Errorf("BuildRepoURL(%v) = %q, want %q", tc.config, url, tc.wantURL)
				}
			}
		})
	}
}

func TestProvider_S3_CredentialEnv(t *testing.T) {
	p := GetProvider("s3")
	if p == nil {
		t.Fatal("s3 provider not found")
	}

	tests := []struct {
		name     string
		secret   map[string]string
		wantEnv  map[string]string
	}{
		{
			name:    "both keys",
			secret:  map[string]string{"access_key": "AKIA...", "secret_key": "secret..."},
			wantEnv: map[string]string{"AWS_ACCESS_KEY_ID": "AKIA...", "AWS_SECRET_ACCESS_KEY": "secret..."},
		},
		{
			name:    "only access_key",
			secret:  map[string]string{"access_key": "AKIA..."},
			wantEnv: map[string]string{"AWS_ACCESS_KEY_ID": "AKIA..."},
		},
		{
			name:    "only secret_key",
			secret:  map[string]string{"secret_key": "secret..."},
			wantEnv: map[string]string{"AWS_SECRET_ACCESS_KEY": "secret..."},
		},
		{
			name:    "empty secret",
			secret:  map[string]string{},
			wantEnv: map[string]string{},
		},
		{
			name:    "nil secret",
			secret:  nil,
			wantEnv: map[string]string{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := p.CredentialEnv(tc.secret)
			if len(env) != len(tc.wantEnv) {
				t.Errorf("CredentialEnv(%v) = %v, want %v", tc.secret, env, tc.wantEnv)
			}
			for k, v := range tc.wantEnv {
				if env[k] != v {
					t.Errorf("CredentialEnv[%q] = %q, want %q", k, env[k], v)
				}
			}
		})
	}
}

func TestProvider_B2_BuildRepoURL(t *testing.T) {
	p := GetProvider("b2")
	if p == nil {
		t.Fatal("b2 provider not found")
	}

	tests := []struct {
		name      string
		config    map[string]string
		wantURL   string
		wantError bool
	}{
		{
			name:    "bucket only",
			config:  map[string]string{"bucket": "my-bucket"},
			wantURL: "b2:my-bucket",
		},
		{
			name:    "with prefix",
			config:  map[string]string{"bucket": "my-bucket", "prefix": "backups"},
			wantURL: "b2:my-bucket:backups",
		},
		{
			name:    "prefix with leading/trailing slashes",
			config:  map[string]string{"bucket": "my-bucket", "prefix": "/backups/"},
			wantURL: "b2:my-bucket:backups",
		},
		{
			name:      "missing bucket",
			config:    map[string]string{},
			wantError: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			url, err := p.BuildRepoURL(tc.config)
			if tc.wantError {
				if err == nil {
					t.Errorf("BuildRepoURL(%v) = %q, want error", tc.config, url)
				}
			} else {
				if err != nil {
					t.Errorf("BuildRepoURL(%v) error = %v, want nil", tc.config, err)
				}
				if url != tc.wantURL {
					t.Errorf("BuildRepoURL(%v) = %q, want %q", tc.config, url, tc.wantURL)
				}
			}
		})
	}
}

func TestProvider_B2_CredentialEnv(t *testing.T) {
	p := GetProvider("b2")
	if p == nil {
		t.Fatal("b2 provider not found")
	}

	tests := []struct {
		name     string
		secret   map[string]string
		wantEnv  map[string]string
	}{
		{
			name:    "both keys",
			secret:  map[string]string{"account_id": "acc123", "app_key": "key456"},
			wantEnv: map[string]string{"B2_ACCOUNT_ID": "acc123", "B2_ACCOUNT_KEY": "key456"},
		},
		{
			name:    "only account_id",
			secret:  map[string]string{"account_id": "acc123"},
			wantEnv: map[string]string{"B2_ACCOUNT_ID": "acc123"},
		},
		{
			name:    "only app_key",
			secret:  map[string]string{"app_key": "key456"},
			wantEnv: map[string]string{"B2_ACCOUNT_KEY": "key456"},
		},
		{
			name:    "empty secret",
			secret:  map[string]string{},
			wantEnv: map[string]string{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := p.CredentialEnv(tc.secret)
			if len(env) != len(tc.wantEnv) {
				t.Errorf("CredentialEnv(%v) = %v, want %v", tc.secret, env, tc.wantEnv)
			}
			for k, v := range tc.wantEnv {
				if env[k] != v {
					t.Errorf("CredentialEnv[%q] = %q, want %q", k, env[k], v)
				}
			}
		})
	}
}

func TestProvider_SFTP_BuildRepoURL(t *testing.T) {
	p := GetProvider("sftp")
	if p == nil {
		t.Fatal("sftp provider not found")
	}

	tests := []struct {
		name      string
		config    map[string]string
		wantURL   string
		wantError bool
	}{
		{
			name:    "user, host, path",
			config:  map[string]string{"user": "user1", "host": "example.com", "path": "/backups"},
			wantURL: "sftp:user1@example.com:/backups",
		},
		{
			name:      "missing user",
			config:    map[string]string{"host": "example.com", "path": "/backups"},
			wantError: true,
		},
		{
			name:      "missing host",
			config:    map[string]string{"user": "user1", "path": "/backups"},
			wantError: true,
		},
		{
			name:      "missing path",
			config:    map[string]string{"user": "user1", "host": "example.com"},
			wantError: true,
		},
		{
			name:      "empty config",
			config:    map[string]string{},
			wantError: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			url, err := p.BuildRepoURL(tc.config)
			if tc.wantError {
				if err == nil {
					t.Errorf("BuildRepoURL(%v) = %q, want error", tc.config, url)
				}
			} else {
				if err != nil {
					t.Errorf("BuildRepoURL(%v) error = %v, want nil", tc.config, err)
				}
				if url != tc.wantURL {
					t.Errorf("BuildRepoURL(%v) = %q, want %q", tc.config, url, tc.wantURL)
				}
			}
		})
	}
}

func TestProvider_SFTP_CredentialEnv(t *testing.T) {
	p := GetProvider("sftp")
	if p == nil {
		t.Fatal("sftp provider not found")
	}
	// SFTP v1 has no secret env vars (key-based auth on host)
	env := p.CredentialEnv(map[string]string{"password": "secret"})
	if env != nil {
		t.Errorf("sftp CredentialEnv = %v, want nil", env)
	}
}

func TestProvider_GDrive_BuildRepoURL(t *testing.T) {
	p := GetProvider("gdrive")
	if p == nil {
		t.Fatal("gdrive provider not found")
	}

	tests := []struct {
		name      string
		config    map[string]string
		wantURL   string
		wantError bool
	}{
		{
			name:    "remote and path",
			config:  map[string]string{"remote": "myremote", "path": "backups"},
			wantURL: "rclone:myremote:backups",
		},
		{
			name:    "path with leading/trailing slashes",
			config:  map[string]string{"remote": "myremote", "path": "/backups/"},
			wantURL: "rclone:myremote:backups",
		},
		{
			name:      "missing remote",
			config:    map[string]string{"path": "backups"},
			wantError: true,
		},
		{
			name:      "missing path",
			config:    map[string]string{"remote": "myremote"},
			wantError: true,
		},
		{
			name:      "empty config",
			config:    map[string]string{},
			wantError: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			url, err := p.BuildRepoURL(tc.config)
			if tc.wantError {
				if err == nil {
					t.Errorf("BuildRepoURL(%v) = %q, want error", tc.config, url)
				}
			} else {
				if err != nil {
					t.Errorf("BuildRepoURL(%v) error = %v, want nil", tc.config, err)
				}
				if url != tc.wantURL {
					t.Errorf("BuildRepoURL(%v) = %q, want %q", tc.config, url, tc.wantURL)
				}
			}
		})
	}
}

func TestProvider_GDrive_CredentialEnv(t *testing.T) {
	p := GetProvider("gdrive")
	if p == nil {
		t.Fatal("gdrive provider not found")
	}
	// GDrive v1 has no secret env vars (rclone remote holds credentials)
	env := p.CredentialEnv(map[string]string{"token": "secret"})
	if env != nil {
		t.Errorf("gdrive CredentialEnv = %v, want nil", env)
	}
}

func TestValidateConfigJSON(t *testing.T) {
	tests := []struct {
		name      string
		config    string
		wantError bool
	}{
		{
			name:      "empty config",
			config:    "",
			wantError: false,
		},
		{
			name:      "valid config no secrets",
			config:    `{"endpoint": "s3.amazonaws.com", "bucket": "my-bucket", "prefix": "backups"}`,
			wantError: false,
		},
		{
			name:      "invalid JSON",
			config:    `{invalid}`,
			wantError: true,
		},
		{
			name:      "contains access_key",
			config:    `{"access_key": "AKIA...", "bucket": "my-bucket"}`,
			wantError: true,
		},
		{
			name:      "contains secret_key",
			config:    `{"secret_key": "secret...", "bucket": "my-bucket"}`,
			wantError: true,
		},
		{
			name:      "contains app_key",
			config:    `{"app_key": "key...", "bucket": "my-bucket"}`,
			wantError: true,
		},
		{
			name:      "contains password",
			config:    `{"password": "secret", "bucket": "my-bucket"}`,
			wantError: true,
		},
		{
			name:      "contains token",
			config:    `{"token": "secret", "bucket": "my-bucket"}`,
			wantError: true,
		},
		{
			name:      "contains account_id",
			config:    `{"account_id": "acc123", "bucket": "my-bucket"}`,
			wantError: true,
		},
		{
			name:      "contains account_key",
			config:    `{"account_key": "key123", "bucket": "my-bucket"}`,
			wantError: true,
		},
		{
			name:      "case-insensitive secret key",
			config:    `{"ACCESS_KEY": "AKIA...", "bucket": "my-bucket"}`,
			wantError: true,
		},
		{
			name:      "mixed case secret key",
			config:    `{"Access_Key": "AKIA...", "bucket": "my-bucket"}`,
			wantError: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateConfigJSON(tc.config)
			if tc.wantError {
				if err == nil {
					t.Errorf("ValidateConfigJSON(%q) = nil, want error", tc.config)
				}
			} else {
				if err != nil {
					t.Errorf("ValidateConfigJSON(%q) = %v, want nil", tc.config, err)
				}
			}
		})
	}
}

func TestBuildRepoURLAndEnv_Local(t *testing.T) {
	// Local provider uses target_path directly, no config/secrets needed
	repo := &catalog.BackupRepoRow{
		ID:         "repo-1",
		Name:       "local-repo",
		TargetType: "local",
		TargetPath: "/tmp/repo",
		ConfigJSON: "",
		SecretName: "",
	}
	url, credEnv, err := BuildRepoURLAndEnv(repo, nil)
	if err != nil {
		t.Fatalf("BuildRepoURLAndEnv(local) error = %v", err)
	}
	if url != "/tmp/repo" {
		t.Errorf("url = %q, want %q", url, "/tmp/repo")
	}
	if credEnv != nil {
		t.Errorf("credEnv = %v, want nil", credEnv)
	}
}

func TestBuildRepoURLAndEnv_UnknownType(t *testing.T) {
	repo := &catalog.BackupRepoRow{
		ID:         "repo-1",
		Name:       "unknown-repo",
		TargetType: "unknown",
		TargetPath: "/tmp/repo",
	}
	_, _, err := BuildRepoURLAndEnv(repo, nil)
	if err == nil {
		t.Fatal("BuildRepoURLAndEnv(unknown) should return error")
	}
	if err.Error() != "unknown target_type \"unknown\"" {
		t.Errorf("error = %q, want %q", err.Error(), "unknown target_type \"unknown\"")
	}
}

func TestBuildRepoURLAndEnv_S3_MissingConfig(t *testing.T) {
	repo := &catalog.BackupRepoRow{
		ID:         "repo-1",
		Name:       "s3-repo",
		TargetType: "s3",
		TargetPath: "",
		ConfigJSON: `{"bucket": "my-bucket"}`, // missing endpoint
		SecretName: "secret-1",
	}
	_, _, err := BuildRepoURLAndEnv(repo, nil)
	if err == nil {
		t.Fatal("BuildRepoURLAndEnv(s3 missing endpoint) should return error")
	}
	if err.Error() != "s3 requires endpoint and bucket in config" {
		t.Errorf("error = %q, want %q", err.Error(), "s3 requires endpoint and bucket in config")
	}
}

func TestBuildRepoURLAndEnv_S3_WithSecret(t *testing.T) {
	// Mock vault that returns a secret
	mockVault := &mockVault{
		secrets: map[string]string{
			"secret-1": `{"access_key": "AKIA...", "secret_key": "secret..."}`,
		},
	}
	repo := &catalog.BackupRepoRow{
		ID:         "repo-1",
		Name:       "s3-repo",
		TargetType: "s3",
		TargetPath: "",
		ConfigJSON: `{"endpoint": "s3.amazonaws.com", "bucket": "my-bucket", "prefix": "backups"}`,
		SecretName: "secret-1",
	}
	url, credEnv, err := BuildRepoURLAndEnv(repo, mockVault)
	if err != nil {
		t.Fatalf("BuildRepoURLAndEnv(s3) error = %v", err)
	}
	if url != "s3:s3.amazonaws.com/my-bucket/backups" {
		t.Errorf("url = %q, want %q", url, "s3:s3.amazonaws.com/my-bucket/backups")
	}
	if credEnv["AWS_ACCESS_KEY_ID"] != "AKIA..." {
		t.Errorf("AWS_ACCESS_KEY_ID = %q, want %q", credEnv["AWS_ACCESS_KEY_ID"], "AKIA...")
	}
	if credEnv["AWS_SECRET_ACCESS_KEY"] != "secret..." {
		t.Errorf("AWS_SECRET_ACCESS_KEY = %q, want %q", credEnv["AWS_SECRET_ACCESS_KEY"], "secret...")
	}
}

func TestBuildRepoURLAndEnv_B2_WithSecret(t *testing.T) {
	mockVault := &mockVault{
		secrets: map[string]string{
			"secret-1": `{"account_id": "acc123", "app_key": "key456"}`,
		},
	}
	repo := &catalog.BackupRepoRow{
		ID:         "repo-1",
		Name:       "b2-repo",
		TargetType: "b2",
		TargetPath: "",
		ConfigJSON: `{"bucket": "my-bucket", "prefix": "backups"}`,
		SecretName: "secret-1",
	}
	url, credEnv, err := BuildRepoURLAndEnv(repo, mockVault)
	if err != nil {
		t.Fatalf("BuildRepoURLAndEnv(b2) error = %v", err)
	}
	if url != "b2:my-bucket:backups" {
		t.Errorf("url = %q, want %q", url, "b2:my-bucket:backups")
	}
	if credEnv["B2_ACCOUNT_ID"] != "acc123" {
		t.Errorf("B2_ACCOUNT_ID = %q, want %q", credEnv["B2_ACCOUNT_ID"], "acc123")
	}
	if credEnv["B2_ACCOUNT_KEY"] != "key456" {
		t.Errorf("B2_ACCOUNT_KEY = %q, want %q", credEnv["B2_ACCOUNT_KEY"], "key456")
	}
}

func TestBuildRepoURLAndEnv_SFTP_NoSecret(t *testing.T) {
	repo := &catalog.BackupRepoRow{
		ID:         "repo-1",
		Name:       "sftp-repo",
		TargetType: "sftp",
		TargetPath: "",
		ConfigJSON: `{"user": "user1", "host": "example.com", "path": "/backups"}`,
		SecretName: "",
	}
	url, credEnv, err := BuildRepoURLAndEnv(repo, nil)
	if err != nil {
		t.Fatalf("BuildRepoURLAndEnv(sftp) error = %v", err)
	}
	if url != "sftp:user1@example.com:/backups" {
		t.Errorf("url = %q, want %q", url, "sftp:user1@example.com:/backups")
	}
	if credEnv != nil {
		t.Errorf("credEnv = %v, want nil", credEnv)
	}
}

func TestBuildRepoURLAndEnv_GDrive_NoSecret(t *testing.T) {
	repo := &catalog.BackupRepoRow{
		ID:         "repo-1",
		Name:       "gdrive-repo",
		TargetType: "gdrive",
		TargetPath: "",
		ConfigJSON: `{"remote": "myremote", "path": "backups"}`,
		SecretName: "",
	}
	url, credEnv, err := BuildRepoURLAndEnv(repo, nil)
	if err != nil {
		t.Fatalf("BuildRepoURLAndEnv(gdrive) error = %v", err)
	}
	if url != "rclone:myremote:backups" {
		t.Errorf("url = %q, want %q", url, "rclone:myremote:backups")
	}
	if credEnv != nil {
		t.Errorf("credEnv = %v, want nil", credEnv)
	}
}

func TestBuildRepoURLAndEnv_SecretFetchError(t *testing.T) {
	mockVault := &mockVault{
		secrets: map[string]string{},
		err:     &mockError{msg: "vault locked"},
	}
	repo := &catalog.BackupRepoRow{
		ID:         "repo-1",
		Name:       "s3-repo",
		TargetType: "s3",
		TargetPath: "",
		ConfigJSON: `{"endpoint": "s3.amazonaws.com", "bucket": "my-bucket"}`,
		SecretName: "secret-1",
	}
	_, _, err := BuildRepoURLAndEnv(repo, mockVault)
	if err == nil {
		t.Fatal("BuildRepoURLAndEnv(vault error) should return error")
	}
	if err.Error() != "fetch secret \"secret-1\": vault locked" {
		t.Errorf("error = %q, want %q", err.Error(), "fetch secret \"secret-1\": vault locked")
	}
}

func TestBuildRepoURLAndEnv_InvalidSecretJSON(t *testing.T) {
	mockVault := &mockVault{
		secrets: map[string]string{
			"secret-1": `invalid json`,
		},
	}
	repo := &catalog.BackupRepoRow{
		ID:         "repo-1",
		Name:       "s3-repo",
		TargetType: "s3",
		TargetPath: "",
		ConfigJSON: `{"endpoint": "s3.amazonaws.com", "bucket": "my-bucket"}`,
		SecretName: "secret-1",
	}
	_, _, err := BuildRepoURLAndEnv(repo, mockVault)
	if err == nil {
		t.Fatal("BuildRepoURLAndEnv(invalid secret JSON) should return error")
	}
	if err.Error() != "parse secret JSON: invalid character 'i' looking for beginning of value" {
		t.Errorf("error = %q", err.Error())
	}
}

// mockVault implements the vault.Vault interface for testing
type mockVault struct {
	secrets map[string]string
	err     error
}

func (m *mockVault) Get(name string) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	val, ok := m.secrets[name]
	if !ok {
		return "", &mockError{msg: "secret not found"}
	}
	return val, nil
}

func (m *mockVault) Set(name, value string) error {
	m.secrets[name] = value
	return nil
}

func (m *mockVault) Delete(name string) error {
	delete(m.secrets, name)
	return nil
}

func (m *mockVault) List() ([]string, error) {
	keys := make([]string, 0, len(m.secrets))
	for k := range m.secrets {
		keys = append(keys, k)
	}
	return keys, nil
}

func (m *mockVault) Unlock(password string) error {
	return nil
}

func (m *mockVault) Lock() error {
	return nil
}

func (m *mockVault) IsLocked() bool {
	return false
}

func (m *mockVault) ChangePassword(oldPassword, newPassword string) error {
	return nil
}

type mockError struct {
	msg string
}

func (e *mockError) Error() string {
	return e.msg
}

// Test that ProviderDef struct has expected fields for all providers
func TestProviderDef_RequiredFields(t *testing.T) {
	for name, p := range Registry {
		t.Run(name, func(t *testing.T) {
			if p.Type != name {
				t.Errorf("ProviderDef.Type = %q, want %q", p.Type, name)
			}
			if p.BuildRepoURL == nil {
				t.Errorf("ProviderDef.BuildRepoURL is nil for %s", name)
			}
			if p.CredentialEnv == nil {
				t.Errorf("ProviderDef.CredentialEnv is nil for %s", name)
			}
			// RequiredConfig and RequiredSecret can be empty slices but not nil
			if p.RequiredConfig == nil {
				t.Errorf("ProviderDef.RequiredConfig is nil for %s", name)
			}
			if p.RequiredSecret == nil {
				t.Errorf("ProviderDef.RequiredSecret is nil for %s", name)
			}
		})
	}
}

// Test that secretKeysToReject covers all expected keys
func TestSecretKeysToReject(t *testing.T) {
	expectedKeys := []string{
		"access_key", "secret_key", "app_key",
		"password", "token",
		"account_id", "account_key",
	}
	for _, key := range expectedKeys {
		if !secretKeysToReject[key] {
			t.Errorf("secretKeysToReject missing key %q", key)
		}
	}
	// Verify case-insensitive rejection works through ValidateConfigJSON
	// (the map itself holds lowercase keys; the function lowercases first)
	if err := ValidateConfigJSON(`{"ACCESS_KEY": "x"}`); err == nil {
		t.Error("ValidateConfigJSON should reject ACCESS_KEY case-insensitively")
	}
}

// Test that config JSON marshaling round-trips correctly
func TestConfigJSON_RoundTrip(t *testing.T) {
	configs := []map[string]string{
		{"endpoint": "s3.amazonaws.com", "bucket": "my-bucket", "prefix": "backups"},
		{"bucket": "my-bucket", "prefix": "backups"},
		{"user": "user1", "host": "example.com", "path": "/backups"},
		{"remote": "myremote", "path": "backups"},
		{},
	}
	for _, cfg := range configs {
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatalf("json.Marshal(%v) = %v", cfg, err)
		}
		var out map[string]string
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatalf("json.Unmarshal(%s) = %v", string(data), err)
		}
		if len(out) != len(cfg) {
			t.Errorf("round-trip length mismatch: in=%d out=%d", len(cfg), len(out))
		}
		for k, v := range cfg {
			if out[k] != v {
				t.Errorf("round-trip key %q: in=%q out=%q", k, v, out[k])
			}
		}
	}
}