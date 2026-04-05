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
	"io"
	"log/slog"
	"testing"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestIncrementReloadCountAnnotation(t *testing.T) {
	tests := []struct {
		name                string
		annotations         map[string]string
		expectedAnnotations map[string]string
	}{
		{
			name:        "no annotation should add annotation",
			annotations: map[string]string{},
			expectedAnnotations: map[string]string{
				ReloadCountAnnotationName: "1",
			},
		},
		{
			name: "existing annotation should increment annotation",
			annotations: map[string]string{
				ReloadCountAnnotationName: "1",
			},
			expectedAnnotations: map[string]string{
				ReloadCountAnnotationName: "2",
			},
		},
	}

	for _, tt := range tests {
		ttp := tt
		t.Run(ttp.name, func(t *testing.T) {
			podTemplateSpec := &corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: ttp.annotations,
				},
			}

			incrementReloadCountAnnotation(podTemplateSpec)

			assert.Equal(t, ttp.expectedAnnotations, podTemplateSpec.Annotations)
		})
	}
}

// Test helpers for runReloader tests
func newTestController() *Controller {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	kubeClient := fake.NewClientset()

	// Create a mock vault logical backend
	mockLogical := &mockVaultLogical{
		readCalls: []string{},
		secrets:   make(map[string]*vaultapi.Secret),
		err:       nil,
	}

	controller := &Controller{
		kubeClient:          kubeClient,
		logger:              logger,
		workloadSecrets:     newWorkloadSecrets(),
		dynamicSecretLeases: make(map[string]*DynamicSecretLease),
		workloadTracking:    make(map[workload]*workloadTrackingInfo),
		vaultConfig: &VaultConfig{
			DynamicSecretRestartThreshold: 0.7, // 70% threshold
		},
		now:               time.Now,
		initVaultClientFn: func() error { return nil },
		getSecretInfoFn:   getSecretInfoFromVault,
		reloadWorkloadFn:  func(context.Context, workload) error { return nil },
		getVaultLogicalFn: func() vaultSecretReader { return mockLogical },
	}

	return controller
}

func TestRunReloader_KVVersionChangeTriggersRestart(t *testing.T) {
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	controller := newTestController()
	controller.now = func() time.Time { return now }

	currentWorkload := workload{name: "my-deployment", namespace: "default", kind: DeploymentKind}

	// Setup: workload has a KV secret with version 1
	controller.workloadSecrets.Store(currentWorkload, []secretMetadata{
		{
			Path:      "secret/my-secret",
			IsKV:      true,
			KVVersion: 1,
		},
	})

	// Setup: also need tracking info for the deployment to be checked for restart
	controller.workloadTracking[currentWorkload] = &workloadTrackingInfo{
		LastRestartTime:    now.Add(-10 * time.Second),
		ShortestDynamicTTL: 0, // no dynamic secrets
	}

	// Track which deployments were reloaded
	reloadedWorkloads := []workload{}
	originalReloadFn := controller.reloadWorkloadFn
	controller.reloadWorkloadFn = func(ctx context.Context, w workload) error {
		reloadedWorkloads = append(reloadedWorkloads, workload{
			name:      w.name,
			namespace: w.namespace,
			kind:      w.kind,
		})
		return originalReloadFn(ctx, w)
	}

	// Mock: secret info returns version 2 (changed)
	controller.getSecretInfoFn = func(reader vaultSecretReader, path string) (*SecretInfo, error) {
		t.Logf("getSecretInfoFn called for path: %s", path)
		if path == "secret/my-secret" {
			return &SecretInfo{
				IsKV:    true,
				Version: 2,
			}, nil
		}
		// Return a valid empty SecretInfo for other paths to avoid nil pointer panics
		return &SecretInfo{}, nil
	}

	// Run reloader
	t.Logf("Running reloader with %d workloads", len(controller.workloadSecrets.GetWorkloadSecretsMap()))
	controller.runReloader(context.Background())

	// Verify: workload was reloaded
	t.Logf("Reloaded workloads: %d", len(reloadedWorkloads))
	if assert.Len(t, reloadedWorkloads, 1) {
		assert.Equal(t, currentWorkload, reloadedWorkloads[0])
	}

	// Verify: KVVersion was updated
	controller.trackingMutex.RLock()
	updatedMetadata := controller.workloadSecrets.GetWorkloadSecretsMap()[currentWorkload]
	controller.trackingMutex.RUnlock()
	assert.Equal(t, 2, updatedMetadata[0].KVVersion)
}

func TestRunReloader_DynamicSecretTTLThresholdTriggersRestart(t *testing.T) {
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	controller := newTestController()
	controller.now = func() time.Time { return now }

	currentWorkload := workload{name: "my-deployment", namespace: "default", kind: DeploymentKind}

	// Setup: workload has a dynamic secret with 1000 seconds TTL
	controller.workloadSecrets.Store(currentWorkload, []secretMetadata{
		{
			Path:            "db/creds/my-role",
			IsDynamic:       true,
			DynamicLeaseTTL: 1000,
		},
	})

	// Setup: last restart was 700 seconds ago (at 70% of 1000 = 700)
	lastRestartTime := now.Add(-700 * time.Second)
	controller.workloadTracking[currentWorkload] = &workloadTrackingInfo{
		LastRestartTime:    lastRestartTime,
		ShortestDynamicTTL: 1000,
	}

	// Track reloads
	reloadedWorkloads := []workload{}
	controller.reloadWorkloadFn = func(ctx context.Context, w workload) error {
		reloadedWorkloads = append(reloadedWorkloads, workload{
			name:      w.name,
			namespace: w.namespace,
			kind:      w.kind,
		})
		return nil
	}

	// Mock: secret info returns dynamic secret
	controller.getSecretInfoFn = func(reader vaultSecretReader, path string) (*SecretInfo, error) {
		if path == "db/creds/my-role" {
			return &SecretInfo{
				IsDynamic: true,
				LeaseInfo: &DynamicSecretLease{
					LeaseID:       "test-lease",
					LeaseDuration: 1000,
					LeaseExpiry:   now.Add(1000 * time.Second),
					SecretPath:    path,
					Renewable:     true,
				},
			}, nil
		}
		return &SecretInfo{}, nil
	}

	// Run reloader
	controller.runReloader(context.Background())

	// Verify: workload should be reloaded at 70% threshold
	assert.Len(t, reloadedWorkloads, 1)
	assert.Equal(t, currentWorkload, reloadedWorkloads[0])

	// Verify: LastRestartTime was updated to now
	controller.trackingMutex.RLock()
	trackingInfo := controller.workloadTracking[currentWorkload]
	controller.trackingMutex.RUnlock()
	assert.Equal(t, now, trackingInfo.LastRestartTime)
}

func TestRunReloader_NoRestartBeforeThreshold(t *testing.T) {
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	controller := newTestController()
	controller.now = func() time.Time { return now }

	currentWorkload := workload{name: "my-deployment", namespace: "default", kind: DeploymentKind}

	// Setup: workload has a dynamic secret with 1000 seconds TTL
	controller.workloadSecrets.Store(currentWorkload, []secretMetadata{
		{
			Path:            "db/creds/my-role",
			IsDynamic:       true,
			DynamicLeaseTTL: 1000,
		},
	})

	// Setup: last restart was 500 seconds ago (below 70% threshold of 700)
	lastRestartTime := now.Add(-500 * time.Second)
	controller.workloadTracking[currentWorkload] = &workloadTrackingInfo{
		LastRestartTime:    lastRestartTime,
		ShortestDynamicTTL: 1000,
	}

	// Track reloads
	reloadedWorkloads := []workload{}
	controller.reloadWorkloadFn = func(ctx context.Context, w workload) error {
		reloadedWorkloads = append(reloadedWorkloads, workload{
			name:      w.name,
			namespace: w.namespace,
			kind:      w.kind,
		})
		return nil
	}

	// Run reloader
	controller.runReloader(context.Background())

	// Verify: workload should NOT be reloaded
	assert.Len(t, reloadedWorkloads, 0)
}

func TestRunReloader_MultipleWorkloadsMixedDecisions(t *testing.T) {
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	controller := newTestController()
	controller.now = func() time.Time { return now }

	depA := workload{name: "deployment-a", namespace: "default", kind: DeploymentKind}
	depB := workload{name: "deployment-b", namespace: "default", kind: DeploymentKind}
	depC := workload{name: "deployment-c", namespace: "default", kind: DeploymentKind}

	// Setup: depA has KV secret with version 1 (will change to 2)
	controller.workloadSecrets.Store(depA, []secretMetadata{
		{
			Path:      "secret/app-a",
			IsKV:      true,
			KVVersion: 1,
		},
	})
	// Need tracking info for depA to be checked for restarts
	controller.workloadTracking[depA] = &workloadTrackingInfo{
		LastRestartTime:    now.Add(-10 * time.Second),
		ShortestDynamicTTL: 0,
	}

	// Setup: depB has dynamic secret at 70% threshold
	controller.workloadSecrets.Store(depB, []secretMetadata{
		{
			Path:            "db/creds/role-b",
			IsDynamic:       true,
			DynamicLeaseTTL: 1000,
		},
	})
	controller.workloadTracking[depB] = &workloadTrackingInfo{
		LastRestartTime:    now.Add(-700 * time.Second),
		ShortestDynamicTTL: 1000,
	}

	// Setup: depC has nothing special (should not restart)
	controller.workloadSecrets.Store(depC, []secretMetadata{
		{
			Path:      "secret/app-c",
			IsKV:      true,
			KVVersion: 1,
		},
	})
	// No tracking info for depC - it won't be checked

	// Track reloads
	reloadedWorkloads := []workload{}
	controller.reloadWorkloadFn = func(ctx context.Context, w workload) error {
		reloadedWorkloads = append(reloadedWorkloads, workload{
			name:      w.name,
			namespace: w.namespace,
			kind:      w.kind,
		})
		return nil
	}

	// Mock: depA gets version 2, others stay same
	controller.getSecretInfoFn = func(reader vaultSecretReader, path string) (*SecretInfo, error) {
		if path == "secret/app-a" {
			return &SecretInfo{
				IsKV:    true,
				Version: 2,
			}, nil
		}
		if path == "secret/app-c" {
			return &SecretInfo{
				IsKV:    true,
				Version: 1,
			}, nil
		}
		// Return a valid empty SecretInfo for other paths
		return &SecretInfo{}, nil
	}

	// Run reloader
	controller.runReloader(context.Background())

	// Verify: only depA and depB were reloaded
	assert.Len(t, reloadedWorkloads, 2)

	reloadedNames := map[string]bool{}
	for _, dep := range reloadedWorkloads {
		reloadedNames[dep.name] = true
	}
	assert.True(t, reloadedNames["deployment-a"])
	assert.True(t, reloadedNames["deployment-b"])
	assert.False(t, reloadedNames["deployment-c"])
}

func TestRunReloader_NoSecretsNoRestart(t *testing.T) {
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	controller := newTestController()
	controller.now = func() time.Time { return now }

	// Setup: controller has no workloads to monitor
	assert.Empty(t, controller.workloadSecrets.GetWorkloadSecretsMap())
	assert.Empty(t, controller.workloadTracking)

	// Track reloads
	reloadedWorkloads := []workload{}
	controller.reloadWorkloadFn = func(ctx context.Context, w workload) error {
		reloadedWorkloads = append(reloadedWorkloads, workload{
			name:      w.name,
			namespace: w.namespace,
			kind:      w.kind,
		})
		return nil
	}

	// Run reloader
	controller.runReloader(context.Background())

	// Verify: no workloads were reloaded
	assert.Len(t, reloadedWorkloads, 0)
}

func TestRunReloader_KVChangeTakesPrecedenceOverTTL(t *testing.T) {
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	controller := newTestController()
	controller.now = func() time.Time { return now }

	currentWorkload := workload{name: "my-deployment", namespace: "default", kind: DeploymentKind}

	// Setup: workload has both KV secret (v1) and dynamic secret
	controller.workloadSecrets.Store(currentWorkload, []secretMetadata{
		{
			Path:      "secret/my-secret",
			IsKV:      true,
			KVVersion: 1,
		},
		{
			Path:            "db/creds/role",
			IsDynamic:       true,
			DynamicLeaseTTL: 1000,
		},
	})

	// Setup: last restart was 500 seconds ago (below threshold)
	controller.workloadTracking[currentWorkload] = &workloadTrackingInfo{
		LastRestartTime:    now.Add(-500 * time.Second),
		ShortestDynamicTTL: 1000,
	}

	// Track reloads and check we only reload once
	reloadCount := 0
	originalReloadFn := controller.reloadWorkloadFn
	controller.reloadWorkloadFn = func(ctx context.Context, w workload) error {
		reloadCount++
		return originalReloadFn(ctx, w)
	}

	// Mock: KV version changed to 2 (triggers restart even though TTL is below threshold)
	controller.getSecretInfoFn = func(reader vaultSecretReader, path string) (*SecretInfo, error) {
		if path == "secret/my-secret" {
			return &SecretInfo{
				IsKV:    true,
				Version: 2, // Changed!
			}, nil
		}
		return nil, nil
	}

	// Run reloader
	controller.runReloader(context.Background())

	// Verify: workload was reloaded (KV change takes precedence)
	assert.Equal(t, 1, reloadCount, "workload should be reloaded due to KV version change")
}
