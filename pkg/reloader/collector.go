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
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/bank-vaults/secrets-webhook/pkg/common"
	corev1 "k8s.io/api/core/v1"
)

type workloadSecretsStore interface {
	Store(workload workload, secrets []string)
	Delete(workload workload)
	GetWorkloadSecretsMap() map[workload][]string
	GetSecretWorkloadsMap() map[string][]workload
}

type workload struct {
	name      string
	namespace string
	kind      string
}

type workloadSecrets struct {
	sync.RWMutex
	workloadSecretsMap map[workload][]string
}

func newWorkloadSecrets() workloadSecretsStore {
	return &workloadSecrets{
		workloadSecretsMap: make(map[workload][]string),
	}
}

func (w *workloadSecrets) Store(workload workload, secrets []string) {
	w.Lock()
	defer w.Unlock()
	w.workloadSecretsMap[workload] = secrets
}

func (w *workloadSecrets) Delete(workload workload) {
	w.Lock()
	defer w.Unlock()
	delete(w.workloadSecretsMap, workload)
}

func (w *workloadSecrets) GetWorkloadSecretsMap() map[workload][]string {
	return w.workloadSecretsMap
}

func (w *workloadSecrets) GetSecretWorkloadsMap() map[string][]workload {
	w.Lock()
	defer w.Unlock()
	secretWorkloads := make(map[string][]workload)
	for workload, secretPaths := range w.workloadSecretsMap {
		for _, secretPath := range secretPaths {
			secretWorkloads[secretPath] = append(secretWorkloads[secretPath], workload)
		}
	}
	return secretWorkloads
}

func (c *Controller) collectWorkloadSecrets(workload workload, template corev1.PodTemplateSpec) {
	collectorLogger := c.logger.With(slog.String("worker", "collector"))

	// Collect secrets from different locations
	vaultSecretPaths := collectSecrets(template)

	if len(vaultSecretPaths) == 0 {
		collectorLogger.Debug("No Vault secret paths found in container env vars")
		return
	}
	collectorLogger.Debug(fmt.Sprintf("Vault secret paths found: %v", vaultSecretPaths))

	// Add workload and secrets to workloadSecrets map
	c.workloadSecrets.Store(workload, vaultSecretPaths)
	collectorLogger.Info(fmt.Sprintf("Collected secrets from %s %s/%s", workload.kind, workload.namespace, workload.name))
}

func collectSecrets(template corev1.PodTemplateSpec) []string {
	containers := []corev1.Container{}
	containers = append(containers, template.Spec.Containers...)
	containers = append(containers, template.Spec.InitContainers...)

	vaultSecretPaths := []string{}
	vaultSecretPaths = append(vaultSecretPaths, collectSecretsFromContainerEnvVars(containers)...)
	vaultSecretPaths = append(vaultSecretPaths, collectSecretsFromAnnotations(template.GetAnnotations())...)

	// Remove duplicates
	slices.Sort(vaultSecretPaths)
	return slices.Compact(vaultSecretPaths)
}

func collectSecretsFromContainerEnvVars(containers []corev1.Container) []string {
	vaultSecretPaths := []string{}
	// iterate through all environment variables and extract secrets
	for _, container := range containers {
		for _, env := range container.Env {
			// Skip if env var does not contain a vault secret or is a secret with pinned version
			if common.HasVaultPrefix(env.Value) && unversionedSecretValue(env.Value) {
				secret := regexp.MustCompile(`vault:(.*?)#`).FindStringSubmatch(env.Value)[1]
				if secret != "" {
					vaultSecretPaths = append(vaultSecretPaths, secret)
				}
			}
		}
	}

	return vaultSecretPaths
}

func collectSecretsFromAnnotations(annotations map[string]string) []string {
	vaultSecretPaths := []string{}

	secretPaths := annotations[common.VaultFromPathAnnotation]
	if secretPaths != "" {
		for _, secretPath := range strings.Split(secretPaths, ",") {
			if unversionedAnnotationSecretValue(secretPath) {
				vaultSecretPaths = append(vaultSecretPaths, secretPath)
			}
		}
	}

	// This is here to preserve backwards compatibility with the deprecated annotation
	if len(vaultSecretPaths) == 0 {
		deprecatedSecretPaths := annotations[common.VaultEnvFromPathAnnotationDeprecated]
		if deprecatedSecretPaths != "" {
			for _, secretPath := range strings.Split(deprecatedSecretPaths, ",") {
				if unversionedAnnotationSecretValue(secretPath) {
					vaultSecretPaths = append(vaultSecretPaths, secretPath)
				}
			}
		}
	}

	return vaultSecretPaths
}

// implementation based on bank-vaults/internal/pkg/injector/vault/injector.go
func unversionedSecretValue(value string) bool {
	split := strings.SplitN(value, "#", 3)
	return len(split) == 2
}

func unversionedAnnotationSecretValue(value string) bool {
	split := strings.SplitN(value, "#", 2)
	return len(split) == 1
}
