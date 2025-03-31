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

func TestGetVaultConfigFromEnv(t *testing.T) {
	t.Run("default config", func(t *testing.T) {
		if err := os.Unsetenv("VAULT_ADDR"); err != nil {
			t.Fatalf("failed to unset VAULT_ADDR: %v", err)
		}
		defaults := VaultConfig{
			Addr:                 "https://vault:8200",
			AuthMethod:           "jwt",
			Role:                 "",
			Path:                 "kubernetes",
			Namespace:            "default",
			SkipVerify:           false,
			TLSSecret:            "",
			TLSSecretNS:          "default",
			ClientTimeout:        10 * time.Second,
			IgnoreMissingSecrets: false,
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
		defaults := VaultConfig{
			Addr:                 "http://127.0.0.1:8200",
			AuthMethod:           "kubernetes",
			Role:                 "test",
			Path:                 "test",
			Namespace:            "test",
			SkipVerify:           true,
			TLSSecret:            "test",
			TLSSecretNS:          "test",
			ClientTimeout:        1 * time.Minute,
			IgnoreMissingSecrets: true,
		}

		vaultConfig := getVaultConfigFromEnv()
		assert.Equal(t, defaults, *vaultConfig)
	})
}

type vaultClientMock struct {
	err         error
	vaultSecret *vaultapi.Secret
}

func (c *vaultClientMock) Read(path string) (*vaultapi.Secret, error) {
	_ = path
	return c.vaultSecret, c.err
}

func TestGetSecretVersionFromVault(t *testing.T) {
	t.Run("secret not found", func(t *testing.T) {
		vaultClient := &vaultClientMock{
			err: ErrSecretNotFound{},
		}

		_, err := getSecretVersionFromVault(vaultClient, "test")
		assert.Equal(t, ErrSecretNotFound{}, err)
	})

	t.Run("other error", func(t *testing.T) {
		vaultClient := &vaultClientMock{
			err: assert.AnError,
		}

		_, err := getSecretVersionFromVault(vaultClient, "test")
		assert.Equal(t, assert.AnError, err)
	})

	t.Run("success", func(t *testing.T) {
		vaultClient := &vaultClientMock{
			vaultSecret: &vaultapi.Secret{
				Data: map[string]interface{}{
					"metadata": map[string]interface{}{
						"version": json.Number("3"),
					},
				},
			},
		}

		version, err := getSecretVersionFromVault(vaultClient, "test")
		assert.NoError(t, err)
		assert.Equal(t, 3, version)
	})
}
