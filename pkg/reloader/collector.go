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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bank-vaults/secrets-webhook/pkg/common"
	corev1 "k8s.io/api/core/v1"
)

var (
	vaultSecretRegex = regexp.MustCompile(`vault:([^#]+)#`)
)

type workloadSecretsStore interface {
	Store(workload workload, secrets []secretMetadata)
	Delete(workload workload)
	GetWorkloadSecretsMap() map[workload][]secretMetadata
	GetSecretWorkloadsMap() map[secretMetadata][]workload
}

type workload struct {
	name      string
	namespace string
	kind      string
}

type secretMetadata struct {
	Path            string
	IsKV            bool
	KVVersion       int
	IsDynamic       bool
	DynamicLeaseTTL int // in seconds
}

type workloadTrackingInfo struct {
	LastRestartTime    time.Time
	ShortestDynamicTTL int // in seconds, 0 if no dynamic secrets
}

type workloadSecrets struct {
	sync.RWMutex
	workloadSecretsMap map[workload][]secretMetadata
}

func newWorkloadSecrets() workloadSecretsStore {
	return &workloadSecrets{
		workloadSecretsMap: make(map[workload][]secretMetadata),
	}
}

func (w *workloadSecrets) Store(workload workload, secrets []secretMetadata) {
	w.Lock()
	defer w.Unlock()
	w.workloadSecretsMap[workload] = secrets
}

func (w *workloadSecrets) Delete(workload workload) {
	w.Lock()
	defer w.Unlock()
	delete(w.workloadSecretsMap, workload)
}

func (w *workloadSecrets) GetWorkloadSecretsMap() map[workload][]secretMetadata {
	return w.workloadSecretsMap
}

func (w *workloadSecrets) GetSecretWorkloadsMap() map[secretMetadata][]workload {
	w.Lock()
	defer w.Unlock()
	secretWorkloads := make(map[secretMetadata][]workload)
	for workload, secretsMetadata := range w.workloadSecretsMap {
		for _, secretMetadata := range secretsMetadata {
			secretWorkloads[secretMetadata] = append(secretWorkloads[secretMetadata], workload)
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

		// Clear stale deployment state to prevent restarts based on old metadata
		c.trackingMutex.Lock()
		delete(c.workloadTracking, workload)
		c.trackingMutex.Unlock()

		return
	}
	collectorLogger.Debug(fmt.Sprintf("Vault secret paths found: %v", vaultSecretPaths))

	// Query Vault for metadata about each secret
	err := c.initVaultClientFn()
	if err != nil {
		collectorLogger.Error(fmt.Sprintf("Failed to initialize Vault client: %v", err))
		return
	}

	// Build a lookup of already-tracked dynamic secrets to avoid re-reading them
	c.trackingMutex.RLock()
	existingMetadata, hasExisting := c.workloadSecrets.GetWorkloadSecretsMap()[workload]
	c.trackingMutex.RUnlock()

	dynamicMetadataByPath := make(map[string]secretMetadata)
	if hasExisting {
		for _, metadata := range existingMetadata {
			if metadata.IsDynamic {
				dynamicMetadataByPath[metadata.Path] = metadata
			}
		}
	}

	secretsMetadata := make([]secretMetadata, 0, len(vaultSecretPaths))
	for _, secretPath := range vaultSecretPaths {
		if metadata, exists := dynamicMetadataByPath[secretPath]; exists {
			secretsMetadata = append(secretsMetadata, metadata)
			collectorLogger.Debug(fmt.Sprintf("Secret %s is dynamic, using tracked TTL: %d seconds", secretPath, metadata.DynamicLeaseTTL))
			continue
		}

		secretInfo, err := c.getSecretInfoFn(c.getVaultLogicalFn(), secretPath)
		if err != nil {
			collectorLogger.Error(fmt.Sprintf("Failed to get secret info for %s: %v", secretPath, err))
			continue
		}

		metadata := secretMetadata{
			Path: secretPath,
		}

		if secretInfo.IsKV {
			metadata.IsKV = true
			metadata.KVVersion = secretInfo.Version
			collectorLogger.Debug(fmt.Sprintf("Secret %s is KV v2, version: %d", secretPath, secretInfo.Version))
		} else if secretInfo.IsDynamic {
			metadata.IsDynamic = true
			metadata.DynamicLeaseTTL = secretInfo.LeaseInfo.LeaseDuration
			collectorLogger.Debug(fmt.Sprintf("Secret %s is dynamic, TTL: %d seconds", secretPath, secretInfo.LeaseInfo.LeaseDuration))
		}

		secretsMetadata = append(secretsMetadata, metadata)
	}

	// Add workload and secrets to workloadSecrets map
	c.workloadSecrets.Store(workload, secretsMetadata)
	collectorLogger.Info(fmt.Sprintf("Collected secrets from %s %s/%s", workload.kind, workload.namespace, workload.name))
}

func (c *Controller) trackWorkloadRestartTime(workload workload, pods []corev1.Pod) {
	c.trackingMutex.Lock()
	defer c.trackingMutex.Unlock()

	// Get deployment secrets to calculate shortest dynamic TTL
	workloadSecretsMeta, exists := c.workloadSecrets.GetWorkloadSecretsMap()[workload]
	if !exists {
		return
	}

	shortestTTL := 0
	for _, secret := range workloadSecretsMeta {
		if secret.IsDynamic {
			if shortestTTL == 0 || secret.DynamicLeaseTTL < shortestTTL {
				shortestTTL = secret.DynamicLeaseTTL
			}
		}
	}

	// Find the oldest running pod to determine deployment's effective start time
	var oldestStartTime time.Time
	for _, pod := range pods {
		if pod.Status.StartTime == nil {
			continue // Pod hasn't started yet
		}

		if pod.Status.Phase != corev1.PodRunning {
			continue // Only consider running pods
		}

		if pod.ObjectMeta.DeletionTimestamp != nil {
			continue // Skip pods that are terminating
		}

		if oldestStartTime.IsZero() || pod.Status.StartTime.Time.Before(oldestStartTime) {
			oldestStartTime = pod.Status.StartTime.Time
		}
	}

	// Only track if we found at least one running pod
	if !oldestStartTime.IsZero() {
		if existing, exists := c.workloadTracking[workload]; exists {
			// Always update to reflect current pod state (oldest time may be earlier now,
			// and TTL may have changed)
			existing.LastRestartTime = oldestStartTime
			existing.ShortestDynamicTTL = shortestTTL
		} else {
			c.workloadTracking[workload] = &workloadTrackingInfo{
				LastRestartTime:    oldestStartTime,
				ShortestDynamicTTL: shortestTTL,
			}
		}
	}
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
			// Skip if env var does not contain a vault secret
			if !isValidVaultSubstring(env.Value) {
				continue
			}

			segments := extractVaultSecretSegments(env.Value)
			for _, segment := range segments {
				if segment.IsVersioned {
					continue
				}
				vaultSecretPaths = append(vaultSecretPaths, segment.Path)
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
			segments := extractVaultSecretSegments(secretPath)
			for _, segment := range segments {
				if segment.IsVersioned {
					continue
				}
				vaultSecretPaths = append(vaultSecretPaths, segment.Path)
			}
		}
	}

	// This is here to preserve backwards compatibility with the deprecated annotation
	if len(vaultSecretPaths) == 0 {
		deprecatedSecretPaths := annotations[common.VaultEnvFromPathAnnotationDeprecated]
		if deprecatedSecretPaths != "" {
			for _, secretPath := range strings.Split(deprecatedSecretPaths, ",") {
				segments := extractVaultSecretSegments(secretPath)
				for _, segment := range segments {
					if segment.IsVersioned {
						continue
					}
					vaultSecretPaths = append(vaultSecretPaths, segment.Path)
				}
			}
		}
	}

	return vaultSecretPaths
}

// implementation based on bank-vaults/secrets-webhook/pkg/provider/vault/provider.go
func isValidVaultSubstring(value string) bool {
	return vaultSecretRegex.MatchString(value)
}

type vaultSecretSegment struct {
	Path        string
	IsVersioned bool
}

func extractVaultSecretSegments(value string) []vaultSecretSegment {
	segments := []vaultSecretSegment{}
	searchIndex := 0
	for {
		start := strings.Index(value[searchIndex:], "vault:")
		if start == -1 {
			break
		}
		start += searchIndex + len("vault:")
		segmentEnd := len(value)
		if next := strings.Index(value[start:], "vault:"); next != -1 {
			segmentEnd = start + next
		}
		segment := value[start:segmentEnd]
		firstHash := strings.Index(segment, "#")
		if firstHash == -1 {
			searchIndex = start
			continue
		}
		path := segment[:firstHash]
		if path == "" {
			searchIndex = start
			continue
		}
		remainder := segment[firstHash+1:]
		isVersioned := false
		if remainder != "" {
			parts := strings.Split(remainder, "#")
			if len(parts) >= 2 {
				last := parts[len(parts)-1]
				if last != "" && isAllDigits(last) {
					isVersioned = true
				}
			}
		}
		segments = append(segments, vaultSecretSegment{Path: path, IsVersioned: isVersioned})
		searchIndex = start
	}

	return segments
}

func isAllDigits(value string) bool {
	if value == "" {
		return false
	}
	_, err := strconv.ParseUint(value, 10, 64)
	return err == nil
}

func unversionedAnnotationSecretValue(value string) bool {
	split := strings.SplitN(value, "#", 2)
	return len(split) == 1
}
