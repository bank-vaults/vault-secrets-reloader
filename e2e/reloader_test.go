// Copyright © 2023 Bank-Vaults Maintainers
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

//go:build e2e
// +build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	vaultV1alpha1 "github.com/bank-vaults/vault-operator/pkg/apis/vault/v1alpha1"
	vaultClientV1alpha1 "github.com/bank-vaults/vault-operator/pkg/client/clientset/versioned/typed/vault/v1alpha1"
	"github.com/bank-vaults/vault-secrets-reloader/pkg/reloader"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	extv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/e2e-framework/klient/decoder"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestWorkloadReload(t *testing.T) {
	workloads := applyWorkloads().
		WithStep("update secrets in Vault", features.Level(1), func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			// create a patch to Vault to update startup secrets
			vaultPatch := &vaultV1alpha1.Vault{
				ObjectMeta: metav1.ObjectMeta{Name: "vault", Namespace: cfg.Namespace()},
				Spec: vaultV1alpha1.VaultSpec{
					Config:         extv1beta1.JSON{Raw: []byte("{\"ui\": true}")},
					ExternalConfig: extv1beta1.JSON{Raw: []byte(getVaultPatch())},
				},
				Status: vaultV1alpha1.VaultStatus{
					Nodes: []string{},
				},
			}

			vaultV1alpha1Client, err := vaultClientV1alpha1.NewForConfig(cfg.Client().RESTConfig())
			require.NoError(t, err)

			vaultPatchJSON, err := json.Marshal(vaultPatch)
			require.NoError(t, err)

			_, err = vaultV1alpha1Client.Vaults(cfg.Namespace()).Patch(ctx, "vault", types.MergePatchType, vaultPatchJSON, metav1.PatchOptions{})
			require.NoError(t, err)

			return ctx
		}).
		Assess("daemonset reloaded", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			daemonSet := &appsv1.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: "reloader-test-daemonset", Namespace: cfg.Namespace()},
			}
			err := wait.For(conditions.New(cfg.Client().Resources()).ResourceMatch(daemonSet, func(obj k8s.Object) bool {
				return obj.(*appsv1.DaemonSet).Spec.Template.Annotations[reloader.ReloadCountAnnotationName] == "1"
			}), wait.WithTimeout(3*time.Minute))
			require.NoError(t, err)

			return ctx
		}).
		Assess("statefulset reloaded", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			statefulSet := &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Name: "reloader-test-statefulset", Namespace: cfg.Namespace()},
			}
			err := wait.For(conditions.New(cfg.Client().Resources()).ResourceMatch(statefulSet, func(obj k8s.Object) bool {
				return obj.(*appsv1.StatefulSet).Spec.Template.Annotations[reloader.ReloadCountAnnotationName] == "1"
			}), wait.WithTimeout(3*time.Minute))
			require.NoError(t, err)

			return ctx
		}).
		Assess("deployment to be reloaded is reloaded", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "reloader-test-deployment-to-be-reloaded", Namespace: cfg.Namespace()},
			}
			err := wait.For(conditions.New(cfg.Client().Resources()).ResourceMatch(deployment, func(obj k8s.Object) bool {
				return obj.(*appsv1.Deployment).Spec.Template.Annotations[reloader.ReloadCountAnnotationName] == "1"
			}), wait.WithTimeout(3*time.Minute))
			require.NoError(t, err)

			return ctx
		}).
		Assess("deployment without reload annotation not reloaded", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "reloader-test-deployment-no-reload", Namespace: cfg.Namespace()},
			}
			err := wait.For(conditions.New(cfg.Client().Resources()).ResourceMatch(deployment, func(obj k8s.Object) bool {
				return obj.(*appsv1.Deployment).Spec.Template.Annotations[reloader.ReloadCountAnnotationName] == ""
			}), wait.WithTimeout(3*time.Minute))
			require.NoError(t, err)

			return ctx
		}).
		Assess("deployment with fixed version secrets not reloaded", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "reloader-test-deployment-fixed-versions-no-reload", Namespace: cfg.Namespace()},
			}
			err := wait.For(conditions.New(cfg.Client().Resources()).ResourceMatch(deployment, func(obj k8s.Object) bool {
				return obj.(*appsv1.Deployment).Spec.Template.Annotations[reloader.ReloadCountAnnotationName] == ""
			}), wait.WithTimeout(3*time.Minute))
			require.NoError(t, err)

			return ctx
		}).
		Teardown(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			err := decoder.DecodeEachFile(
				ctx, os.DirFS("deploy/workloads"), "*",
				decoder.DeleteHandler(cfg.Client().Resources()),
				decoder.MutateNamespace(cfg.Namespace()),
			)
			require.NoError(t, err)

			return ctx
		}).
		Feature()

	testenv.Test(t, workloads)
}

func applyWorkloads() *features.FeatureBuilder {
	return features.New("workloads").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			err := decoder.DecodeEachFile(
				ctx, os.DirFS("deploy/workloads"), "*",
				decoder.CreateHandler(cfg.Client().Resources()),
				decoder.MutateNamespace(cfg.Namespace()),
			)
			require.NoError(t, err)

			// workloadsAvailable fails on Github Runners, so waiting a little for the workloads to come up is fine for now
			// err = workloadsAvailable(cfg)
			// require.NoError(t, err)
			time.Sleep(3 * time.Minute)

			return ctx
		})
}

func workloadsAvailable(cfg *envconf.Config) error {
	var errors []error

	// wait for the daemonset to become available
	daemonSet := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "reloader-test-daemonset", Namespace: cfg.Namespace()},
	}
	err := wait.For(conditions.New(cfg.Client().Resources()).ResourceMatch(daemonSet, func(obj k8s.Object) bool {
		return obj.(*appsv1.DaemonSet).Status.NumberReady == obj.(*appsv1.DaemonSet).Status.DesiredNumberScheduled
	}), wait.WithTimeout(10*time.Minute))
	if err != nil {
		errors = append(errors, fmt.Errorf("reloader-test-daemonset: %w", err))
	}

	// wait for the statefulset to become available
	statefulSet := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "reloader-test-statefulset", Namespace: cfg.Namespace()},
	}
	err = wait.For(conditions.New(cfg.Client().Resources()).ResourceMatch(statefulSet, func(obj k8s.Object) bool {
		return obj.(*appsv1.StatefulSet).Status.ReadyReplicas == *obj.(*appsv1.StatefulSet).Spec.Replicas
	}), wait.WithTimeout(10*time.Minute))
	if err != nil {
		errors = append(errors, fmt.Errorf("reloader-test-statefulset: %w", err))
	}

	// wait for the deployments to become available
	deploymentToBeReloaded := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "reloader-test-deployment-to-be-reloaded", Namespace: cfg.Namespace()},
	}
	err = wait.For(conditions.New(cfg.Client().Resources()).DeploymentConditionMatch(deploymentToBeReloaded, appsv1.DeploymentAvailable, v1.ConditionTrue), wait.WithTimeout(10*time.Minute))
	if err != nil {
		errors = append(errors, fmt.Errorf("reloader-test-deployment-to-be-reloaded: %w", err))
	}

	deploymentNoReload := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "reloader-test-deployment-no-reload", Namespace: cfg.Namespace()},
	}
	err = wait.For(conditions.New(cfg.Client().Resources()).DeploymentConditionMatch(deploymentNoReload, appsv1.DeploymentAvailable, v1.ConditionTrue), wait.WithTimeout(10*time.Minute))
	if err != nil {
		errors = append(errors, fmt.Errorf("reloader-test-deployment-no-reload: %w", err))
	}

	deploymentFixedVersionsNoReload := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "reloader-test-deployment-fixed-versions-no-reload", Namespace: cfg.Namespace()},
	}
	err = wait.For(conditions.New(cfg.Client().Resources()).DeploymentConditionMatch(deploymentFixedVersionsNoReload, appsv1.DeploymentAvailable, v1.ConditionTrue), wait.WithTimeout(10*time.Minute))
	if err != nil {
		errors = append(errors, fmt.Errorf("reloader-test-deployment-fixed-versions-no-reload: %w", err))
	}

	if len(errors) == 0 {
		return nil
	}

	var errorStrings []string
	for _, err := range errors {
		errorStrings = append(errorStrings, err.Error())
	}
	return fmt.Errorf(strings.Join(errorStrings, ", "))
}

func getVaultPatch() string {
	return `{
  "startupSecrets": [
    {
      "type": "kv",
      "path": "secret/data/accounts/aws",
      "data": {
        "data": {
          "AWS_ACCESS_KEY_ID": "secretId2",
          "AWS_SECRET_ACCESS_KEY": "s3cr3t2"
        }
      }
    },
    {
      "type": "kv",
      "path": "secret/data/dockerrepo",
      "data": {
        "data": {
          "DOCKER_REPO_USER": "dockerrepouser2",
          "DOCKER_REPO_PASSWORD": "dockerrepopassword2"
        }
      }
    },
    {
      "type": "kv",
      "path": "secret/data/mysql",
      "data": {
        "data": {
          "MYSQL_ROOT_PASSWORD": "s3cr3t2",
          "MYSQL_PASSWORD": "3xtr3ms3cr3t2"
        }
      }
    }
  ]
}`
}
