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
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/bank-vaults/vault-sdk/vault"
	vaultapi "github.com/hashicorp/vault/api"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type VaultConfig struct {
	Addr                 string
	AuthMethod           string
	Role                 string
	Path                 string
	Namespace            string
	SkipVerify           bool
	TLSSecret            string
	TLSSecretNS          string
	ClientTimeout        time.Duration
	IgnoreMissingSecrets bool
}

func getVaultConfigFromEnv() *VaultConfig {
	var vaultConfig VaultConfig

	vaultConfig.Addr = os.Getenv("VAULT_ADDR")
	if vaultConfig.Addr == "" {
		vaultConfig.Addr = "https://vault:8200"
	}

	vaultConfig.AuthMethod = os.Getenv("VAULT_AUTH_METHOD")
	if vaultConfig.AuthMethod == "" {
		vaultConfig.AuthMethod = "jwt"
	}

	vaultConfig.Role = os.Getenv("VAULT_ROLE")

	vaultConfig.Path = os.Getenv("VAULT_PATH")
	if vaultConfig.Path == "" {
		vaultConfig.Path = "kubernetes"
	}

	vaultConfig.Namespace = os.Getenv("VAULT_NAMESPACE")
	if vaultConfig.Namespace == "" {
		vaultConfig.Namespace = "default"
	}

	vaultConfig.SkipVerify, _ = strconv.ParseBool(os.Getenv("VAULT_SKIP_VERIFY"))

	vaultConfig.TLSSecret = os.Getenv("VAULT_TLS_SECRET")

	vaultConfig.TLSSecretNS = os.Getenv("VAULT_TLS_SECRET_NS")
	if vaultConfig.TLSSecretNS == "" {
		vaultConfig.TLSSecretNS = "default"
	}

	vaultConfig.ClientTimeout, _ = time.ParseDuration(os.Getenv("VAULT_CLIENT_TIMEOUT"))
	if vaultConfig.ClientTimeout == 0 {
		vaultConfig.ClientTimeout = 10 * time.Second
	}

	vaultConfig.IgnoreMissingSecrets, _ = strconv.ParseBool(os.Getenv("VAULT_IGNORE_MISSING_SECRETS"))

	return &vaultConfig
}

func (c *Controller) initVaultClient() error {
	if c.vaultClient != nil {
		_, err := c.vaultClient.Sys().Health()
		if err == nil {
			// Client is valid, no need to init
			return nil
		}
		// log error and continue with (re)creating client
		c.logger.Error("connection to Vault lost, recreating client")
	}

	c.logger.Info("Initializing Vault client")

	c.vaultConfig = getVaultConfigFromEnv()
	clientConfig := vaultapi.DefaultConfig()
	if clientConfig.Error != nil {
		return clientConfig.Error
	}

	clientConfig.Address = c.vaultConfig.Addr
	clientConfig.Timeout = c.vaultConfig.ClientTimeout

	tlsConfig := vaultapi.TLSConfig{Insecure: c.vaultConfig.SkipVerify}
	err := clientConfig.ConfigureTLS(&tlsConfig)
	if err != nil {
		return err
	}

	if c.vaultConfig.TLSSecret != "" {
		tlsSecret, err := c.kubeClient.CoreV1().Secrets(c.vaultConfig.TLSSecretNS).Get(
			context.Background(),
			c.vaultConfig.TLSSecret,
			metav1.GetOptions{},
		)
		if err != nil {
			return fmt.Errorf("failed to read Vault TLS Secret: %s", err.Error())
		}

		clientTLSConfig := clientConfig.HttpClient.Transport.(*http.Transport).TLSClientConfig

		pool := x509.NewCertPool()

		ok := pool.AppendCertsFromPEM(tlsSecret.Data["ca.crt"])
		if !ok {
			return fmt.Errorf("error loading Vault CA PEM from TLS Secret: %s", tlsSecret.Name)
		}

		clientTLSConfig.RootCAs = pool
	}

	vaultClient, err := vault.NewClientFromConfig(
		clientConfig,
		vault.ClientRole(c.vaultConfig.Role),
		vault.ClientAuthPath(c.vaultConfig.Path),
		vault.ClientAuthMethod(c.vaultConfig.AuthMethod),
		vault.ClientLogger(&clientLogger{logger: c.logger}),
		vault.VaultNamespace(c.vaultConfig.Namespace),
	)
	if err != nil {
		return err
	}
	//
	// Check connection to Vault
	_, err = vaultClient.RawClient().Sys().Health()
	if err != nil {
		c.logger.Error("testing connection to Vault failed")
		return err
	}

	c.vaultClient = vaultClient.RawClient()
	c.logger.Info("Vault client initialized")
	return nil
}

type ErrSecretNotFound struct {
	secretPath string
}

func (e ErrSecretNotFound) Error() string {
	return fmt.Sprintf("Vault secret path %s not found", e.secretPath)
}

type vaultSecretReader interface {
	Read(path string) (*vaultapi.Secret, error)
}

func getSecretVersionFromVault(vaultClient vaultSecretReader, secretPath string) (int, error) {
	secret, err := vaultClient.Read(secretPath)
	if err != nil {
		return 0, err
	}
	if secret != nil {
		secretVersion, err := secret.Data["metadata"].(map[string]interface{})["version"].(json.Number).Int64()
		if err != nil {
			return 0, err
		}
		return int(secretVersion), nil
	}

	return 0, ErrSecretNotFound{secretPath: secretPath}
}
