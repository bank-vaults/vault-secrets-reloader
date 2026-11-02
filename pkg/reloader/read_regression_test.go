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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	vaultapi "github.com/hashicorp/vault/api"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func readController() *Controller {
	return &Controller{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), workloadSecrets: newWorkloadSecrets(), secretVersions: map[string]int{}, vaultConfig: &VaultConfig{}}
}
func readDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "demo"}, Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{SecretReloadAnnotationName: "true"}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Env: []corev1.EnvVar{{Name: "PASSWORD", Value: "vault:secret/data/demo#password"}}}}}}}}
}
func readPollingController(t *testing.T, failRead *bool) (*Controller, *fake.Clientset) {
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
	c := readController()
	c.vaultClient = v
	k := fake.NewClientset(readDeployment())
	c.kubeClient = k
	c.workloadSecrets.Store(workload{name: "app", namespace: "demo", kind: DeploymentKind}, []string{"secret/data/demo"})
	c.secretVersions["secret/data/demo"] = 1
	return c, k
}

func TestReadErrorPreservesVersionUntilRecovery(t *testing.T) {
	failRead := true
	c, k := readPollingController(t, &failRead)
	c.runReloader(context.Background())
	if got := c.secretVersions["secret/data/demo"]; got != 1 {
		t.Fatalf("version after read error = %d, want 1", got)
	}
	failRead = false
	c.runReloader(context.Background())
	d, err := k.AppsV1().Deployments("demo").Get(context.Background(), "app", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Spec.Template.Annotations[ReloadCountAnnotationName]; got != "1" {
		t.Fatalf("reload count after recovery = %q, want 1", got)
	}
	if got := c.secretVersions["secret/data/demo"]; got != 2 {
		t.Fatalf("version after recovery = %d, want 2", got)
	}
}
