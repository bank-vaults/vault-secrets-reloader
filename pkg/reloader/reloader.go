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
	"fmt"
	"log/slog"
	"strconv"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (c *Controller) runReloader(ctx context.Context) {
	reloaderLogger := c.logger.With(slog.String("worker", "reloader"))
	reloaderLogger.Info("Reloader started")

	if len(c.workloadSecrets.GetWorkloadSecretsMap()) == 0 {
		reloaderLogger.Info("No deployments to monitor")
		return
	}

	err := c.initVaultClientFn()
	if err != nil {
		reloaderLogger.Error(fmt.Errorf("failed to initialize Vault client: %w", err).Error())
		return
	}

	// Check for KV version changes per deployment
	workloadsWithKVChanges := make(map[workload]bool)

	var wg sync.WaitGroup
	var mu sync.Mutex
	for currentSecretMetadata, workloads := range c.workloadSecrets.GetSecretWorkloadsMap() {
		wg.Add(1)
		go func(currentSecretMetadata secretMetadata, workloads []workload) {
			defer wg.Done()
			reloaderLogger.Debug(fmt.Sprintf("Checking secret: %s", currentSecretMetadata.Path))

			if !currentSecretMetadata.IsKV {
				return
			}

			// Get current secret version
			currentInfo, err := c.getSecretInfoFn(c.getVaultLogicalFn(), currentSecretMetadata.Path)
			if err != nil {
				c.handleSecretError(err, currentSecretMetadata.Path, reloaderLogger)
				return
			}

			mu.Lock()
			defer mu.Unlock()

			if currentInfo.IsKV && currentInfo.Version != currentSecretMetadata.KVVersion {
				for _, workload := range workloads {
					reloaderLogger.Info(fmt.Sprintf("KV secret %s version changed from %d to %d for workload %s/%s",
						currentSecretMetadata.Path, currentSecretMetadata.KVVersion, currentInfo.Version, workload.namespace, workload.name))
					workloadsWithKVChanges[workload] = true

					// Update stored version
					c.trackingMutex.Lock()
					for i := range c.workloadSecrets.GetWorkloadSecretsMap()[workload] {
						if c.workloadSecrets.GetWorkloadSecretsMap()[workload][i].Path == currentSecretMetadata.Path {
							c.workloadSecrets.GetWorkloadSecretsMap()[workload][i].KVVersion = currentInfo.Version
						}
					}
					c.trackingMutex.Unlock()
				}
			}
		}(currentSecretMetadata, workloads)
	}
	// wait for secret version checking to complete
	wg.Wait()

	// Check workloads for time-based and KV-based restarts
	workloadsToReload := make(map[workload]string) // value: reason for restart
	now := c.now()

	c.trackingMutex.RLock()
	trackingCopy := make(map[workload]*workloadTrackingInfo)
	for k, v := range c.workloadTracking {
		trackingCopy[k] = v
	}
	c.trackingMutex.RUnlock()

	for workload, trackingInfo := range trackingCopy {
		// Check if workload has KV changes
		if workloadsWithKVChanges[workload] {
			reloaderLogger.Info(fmt.Sprintf("Workload %s/%s needs restart due to KV secret version change", workload.namespace, workload.name))
			workloadsToReload[workload] = "KV secret version changed"
			continue
		}

		// Check time-based restart for dynamic secrets
		if trackingInfo.ShortestDynamicTTL > 0 {
			elapsedSeconds := int(now.Sub(trackingInfo.LastRestartTime).Seconds())
			restartThreshold := int(float64(trackingInfo.ShortestDynamicTTL) * c.vaultConfig.DynamicSecretRestartThreshold)

			if elapsedSeconds >= restartThreshold {
				reloaderLogger.Info(fmt.Sprintf("Workload %s/%s needs restart: elapsed=%ds, threshold=%ds (%.0f%%%% of %ds TTL)",
					workload.namespace, workload.name, elapsedSeconds, restartThreshold, c.vaultConfig.DynamicSecretRestartThreshold*100, trackingInfo.ShortestDynamicTTL))
				workloadsToReload[workload] = fmt.Sprintf("Dynamic secret TTL threshold reached (%ds/%ds)", elapsedSeconds, restartThreshold)
			}
		}
	}

	// Reloading workloads
	wg = sync.WaitGroup{} // Reset the WaitGroup
	for workloadToReload, reason := range workloadsToReload {
		wg.Add(1)
		go func(workloadToReload workload, reason string) {
			defer wg.Done()
			reloaderLogger.Info(fmt.Sprintf("Triggering rolling restart for %s %s/%s: %s", workloadToReload.kind, workloadToReload.namespace, workloadToReload.name, reason))

			err := c.reloadWorkloadFn(ctx, workloadToReload)
			if err != nil {
				reloaderLogger.Error(fmt.Sprintf("Failed to restart %s %s/%s: %v", workloadToReload.kind, workloadToReload.namespace, workloadToReload.name, err))
				return
			}

			reloaderLogger.Info(fmt.Sprintf("Successfully triggered rolling restart for %s %s/%s", workloadToReload.kind, workloadToReload.namespace, workloadToReload.name))

			// Update last restart time
			c.trackingMutex.Lock()
			if tracking, exists := c.workloadTracking[workloadToReload]; exists {
				tracking.LastRestartTime = now
			}
			c.trackingMutex.Unlock()
		}(workloadToReload, reason)
	}
	// wait for workload reloading to complete
	wg.Wait()

	if len(workloadsToReload) == 0 {
		reloaderLogger.Info(fmt.Sprintf("No workloads need restart (monitoring %d workloads)", len(c.workloadTracking)))
	} else {
		reloaderLogger.Info(fmt.Sprintf("Triggered rolling restart for %d workloads", len(workloadsToReload)))
	}
}

func (c *Controller) reloadWorkload(ctx context.Context, workload workload) error {
	// Reload object based on its type
	switch workload.kind {
	case DeploymentKind:
		deployment, err := c.kubeClient.AppsV1().Deployments(workload.namespace).Get(ctx, workload.name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		incrementReloadCountAnnotation(&deployment.Spec.Template)

		_, err = c.kubeClient.AppsV1().Deployments(workload.namespace).Update(ctx, deployment, metav1.UpdateOptions{})
		if err != nil {
			return err
		}

	case DaemonSetKind:
		daemonSet, err := c.kubeClient.AppsV1().DaemonSets(workload.namespace).Get(ctx, workload.name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		incrementReloadCountAnnotation(&daemonSet.Spec.Template)

		_, err = c.kubeClient.AppsV1().DaemonSets(workload.namespace).Update(ctx, daemonSet, metav1.UpdateOptions{})
		if err != nil {
			return err
		}

	case StatefulSetKind:
		statefulSet, err := c.kubeClient.AppsV1().StatefulSets(workload.namespace).Get(ctx, workload.name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		incrementReloadCountAnnotation(&statefulSet.Spec.Template)

		_, err = c.kubeClient.AppsV1().StatefulSets(workload.namespace).Update(ctx, statefulSet, metav1.UpdateOptions{})
		if err != nil {
			return err
		}

	default:
		return fmt.Errorf("unknown object type: %s", workload.kind)
	}

	return nil
}

func (c *Controller) handleSecretError(err error, secretPath string, logger *slog.Logger) {
	switch err.(type) {
	case ErrSecretNotFound:
		if !c.vaultConfig.IgnoreMissingSecrets {
			logger.Error(err.Error())
		} else {
			logger.Warn(fmt.Sprintf(
				"Path not found: %s - We couldn't find a secret path. This is not an error since missing secrets can be ignored according to the configuration you've set (env: VAULT_IGNORE_MISSING_SECRETS).",
				secretPath,
			))
		}

	default:
		logger.Error(fmt.Errorf("failed to get secret version: %w", err).Error())
	}
}

func incrementReloadCountAnnotation(podTemplate *corev1.PodTemplateSpec) {
	version := "1"

	if reloadCount := podTemplate.GetAnnotations()[ReloadCountAnnotationName]; reloadCount != "" {
		count, err := strconv.Atoi(reloadCount)
		if err == nil {
			count++
			version = strconv.Itoa(count)
		}
	}

	podTemplate.GetAnnotations()[ReloadCountAnnotationName] = version
}
