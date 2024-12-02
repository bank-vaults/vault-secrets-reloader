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
		reloaderLogger.Info("No workloads to reload")
		return
	}

	err := c.initVaultClient()
	if err != nil {
		reloaderLogger.Error(fmt.Errorf("failed to initialize Vault client: %w", err).Error())
		return
	}

	// Create a secretWorkloads map and compare the currently used secrets' version
	// with the one stored in the secretVersions map, while creating a new secretVersions map
	workloadsToReload := make(map[workload]bool)
	newSecretVersions := make(map[string]int)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for secretPath, workloads := range c.workloadSecrets.GetSecretWorkloadsMap() {
		wg.Add(1)
		go func(secretPath string, workloads []workload) {
			defer wg.Done()
			reloaderLogger.Debug(fmt.Sprintf("Checking secret: %s", secretPath))

			// Get current secret version
			currentVersion, err := getSecretVersionFromVault(c.vaultClient.Logical(), secretPath)
			if err != nil {
				c.handleSecretError(err, secretPath, reloaderLogger)
				return
			}

			mu.Lock()
			defer mu.Unlock()

			// Compare secret versions
			switch c.secretVersions[secretPath] {
			case 0:
				reloaderLogger.Debug(fmt.Sprintf("Secret %s not found in secretVersions map, creating it", secretPath))
			case currentVersion:
				reloaderLogger.Debug(fmt.Sprintf("Secret %s did not change", secretPath))
			default:
				reloaderLogger.Debug(fmt.Sprintf("Secret version stored: %d current: %d", c.secretVersions[secretPath], currentVersion))
				for _, workload := range workloads {
					workloadsToReload[workload] = true
				}
			}

			newSecretVersions[secretPath] = currentVersion
		}(secretPath, workloads)
	}
	// wait for secret version checking to complete
	wg.Wait()

	// Reloading workloads
	wg = sync.WaitGroup{} // Reset the WaitGroup
	for workloadToReload := range workloadsToReload {
		wg.Add(1)
		go func(workloadToReload workload) {
			defer wg.Done()
			reloaderLogger.Info(fmt.Sprintf("Reloading workload: %s", workloadToReload))

			err := c.reloadWorkload(ctx, workloadToReload)
			if err != nil {
				reloaderLogger.Error(fmt.Errorf("failed reloading workload: %s: %w", workloadToReload, err).Error())
			}
		}(workloadToReload)
	}
	// wait for workload reloading to complete
	wg.Wait()

	// Replace secretVersions map with the new one so we don't keep deleted secrets in the map
	c.secretVersions = newSecretVersions
	reloaderLogger.Debug(fmt.Sprintf("Updated secretVersions map: %#v", newSecretVersions))

	if len(workloadsToReload) == 0 {
		reloaderLogger.Info("No workloads to reload")
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
