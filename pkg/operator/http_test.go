// Copyright © 2026 Contributors
// SPDX-License-Identifier: Apache-2.0
package operator

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	api "github.com/bank-vaults/vault-secrets-reloader/api/v1alpha1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// This is an HTTP protocol stub, not a real API server or RBAC enforcement test.
func TestHTTPClientsUseScopedWorkloadURLs(t *testing.T) {
	var mu sync.Mutex
	paths := []string{}
	cr := testCR("team-a")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("watch") == "true" {
			w.WriteHeader(200)
			if r.URL.Query().Get("sendInitialEvents") == "true" {
				encoder := json.NewEncoder(w)
				if r.URL.Path == "/apis/"+api.Group+"/v1alpha1/vaultreloaders" {
					_ = encoder.Encode(map[string]interface{}{"type": "ADDED", "object": cr.Object})
				}
				_ = encoder.Encode(map[string]interface{}{"type": "BOOKMARK", "object": map[string]interface{}{"apiVersion": "v1", "kind": "PartialObjectMetadata", "metadata": map[string]interface{}{"resourceVersion": "1", "annotations": map[string]string{"k8s.io/initial-events-end": "true"}}}})
			}
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		switch {
		case r.URL.Path == "/apis/"+api.Group+"/v1alpha1/vaultreloaders":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"apiVersion": api.GroupVersion.String(), "kind": "VaultReloaderList", "metadata": map[string]string{"resourceVersion": "1"}, "items": []interface{}{cr.Object}})
		case strings.HasSuffix(r.URL.Path, "/vaultreloaders/default/status"):
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"/status"`) {
				t.Error("expected conditional whole-status patch")
			}
			_ = json.NewEncoder(w).Encode(cr.Object)
		case strings.HasSuffix(r.URL.Path, "/vaultreloaders/default"):
			_ = json.NewEncoder(w).Encode(cr.Object)
		case r.URL.Path == "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"apiVersion": "authorization.k8s.io/v1", "kind": "SelfSubjectAccessReview", "status": map[string]bool{"allowed": true}})
		case strings.HasPrefix(r.URL.Path, "/apis/apps/v1/namespaces/team-a/"):
			kind := "DeploymentList"
			if strings.HasSuffix(r.URL.Path, "/statefulsets") {
				kind = "StatefulSetList"
			}
			if strings.HasSuffix(r.URL.Path, "/daemonsets") {
				kind = "DaemonSetList"
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"apiVersion": "apps/v1", "kind": kind, "metadata": map[string]string{"resourceVersion": "1"}, "items": []interface{}{}})
		default:
			w.WriteHeader(403)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"apiVersion": "v1", "kind": "Status", "status": "Failure", "reason": "Forbidden", "code": 403})
		}
	}))
	defer server.Close()
	cfg := &rest.Config{Host: server.URL, QPS: 100, Burst: 100}
	k, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	o := testOptions()
	o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	c, err := NewController(o, k, d, &metadataFake{calls: map[string]int{}, deny: map[string]bool{}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				if err != context.Canceled {
					t.Error(err)
				}
			}
		case <-time.After(3 * time.Second):
			t.Error("HTTP watchers did not cancel")
		}
	}()
	eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, path := range paths {
			if strings.HasSuffix(path, "/daemonsets") {
				return true
			}
		}
		return false
	})
	mu.Lock()
	defer mu.Unlock()
	for _, path := range paths {
		if strings.Contains(path, "/apis/apps/") && !strings.Contains(path, "/namespaces/team-a/") {
			t.Fatalf("unscoped workload request: %s", path)
		}
		if (strings.Contains(path, "/secrets/") || strings.HasSuffix(path, "/secrets")) || strings.HasSuffix(path, "/namespaces") {
			t.Fatalf("unneeded discovery or Secret request: %s", path)
		}
	}
}
