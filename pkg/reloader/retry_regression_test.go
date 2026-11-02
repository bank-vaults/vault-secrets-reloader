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
	"io"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	vaultapi "github.com/hashicorp/vault/api"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func retryController() *Controller {
	return &Controller{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), workloadSecrets: newWorkloadSecrets(), secretVersions: map[string]int{}, vaultConfig: &VaultConfig{}}
}
func retryDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "demo"}, Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{SecretReloadAnnotationName: "true"}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Env: []corev1.EnvVar{{Name: "PASSWORD", Value: "vault:secret/data/demo#password"}}}}}}}}
}
func retryPollingController(t *testing.T, failRead *bool) (*Controller, *fake.Clientset) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/sys/health" {
			if _, err := fmt.Fprint(w, `{"initialized":true,"sealed":false,"standby":false}`); err != nil {
				t.Errorf("write Vault test response: %v", err)
			}
			return
		}
		if *failRead {
			w.WriteHeader(http.StatusInternalServerError)
			if _, err := fmt.Fprint(w, `{"errors":["synthetic read error"]}`); err != nil {
				t.Errorf("write Vault test response: %v", err)
			}
			return
		}
		if _, err := fmt.Fprint(w, `{"data":{"data":{"password":"synthetic"},"metadata":{"version":2}}}`); err != nil {
			t.Errorf("write Vault test response: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	config := vaultapi.DefaultConfig()
	config.Address = server.URL
	config.MaxRetries = 0
	v, err := vaultapi.NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	c := retryController()
	c.vaultClient = v
	k := fake.NewClientset(retryDeployment())
	c.kubeClient = k
	c.workloadSecrets.Store(workload{name: "app", namespace: "demo", kind: DeploymentKind}, []string{"secret/data/demo"})
	c.secretVersions["secret/data/demo"] = 1
	return c, k
}

func TestRetriesFailedWorkloadUpdate(t *testing.T) {
	failRead := false
	c, k := retryPollingController(t, &failRead)
	attempts := 0
	k.PrependReactor("update", "deployments", func(action ktesting.Action) (bool, runtime.Object, error) {
		attempts++
		if attempts == 1 {
			return true, nil, fmt.Errorf("synthetic update failure")
		}
		return false, nil, nil
	})
	c.runReloader(context.Background())
	c.runReloader(context.Background())
	c.runReloader(context.Background())
	if attempts != 2 {
		t.Fatalf("expected one failed update and one successful retry, got %d attempts", attempts)
	}
	d, err := k.AppsV1().Deployments("demo").Get(context.Background(), "app", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Spec.Template.Annotations[ReloadCountAnnotationName]; got != "1" {
		t.Fatalf("reload count = %q, want 1", got)
	}
}

func TestRetryDoesNotReloadSuccessfulConsumersAgain(t *testing.T) {
	failRead := false
	c, k := retryPollingController(t, &failRead)
	second := retryDeployment()
	second.Name = "other"
	if _, err := k.AppsV1().Deployments("demo").Create(context.Background(), second, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	c.workloadSecrets.Store(workload{name: "other", namespace: "demo", kind: DeploymentKind}, []string{"secret/data/demo"})
	var mu sync.Mutex
	attempts := map[string]int{}
	k.PrependReactor("update", "deployments", func(action ktesting.Action) (bool, runtime.Object, error) {
		name := action.(ktesting.UpdateAction).GetObject().(*appsv1.Deployment).Name
		mu.Lock()
		defer mu.Unlock()
		attempts[name]++
		if name == "app" && attempts[name] == 1 {
			return true, nil, fmt.Errorf("synthetic update failure")
		}
		return false, nil, nil
	})
	c.runReloader(context.Background())
	c.runReloader(context.Background())
	if attempts["app"] != 2 || attempts["other"] != 1 {
		t.Fatalf("update attempts = %v, want app=2 and other=1", attempts)
	}
}
