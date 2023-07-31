// Copyright © 2023 Banzai Cloud
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

package controllers

import (
	"context"
	"fmt"
	"os"

	"github.com/bank-vaults/vault-sdk/vault"
	"github.com/bank-vaults/vault-secrets-reloader/internal/collector"
	config "github.com/bank-vaults/vault-secrets-reloader/internal/vault_client_config"

	"github.com/go-logr/logr"
	"github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	kubernetesConfig "sigs.k8s.io/controller-runtime/pkg/client/config"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

var (
	hashSecretName = "vault-secrets-reloader-hashes"
	podNamespace   = os.Getenv("POD_NAMESPACE")
)

// DeploymentReconciler reconciles a Deployment object
type DeploymentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=apps,resources=deployments/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=apps,resources=deployments/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;patch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Deployment object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.1/pkg/reconcile
func (r *DeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.Log.WithValues("deployment", req.NamespacedName)

	// Step 0: Fetch the Deployment from the Kubernetes API.
	var deployment appsv1.Deployment
	if err := r.Get(ctx, req.NamespacedName, &deployment); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch Deployment")
		return ctrl.Result{}, err
	}

	if deployment.Spec.Template.GetAnnotations()["alpha.vault.security.banzaicloud.io/reload-on-secret-change"] != "true" {
		return ctrl.Result{}, nil
	}
	log.Info("Reconciling")

	k8sClient, err := newK8SClient()
	if err != nil {
		log.Error(err, "unable to create k8s client")
		return ctrl.Result{}, err
	}

	deploymentEnvVars, objectEnvVars, err := collector.CollectDeploymentEnvVars(k8sClient, &deployment)
	if err != nil {
		log.Error(err, "unable to collect deployment env vars")
		return ctrl.Result{}, err
	}

	// Create a Vault client and get the current version of the secrets
	vaultConfig := config.ParseVaultConfig(deployment.Spec.Template.GetAnnotations(), podNamespace, deployment.Spec.Template.Spec.ServiceAccountName)

	var logger *logrus.Entry
	{
		l := logrus.New()
		l.SetLevel(logrus.DebugLevel)
		logger = l.WithField("controller", "vault-secrets-reloader")
	}
	vaultClient, err := config.NewVaultClientFromVaultConfig(logger, k8sClient, podNamespace, vaultConfig)
	if err != nil {
		log.Error(err, "unable to create vault client")
		return ctrl.Result{}, err
	}
	defer vaultClient.Close()

	// Reload secrets, configmaps first
	for object, envVars := range objectEnvVars {
		if object.GetAnnotations()["alpha.vault.security.banzaicloud.io/reload-on-secret-change"] == "true" {
			err = reloadObject(log, k8sClient, vaultClient, object, envVars)
			if err != nil {
				log.Error(err, "reloading secrets of object failed")
				return ctrl.Result{}, err
			}
		}
	}

	// Reload deployment
	err = reloadObject(log, k8sClient, vaultClient, &deployment, deploymentEnvVars)
	if err != nil {
		log.Error(err, "reloading secrets of deployment failed")
		return ctrl.Result{}, err
	}

	log.Info("Reloading secrets of deployment done")
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *DeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// Uncomment the following line adding a pointer to an instance of the controlled resource as an argument
		For(&appsv1.Deployment{}).
		Complete(r)
}

func newK8SClient() (kubernetes.Interface, error) {
	kubeConfig, err := kubernetesConfig.GetConfig()
	if err != nil {
		return nil, err
	}

	return kubernetes.NewForConfig(kubeConfig)
}

func reloadObject(
	log logr.Logger,
	k8sClient kubernetes.Interface,
	vaultClient *vault.Client,
	object metav1.Object,
	envVars []corev1.EnvVar,
) error {
	// Create a map to store used Vault secrets and their versions
	vaultSecrets := make(map[string]int)

	objectKind := getObjectKind(object)

	// 1. Collect environment variables that need to be injected from Vault
	collector.CollectSecretsFromEnvVars(envVars, vaultSecrets)

	if deployment, ok := object.(*appsv1.Deployment); ok {
		// 2. Collect secrets from vault.security.banzaicloud.io/vault-env-from-path annnotation
		collector.CollectSecretsFromAnnotation(deployment, vaultSecrets)

		// 3. Collect secrets from Consul templates
		err := collector.CollectSecretsFromTemplate(k8sClient, deployment, vaultSecrets)
		if err != nil {
			return err
		}

		if len(vaultSecrets) == 0 {
			log.Info(fmt.Sprintf("No secrets found for deployment %s.%s", deployment.GetNamespace(), deployment.GetName()))
			return nil
		}
	}

	// Get the current version of the secrets
	for secretPath := range vaultSecrets {
		currentVersion, err := collector.GetSecretVersionFromVault(vaultClient, secretPath)
		if err != nil {
			log.Error(err, "getting secret version from vault failed")
			return err
		}
		vaultSecrets[secretPath] = currentVersion
	}

	// Hashing the secrets
	hashStr, err := collector.CreateCollectedVaultSecretsHash(vaultSecrets)
	if err != nil {
		return err
	}

	// Get secret that stores the hashes
	hashSecret, err := k8sClient.CoreV1().Secrets(podNamespace).Get(context.Background(), hashSecretName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	// Get the current hash from the secret
	objectHashSecretName := fmt.Sprintf("%s_%s_%s", object.GetNamespace(), objectKind, object.GetName())
	secretPatchJSON := fmt.Sprintf("{\"stringData\":{\"%s\":\"%s\"}}", objectHashSecretName, hashStr)

	if hashSecret.Data[objectHashSecretName] == nil {
		log.Info(fmt.Sprintf("Secret %s not found in hash secret, creating it", objectHashSecretName))
		_, err = k8sClient.CoreV1().Secrets(podNamespace).Patch(context.Background(), hashSecretName, types.MergePatchType, []byte(secretPatchJSON), metav1.PatchOptions{})
		if err != nil {
			return err
		}
		return nil
	}

	// Set the hash as an annotation on the deployment if it is different from the current once
	if string(hashSecret.Data[objectHashSecretName]) == hashStr {
		log.Info(fmt.Sprintf("Secrets of %s %s/%s are up to date", objectKind, object.GetNamespace(), object.GetName()))
	} else {
		log.Info(fmt.Sprintf("Secrets of %s %s/%s are out of date", objectKind, object.GetNamespace(), object.GetName()))

		// Reload object based on its type
		switch o := object.(type) {
		case *appsv1.Deployment:
			o.Spec.Template.GetAnnotations()["alpha.vault.security.banzaicloud.io/secret-version-hash"] = hashStr
			_, err := k8sClient.AppsV1().Deployments(o.GetNamespace()).Update(context.Background(), o, metav1.UpdateOptions{})
			if err != nil {
				return err
			}
		case *corev1.ConfigMap:
			_, err := k8sClient.CoreV1().ConfigMaps(o.GetNamespace()).Patch(context.Background(), o.GetName(), types.MergePatchType, []byte(o.GetAnnotations()["kubectl.kubernetes.io/last-applied-configuration"]), metav1.PatchOptions{})
			if err != nil {
				return err
			}
		case *corev1.Secret:
			_, err := k8sClient.CoreV1().Secrets(o.GetNamespace()).Patch(context.Background(), o.GetName(), types.MergePatchType, []byte(o.GetAnnotations()["kubectl.kubernetes.io/last-applied-configuration"]), metav1.PatchOptions{})
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown object type: %T", o)
		}

		// Also update the hash in the secret for storing them
		_, err := k8sClient.CoreV1().Secrets(podNamespace).Patch(context.Background(), hashSecretName, types.MergePatchType, []byte(secretPatchJSON), metav1.PatchOptions{})
		if err != nil {
			return err
		}

		log.Info(fmt.Sprintf("Secret version hash of %s %s/%s updated", objectKind, object.GetNamespace(), object.GetName()))
	}
	return nil
}

func getObjectKind(object metav1.Object) string {
	if _, ok := object.(*appsv1.Deployment); ok {
		return "deployment"
	}
	if _, ok := object.(*corev1.ConfigMap); ok {
		return "configmap"
	}
	if _, ok := object.(*corev1.Secret); ok {
		return "secret"
	}
	return ""
}
