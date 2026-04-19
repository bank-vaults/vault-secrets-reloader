// Copyright © 2023 Cisco
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package reloader

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
	"github.com/stretchr/testify/assert"
)

type mockVaultClient struct {
	err         error
	vaultSecret *vaultapi.Secret
}

func (c *mockVaultClient) Read(path string) (*vaultapi.Secret, error) {
	_ = path
	return c.vaultSecret, c.err
}

func TestGetVaultConfigFromEnv(t *testing.T) {
	t.Run("default config", func(t *testing.T) {
		if err := os.Unsetenv("VAULT_ADDR"); err != nil {
			t.Fatalf("failed to unset VAULT_ADDR: %v", err)
		}
		defaults := VaultConfig{
			Addr:                          "https://vault:8200",
			AuthMethod:                    "jwt",
			Role:                          "",
			Path:                          "kubernetes",
			Namespace:                     "default",
			SkipVerify:                    false,
			TLSSecret:                     "",
			TLSSecretNS:                   "default",
			ClientTimeout:                 10 * time.Second,
			IgnoreMissingSecrets:          false,
			DynamicSecretRestartThreshold: 0.7,
		}

		vaultConfig := getVaultConfigFromEnv()
		assert.Equal(t, defaults, *vaultConfig)
	})

	t.Run("custom config", func(t *testing.T) {
		if err := os.Setenv("VAULT_ADDR", "http://127.0.0.1:8200"); err != nil {
			t.Fatalf("failed to set VAULT_ADDR: %v", err)
		}
		if err := os.Setenv("VAULT_AUTH_METHOD", "kubernetes"); err != nil {
			t.Fatalf("failed to set VAULT_AUTH_METHOD: %v", err)
		}
		if err := os.Setenv("VAULT_ROLE", "test"); err != nil {
			t.Fatalf("failed to set VAULT_ROLE: %v", err)
		}
		if err := os.Setenv("VAULT_PATH", "test"); err != nil {
			t.Fatalf("failed to set VAULT_PATH: %v", err)
		}
		if err := os.Setenv("VAULT_NAMESPACE", "test"); err != nil {
			t.Fatalf("failed to set VAULT_NAMESPACE: %v", err)
		}
		if err := os.Setenv("VAULT_SKIP_VERIFY", "true"); err != nil {
			t.Fatalf("failed to set VAULT_SKIP_VERIFY: %v", err)
		}
		if err := os.Setenv("VAULT_TLS_SECRET", "test"); err != nil {
			t.Fatalf("failed to set VAULT_TLS_SECRET: %v", err)
		}
		if err := os.Setenv("VAULT_TLS_SECRET_NS", "test"); err != nil {
			t.Fatalf("failed to set VAULT_TLS_SECRET_NS: %v", err)
		}
		if err := os.Setenv("VAULT_CLIENT_TIMEOUT", "1m"); err != nil {
			t.Fatalf("failed to set VAULT_CLIENT_TIMEOUT: %v", err)
		}
		if err := os.Setenv("VAULT_IGNORE_MISSING_SECRETS", "true"); err != nil {
			t.Fatalf("failed to set VAULT_IGNORE_MISSING_SECRETS: %v", err)
		}
		if err := os.Setenv("VAULT_DYNAMIC_SECRET_RESTART_THRESHOLD", "0.5"); err != nil {
			t.Fatalf("failed to set VAULT_DYNAMIC_SECRET_RESTART_THRESHOLD: %v", err)
		}
		defaults := VaultConfig{
			Addr:                          "http://127.0.0.1:8200",
			AuthMethod:                    "kubernetes",
			Role:                          "test",
			Path:                          "test",
			Namespace:                     "test",
			SkipVerify:                    true,
			TLSSecret:                     "test",
			TLSSecretNS:                   "test",
			ClientTimeout:                 1 * time.Minute,
			IgnoreMissingSecrets:          true,
			DynamicSecretRestartThreshold: 0.5,
		}

		vaultConfig := getVaultConfigFromEnv()
		assert.Equal(t, defaults, *vaultConfig)
	})
}

func TestGetSecretVersionFromVault(t *testing.T) {
	t.Run("secret not found", func(t *testing.T) {
		vaultClient := &mockVaultClient{
			err: ErrSecretNotFound{},
		}

		_, err := getSecretInfoFromVault(vaultClient, "test")
		assert.Equal(t, ErrSecretNotFound{}, err)
	})

	t.Run("other error", func(t *testing.T) {
		vaultClient := &mockVaultClient{
			err: assert.AnError,
		}

		_, err := getSecretInfoFromVault(vaultClient, "test")
		assert.Equal(t, assert.AnError, err)
	})

	t.Run("success", func(t *testing.T) {
		vaultClient := &mockVaultClient{
			vaultSecret: &vaultapi.Secret{
				Data: map[string]interface{}{
					"metadata": map[string]interface{}{
						"version": json.Number("3"),
					},
				},
			},
		}

		secretInfo, err := getSecretInfoFromVault(vaultClient, "test")
		assert.NoError(t, err)
		assert.NotNil(t, secretInfo)
		assert.True(t, secretInfo.IsKV)
		assert.False(t, secretInfo.IsDynamic)
		assert.Equal(t, 3, secretInfo.Version)
		assert.Nil(t, secretInfo.LeaseInfo)
	})

	t.Run("dynamic secret", func(t *testing.T) {
		vaultClient := &mockVaultClient{
			vaultSecret: &vaultapi.Secret{
				LeaseID:       "database/creds/my-role/abc123",
				LeaseDuration: 3600,
				Renewable:     true,
				Data: map[string]interface{}{
					"username": "v-token-my-role-abc123",
					"password": "A1a-secretpassword",
				},
			},
		}

		secretInfo, err := getSecretInfoFromVault(vaultClient, "test")
		assert.NoError(t, err)
		assert.NotNil(t, secretInfo)
		assert.False(t, secretInfo.IsKV)
		assert.True(t, secretInfo.IsDynamic)
		assert.NotNil(t, secretInfo.LeaseInfo)
		assert.Equal(t, "database/creds/my-role/abc123", secretInfo.LeaseInfo.LeaseID)
		assert.Equal(t, 3600, secretInfo.LeaseInfo.LeaseDuration)
		assert.True(t, secretInfo.LeaseInfo.Renewable)
		assert.Equal(t, "test", secretInfo.LeaseInfo.SecretPath)
	})
}

func TestRenewDynamicSecretLease(t *testing.T) {
	t.Run("successful renewal", func(t *testing.T) {
		originalLease := &DynamicSecretLease{
			LeaseID:       "database/creds/my-role/old-lease",
			LeaseDuration: 3600,
			LeaseExpiry:   time.Now(),
			SecretPath:    "database/creds/my-role",
			Renewable:     true,
		}

		vaultClient := &mockVaultClient{
			vaultSecret: &vaultapi.Secret{
				LeaseID:       "database/creds/my-role/new-lease",
				LeaseDuration: 3600,
				Renewable:     true,
				Data: map[string]interface{}{
					"username": "v-token-my-role-new123",
					"password": "A1a-newsecretpassword",
				},
			},
		}

		renewedLease, err := renewDynamicSecretLease(vaultClient, originalLease)
		assert.NoError(t, err)
		assert.NotNil(t, renewedLease)
		assert.Equal(t, "database/creds/my-role/new-lease", renewedLease.LeaseID)
		assert.Equal(t, 3600, renewedLease.LeaseDuration)
		assert.True(t, renewedLease.Renewable)
		assert.Equal(t, "database/creds/my-role", renewedLease.SecretPath)
		assert.True(t, renewedLease.LeaseExpiry.After(time.Now()))
	})

	t.Run("read secret fails", func(t *testing.T) {
		originalLease := &DynamicSecretLease{
			LeaseID:       "database/creds/my-role/abc123",
			LeaseDuration: 3600,
			LeaseExpiry:   time.Now(),
			SecretPath:    "database/creds/my-role",
			Renewable:     true,
		}

		vaultClient := &mockVaultClient{
			err: assert.AnError,
		}

		renewedLease, err := renewDynamicSecretLease(vaultClient, originalLease)
		assert.Error(t, err)
		assert.Nil(t, renewedLease)
		assert.Contains(t, err.Error(), "failed to read dynamic secret")
	})

	t.Run("secret not found", func(t *testing.T) {
		originalLease := &DynamicSecretLease{
			LeaseID:       "database/creds/my-role/abc123",
			LeaseDuration: 3600,
			LeaseExpiry:   time.Now(),
			SecretPath:    "database/creds/my-role",
			Renewable:     true,
		}

		vaultClient := &mockVaultClient{
			vaultSecret: nil,
		}

		renewedLease, err := renewDynamicSecretLease(vaultClient, originalLease)
		assert.Error(t, err)
		assert.Nil(t, renewedLease)
		assert.Contains(t, err.Error(), "dynamic secret at path")
		assert.Contains(t, err.Error(), "not found")
	})

	t.Run("secret is no longer dynamic", func(t *testing.T) {
		originalLease := &DynamicSecretLease{
			LeaseID:       "database/creds/my-role/abc123",
			LeaseDuration: 3600,
			LeaseExpiry:   time.Now(),
			SecretPath:    "database/creds/my-role",
			Renewable:     true,
		}

		vaultClient := &mockVaultClient{
			vaultSecret: &vaultapi.Secret{
				LeaseID:       "",
				LeaseDuration: 0,
				Renewable:     false,
				Data: map[string]interface{}{
					"username": "static-user",
				},
			},
		}

		renewedLease, err := renewDynamicSecretLease(vaultClient, originalLease)
		assert.Error(t, err)
		assert.Nil(t, renewedLease)
		assert.Contains(t, err.Error(), "no longer a dynamic secret")
	})

	t.Run("non-renewable secret", func(t *testing.T) {
		originalLease := &DynamicSecretLease{
			LeaseID:       "database/creds/my-role/abc123",
			LeaseDuration: 3600,
			LeaseExpiry:   time.Now(),
			SecretPath:    "database/creds/my-role",
			Renewable:     true,
		}

		vaultClient := &mockVaultClient{
			vaultSecret: &vaultapi.Secret{
				LeaseID:       "database/creds/my-role/new-lease",
				LeaseDuration: 3600,
				Renewable:     false,
				Data: map[string]interface{}{
					"username": "v-token-my-role-new123",
					"password": "A1a-newsecretpassword",
				},
			},
		}

		renewedLease, err := renewDynamicSecretLease(vaultClient, originalLease)
		assert.NoError(t, err)
		assert.NotNil(t, renewedLease)
		assert.False(t, renewedLease.Renewable)
	})
}
