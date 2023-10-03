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
	"time"

	vaultapi "github.com/hashicorp/vault/api"
	"github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	appsinformers "k8s.io/client-go/informers/apps/v1"
	"k8s.io/client-go/kubernetes"
	appslisters "k8s.io/client-go/listers/apps/v1"
	"k8s.io/client-go/tools/cache"
)

const (
	DeploymentKind  = "Deployment"
	DaemonSetKind   = "DaemonSet"
	StatefulSetKind = "StatefulSet"

	SecretReloadAnnotationName = "alpha.vault.security.banzaicloud.io/reload-on-secret-change"
	ReloadCountAnnotationName  = "alpha.vault.security.banzaicloud.io/secret-reload-count"
)

// Controller is the controller implementation for Foo resources
type Controller struct {
	kubeClient  kubernetes.Interface
	vaultClient *vaultapi.Client
	vaultConfig *VaultConfig
	logger      *logrus.Entry

	deploymentsLister  appslisters.DeploymentLister
	deploymentsSynced  cache.InformerSynced
	daemonSetsSynced   cache.InformerSynced
	daemonSetsLister   appslisters.DaemonSetLister
	statefulSetsLister appslisters.StatefulSetLister
	statefulSetsSynced cache.InformerSynced

	// workloadSecrets map[Workload][]string
	workloadSecrets workloadSecretsStore
	secretVersions  map[string]int
}

// NewController returns a new sample controller
func NewController(
	logger *logrus.Entry,
	kubeClient kubernetes.Interface,
	deploymentInformer appsinformers.DeploymentInformer,
	daemonSetInformer appsinformers.DaemonSetInformer,
	statefulSetInformer appsinformers.StatefulSetInformer,
) *Controller {
	controller := &Controller{
		kubeClient:         kubeClient,
		logger:             logger,
		deploymentsLister:  deploymentInformer.Lister(),
		deploymentsSynced:  deploymentInformer.Informer().HasSynced,
		daemonSetsLister:   daemonSetInformer.Lister(),
		daemonSetsSynced:   daemonSetInformer.Informer().HasSynced,
		statefulSetsLister: statefulSetInformer.Lister(),
		statefulSetsSynced: deploymentInformer.Informer().HasSynced,
		workloadSecrets:    newWorkloadSecrets(),
		secretVersions:     make(map[string]int),
	}

	logger.Info("Setting up event handlers")

	// Set up event handlers for Deployments, DaemonSets and StatefulSets
	_, _ = deploymentInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.handleObject,
		UpdateFunc: func(old, new interface{}) { controller.handleObject(new) },
		DeleteFunc: controller.handleObjectDelete,
	})

	_, _ = daemonSetInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.handleObject,
		UpdateFunc: func(old, new interface{}) { controller.handleObject(new) },
		DeleteFunc: controller.handleObjectDelete,
	})

	_, _ = statefulSetInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.handleObject,
		UpdateFunc: func(old, new interface{}) { controller.handleObject(new) },
		DeleteFunc: controller.handleObjectDelete,
	})

	return controller
}

// Run will set up the event handlers for types we are interested in, as well
// as syncing informer caches and starting reloader worker. It will block until stopCh
// is closed, at which point it will wait for the reloader to finish processing.
func (c *Controller) Run(ctx context.Context, reloaderPeriod time.Duration) error {
	defer utilruntime.HandleCrash()

	// Start the informer factories to begin populating the informer caches
	c.logger.Info("Starting vault-secrets-reloader controller")

	// Wait for the caches to be synced before starting reloader
	c.logger.Info("Waiting for informer caches to sync")

	if !cache.WaitForCacheSync(ctx.Done(), c.deploymentsSynced, c.daemonSetsSynced, c.statefulSetsSynced) {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	// Launch reloader to reload resources with changed secrets
	go wait.UntilWithContext(ctx, c.runReloader, reloaderPeriod)

	<-ctx.Done()
	c.logger.Info("Shutting down reloader")

	return nil
}

// handleObject will take any resource implementing metav1.Object and collects
// Vault secret references from environment variables of their pod template to a
// shared store if it is a workload and has the reload annotation set.
func (c *Controller) handleObject(obj interface{}) {
	// Get required params from supported workloads
	var workloadData workload
	var podTemplateSpec corev1.PodTemplateSpec
	switch o := obj.(type) {
	case *appsv1.Deployment:
		workloadData = workload{name: o.Name, namespace: o.Namespace, kind: DeploymentKind}
		podTemplateSpec = o.Spec.Template

	case *appsv1.DaemonSet:
		workloadData = workload{name: o.Name, namespace: o.Namespace, kind: DaemonSetKind}
		podTemplateSpec = o.Spec.Template

	case *appsv1.StatefulSet:
		workloadData = workload{name: o.Name, namespace: o.Namespace, kind: StatefulSetKind}
		podTemplateSpec = o.Spec.Template

	default:
		// Unsupported workload
		c.logger.Error("error decoding object, invalid type")
		return
	}

	// Process workload, skip if reload annotation not present
	if podTemplateSpec.GetAnnotations()[SecretReloadAnnotationName] != "true" {
		return
	}
	c.logger.Debugf("Processing workload: %#v", workloadData)
	c.collectWorkloadSecrets(workloadData, podTemplateSpec)
}

// handleObjectDelete will take any resource implementing metav1.Object and deletes
// it from the shared store if it is a workload and has the reload annotation set.
func (c *Controller) handleObjectDelete(obj interface{}) {
	var object metav1.Object
	var ok bool
	if object, ok = obj.(metav1.Object); !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			c.logger.Error("error decoding object, invalid type")
			return
		}
		object, ok = tombstone.Obj.(metav1.Object)
		if !ok {
			c.logger.Error("error decoding object tombstone, invalid type")
			return
		}
		c.logger.Debug("Recovered deleted object: ", object.GetName())
	}

	var workloadData workload
	var podTemplateSpec corev1.PodTemplateSpec
	switch o := object.(type) {
	case *appsv1.Deployment:
		workloadData = workload{name: o.GetName(), namespace: o.GetNamespace(), kind: DeploymentKind}
		podTemplateSpec = o.Spec.Template

	case *appsv1.DaemonSet:
		workloadData = workload{name: o.GetName(), namespace: o.GetNamespace(), kind: DaemonSetKind}
		podTemplateSpec = o.Spec.Template

	case *appsv1.StatefulSet:
		workloadData = workload{name: o.GetName(), namespace: o.GetNamespace(), kind: StatefulSetKind}
		podTemplateSpec = o.Spec.Template

	default:
		c.logger.Error("error decoding object, invalid type")
		return
	}

	// Delete workload, skip if reload annotation not present
	if podTemplateSpec.GetAnnotations()[SecretReloadAnnotationName] != "true" {
		return
	}
	c.logger.Debugf("Deleting workload from store: %#v", workloadData)
	c.workloadSecrets.Delete(workloadData)
}
