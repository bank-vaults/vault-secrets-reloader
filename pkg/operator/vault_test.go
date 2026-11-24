// Copyright © 2026 Contributors
// SPDX-License-Identifier: Apache-2.0
package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/bank-vaults/vault-secrets-reloader/api/v1alpha1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func testOptions() Options {
	o := DefaultOptions()
	o.LeaderElection = false
	o.AllowHTTP = true
	o.AllowedAddresses = []string{"http://vault.test"}
	o.PollInterval = 100 * time.Millisecond
	o.ReconcileInterval = 100 * time.Millisecond
	o.RetryMin = 20 * time.Millisecond
	o.RetryMax = 100 * time.Millisecond
	o.RequestTimeout = 200 * time.Millisecond
	o.SyncTimeout = 200 * time.Millisecond
	o.AccessInterval = 100 * time.Millisecond
	o.VaultQPS = 1000
	return o
}
func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}
func TestMetadataOnlyAndPermissionRecovery(t *testing.T) {
	var deny atomic.Bool
	var version atomic.Int64
	version.Store(1)
	var forbiddenData atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/auth/kubernetes/login":
			_, _ = fmt.Fprint(w, `{"auth":{"client_token":"test-token","lease_duration":600}}`)
		case "/v1/team/kv/metadata/payments/db":
			if deny.Load() {
				w.WriteHeader(403)
				_, _ = fmt.Fprint(w, `{"errors":["do not log synthetic password"]}`)
				return
			}
			n := version.Load()
			_, _ = fmt.Fprintf(w, `{"data":{"current_version":%d,"created_time":"key-created","custom_metadata":{"private":"never persist"},"versions":{"%d":{"created_time":"version-created","deletion_time":"","destroyed":false}}}}`, n, n)
		default:
			forbiddenData.Add(1)
			w.WriteHeader(403)
		}
	}))
	defer server.Close()
	o := testOptions()
	o.JWTFile = filepath.Join(t.TempDir(), "jwt")
	if err := os.WriteFile(o.JWTFile, []byte("synthetic-jwt"), 0600); err != nil {
		t.Fatal(err)
	}
	o.AllowedAddresses = []string{server.URL}
	reader := NewVaultReader(o, fake.NewClientset())
	defer reader.Close()
	s := Source{Tenant: "a", Address: server.URL, AuthMount: "kubernetes", Role: "a"}
	got, err := reader.Read(context.Background(), s, "team/kv/data/payments/db")
	if err != nil || got.Version != 1 {
		t.Fatalf("version=%v err=%v", got, err)
	}
	deny.Store(true)
	_, err = reader.Read(context.Background(), s, "team/kv/data/payments/db")
	if err == nil || err.Error() != "VaultAccessDenied" {
		t.Fatalf("expected sanitized access error, got %v", err)
	}
	deny.Store(false)
	version.Store(2)
	got, err = reader.Read(context.Background(), s, "team/kv/data/payments/db")
	if err != nil || got.Version != 2 {
		t.Fatalf("recovery=%v %v", got, err)
	}
	if forbiddenData.Load() != 0 {
		t.Fatal("attempted a non-metadata/data fallback endpoint")
	}
}
func TestMetadataValidationAndLifecycle(t *testing.T) {
	cases := []struct{ payload, code string }{
		{`{"data":{"current_version":0}}`, "InvalidMetadata"},
		{`{"data":{"current_version":1,"created_time":"k","versions":{"1":{"created_time":"v","destroyed":true}}}}`, "VersionDestroyed"},
		{`{"data":{"current_version":1,"created_time":"k","versions":{"1":{"created_time":"v","deletion_time":"2000-01-01T00:00:00Z"}}}}`, "VersionDeleted"},
		{`{"data":{"current_version":1,"created_time":"k","versions":{}}}`, "InvalidMetadata"},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/login") {
					_, _ = fmt.Fprint(w, `{"auth":{"client_token":"t","lease_duration":600}}`)
				} else {
					_, _ = fmt.Fprint(w, tc.payload)
				}
			}))
			defer srv.Close()
			o := testOptions()
			o.JWTFile = filepath.Join(t.TempDir(), "jwt")
			_ = os.WriteFile(o.JWTFile, []byte("jwt"), 0600)
			o.AllowedAddresses = []string{srv.URL}
			v := NewVaultReader(o, fake.NewClientset())
			defer v.Close()
			_, err := v.Read(context.Background(), Source{Address: srv.URL, AuthMount: "kubernetes"}, "kv/data/x")
			if err == nil || err.Error() != tc.code {
				t.Fatalf("want %s, got %v", tc.code, err)
			}
		})
	}
}
func TestLoginFailureDoesNotBlockOtherIdentity(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/login") {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["role"] == "bad" {
				once.Do(func() { close(entered) })
				<-release
				w.WriteHeader(403)
				return
			}
			_, _ = fmt.Fprint(w, `{"auth":{"client_token":"good","lease_duration":600}}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"data":{"current_version":1,"created_time":"k","versions":{"1":{"created_time":"v"}}}}`)
	}))
	defer srv.Close()
	defer close(release)
	o := testOptions()
	o.RequestTimeout = time.Second
	o.JWTFile = filepath.Join(t.TempDir(), "jwt")
	_ = os.WriteFile(o.JWTFile, []byte("jwt"), 0600)
	o.AllowedAddresses = []string{srv.URL}
	reader := NewVaultReader(o, fake.NewClientset())
	defer reader.Close()
	badDone := make(chan struct{})
	go func() {
		defer close(badDone)
		_, _ = reader.Read(context.Background(), Source{Tenant: "bad", Address: srv.URL, AuthMount: "kubernetes", Role: "bad"}, "kv/data/x")
	}()
	<-entered
	started := time.Now()
	_, err := reader.Read(context.Background(), Source{Tenant: "good", Address: srv.URL, AuthMount: "kubernetes", Role: "good"}, "kv/data/x")
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > 500*time.Millisecond {
		t.Fatal("other identity blocked behind failed login")
	}
	// close/release happens before session Close so the blocked login can finish.
	select {
	case <-badDone:
	case <-time.After(2 * time.Second):
		t.Fatal("login did not time out")
	}
}
func TestSourceSecurityAndPathParsing(t *testing.T) {
	w := testDeployment("team-a", "app")
	o := testOptions()
	w.Spec.Template.Annotations[prefix+"vault-addr"] = "http://vault.test"
	workload, _ := workloadOf(w)
	s, err := parseSource(workload, o)
	if err != nil {
		t.Fatal(err)
	}
	if s.Role != "reloader-role" || len(s.Paths) != 1 {
		t.Fatalf("unexpected source: %+v", s)
	}
	s2 := s
	s2.Tenant = "team-b"
	if s.ContextKey() == s2.ContextKey() {
		t.Fatal("cross-tenant credential contexts merged")
	}
	s2 = s
	s2.Role = "other"
	if s.ContextKey() == s2.ContextKey() {
		t.Fatal("roles merged")
	}
	w.Spec.Template.Annotations[prefix+"vault-addr"] = "http://attacker.test"
	workload, _ = workloadOf(w)
	if _, e := parseSource(workload, o); e == nil {
		t.Fatal("unapproved origin accepted")
	}
	w.Spec.Template.Annotations[prefix+"vault-addr"] = "http://vault.test"
	w.Spec.Template.Annotations[prefix+"vault-tls-secret"] = "any-secret"
	workload, _ = workloadOf(w)
	if _, e := parseSource(workload, o); e == nil {
		t.Fatal("unapproved Secret read")
	}
	for _, p := range []string{"kv/data/x", "team/kv/data/a b"} {
		got, e := metadataPath(p)
		if e != nil || !strings.Contains(got, "/metadata/") {
			t.Fatalf("path %s => %s %v", p, got, e)
		}
	}
	for _, p := range []string{"kv/x", "/kv/data/x", "kv/data/../x", "kv/data/x?x=y", "kv/data/x/data/y"} {
		if _, e := metadataPath(p); e == nil {
			t.Fatalf("unsafe/ambiguous path accepted: %s", p)
		}
	}
}

func TestReloaderRoleIsRequiredAndIndependentOfApplicationRole(t *testing.T) {
	dep := testDeployment("team-a", "app")
	dep.Spec.Template.Annotations[prefix+"vault-role"] = "application-role"
	dep.Spec.Template.Spec.ServiceAccountName = "application-sa"
	for _, role := range []string{"", "   "} {
		dep.Spec.Template.Annotations[VaultRoleAnnotation] = role
		w, _ := workloadOf(dep)
		if _, err := parseSource(w, testOptions()); err == nil || !strings.Contains(err.Error(), VaultRoleAnnotation) {
			t.Fatalf("missing role accepted or unclear error: %v", err)
		}
	}
	dep.Spec.Template.Annotations[VaultRoleAnnotation] = "metadata-role"
	w, _ := workloadOf(dep)
	first, err := parseSource(w, testOptions())
	if err != nil || first.Role != "metadata-role" {
		t.Fatalf("operator role: %q, error: %v", first.Role, err)
	}
	dep.Spec.Template.Annotations[VaultRoleAnnotation] = "other-role"
	w, _ = workloadOf(dep)
	second, err := parseSource(w, testOptions())
	if err != nil || second.Role != "other-role" || first.ContextKey() == second.ContextKey() {
		t.Fatalf("role change did not change Vault context: %q, %v", second.Role, err)
	}
}

func TestVaultLoginUsesOperatorJWTAndReloaderRole(t *testing.T) {
	var login struct{ Role, JWT string }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/login") {
			if err := json.NewDecoder(r.Body).Decode(&login); err != nil {
				t.Error(err)
			}
			_, _ = fmt.Fprint(w, `{"auth":{"client_token":"token","lease_duration":600}}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"data":{"current_version":1,"created_time":"k","versions":{"1":{"created_time":"v"}}}}`)
	}))
	defer srv.Close()
	o := testOptions()
	o.JWTFile = filepath.Join(t.TempDir(), "jwt")
	if err := os.WriteFile(o.JWTFile, []byte("operator-jwt"), 0600); err != nil {
		t.Fatal(err)
	}
	o.AllowedAddresses = []string{srv.URL}
	dep := testDeployment("team-a", "app")
	dep.Spec.Template.Annotations[prefix+"vault-addr"] = srv.URL
	dep.Spec.Template.Annotations[prefix+"vault-role"] = "application-role"
	dep.Spec.Template.Annotations[VaultRoleAnnotation] = "metadata-role"
	w, _ := workloadOf(dep)
	s, err := parseSource(w, o)
	if err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientset()
	reader := NewVaultReader(o, kube)
	defer reader.Close()
	if _, err = reader.Read(context.Background(), s, "kv/data/demo"); err != nil {
		t.Fatal(err)
	}
	if login.JWT != "operator-jwt" || login.Role != "metadata-role" {
		t.Fatalf("wrong Vault login identity or role: role=%q", login.Role)
	}
	if len(kube.Actions()) != 0 {
		t.Fatalf("unexpected Kubernetes calls during login: %v", kube.Actions())
	}
}

func TestEmptyOperatorJWTDoesNotContactVault(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
	}))
	defer srv.Close()
	o := testOptions()
	o.JWTFile = filepath.Join(t.TempDir(), "jwt")
	if err := os.WriteFile(o.JWTFile, []byte("  \n"), 0600); err != nil {
		t.Fatal(err)
	}
	o.AllowedAddresses = []string{srv.URL}
	reader := NewVaultReader(o, fake.NewClientset())
	defer reader.Close()
	_, err := reader.Read(context.Background(), Source{Address: srv.URL, AuthMount: "kubernetes", Role: "metadata-role"}, "kv/data/demo")
	if err == nil || err.Error() != "JWTUnavailable" || requests.Load() != 0 {
		t.Fatalf("empty JWT handling: err=%v, requests=%d", err, requests.Load())
	}
}
func TestDisabledSecretReadNeverReachesAPI(t *testing.T) {
	k := fake.NewClientset(&core.Secret{ObjectMeta: meta.ObjectMeta{Name: "ca", Namespace: "a"}})
	o := testOptions()
	v := NewVaultReader(o, k)
	_, err := v.Read(context.Background(), Source{Tenant: "a", Address: "http://vault.test", CASecret: "ca"}, "kv/data/x")
	if err == nil {
		t.Fatal("unexpected success")
	}
	if len(k.Actions()) != 0 {
		t.Fatal("Secret API called despite disabled option")
	}
}

func TestMetadataRecreateSameVersionChangesIdentity(t *testing.T) {
	a := api.Version{Version: 1, Created: "old", KeyCreated: "old-key"}
	b := api.Version{Version: 1, Created: "new", KeyCreated: "new-key"}
	if digest(a) == digest(b) {
		t.Fatal("metadata recreation not detected")
	}
}

func TestControllerJWTFileRotationInvalidatesLogin(t *testing.T) {
	var logins atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/login") {
			logins.Add(1)
			_, _ = fmt.Fprint(w, `{"auth":{"client_token":"short-lived","lease_duration":600}}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"data":{"current_version":1,"created_time":"k","versions":{"1":{"created_time":"v"}}}}`)
	}))
	defer srv.Close()
	o := testOptions()
	o.AllowedAddresses = []string{srv.URL}
	o.JWTFile = filepath.Join(t.TempDir(), "jwt")
	if err := os.WriteFile(o.JWTFile, []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	v := NewVaultReader(o, fake.NewClientset())
	defer v.Close()
	s := Source{Tenant: "a", Address: srv.URL, AuthMount: "kubernetes"}
	if _, err := v.Read(context.Background(), s, "kv/data/x"); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Read(context.Background(), s, "kv/data/x"); err != nil {
		t.Fatal(err)
	}
	if logins.Load() != 1 {
		t.Fatal("unexpected login before JWT rotation")
	}
	if err := os.WriteFile(o.JWTFile, []byte("second"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Read(context.Background(), s, "kv/data/x"); err != nil {
		t.Fatal(err)
	}
	if logins.Load() != 2 {
		t.Fatal("old login survived JWT file replacement")
	}
}

func TestReferenceCompatibilityWithoutReadingValues(t *testing.T) {
	o := testOptions()
	dep := testDeployment("a", "app")
	dep.Spec.Template.Spec.Containers[0].Env = []core.EnvVar{{Name: "ORDINARY", Value: "${HOME}/cache"}, {Name: "TEMPLATE_KEY", Value: "vault:kv/data/demo#${.password}"}, {Name: "PINNED", Value: "vault:kv/data/pinned#password#4"}}
	dep.Spec.Template.Spec.InitContainers = []core.Container{{Name: "init", Env: []core.EnvVar{{Name: "EXTRA", Value: "vault:kv/data/init#key"}}}}
	dep.Spec.Template.Annotations[prefix+"vault-from-path"] = "kv/data/all,kv/data/pinned#2"
	w, _ := workloadOf(dep)
	s, err := parseSource(w, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Paths) != 3 {
		t.Fatalf("expected env, init and from-path without pinned refs: %v", s.Paths)
	}
	dep.Spec.Template.Spec.Containers[0].Env = []core.EnvVar{{Name: "DYNAMIC", Value: "vault:kv/data/${name}#password"}}
	w, _ = workloadOf(dep)
	if _, err = parseSource(w, o); err == nil {
		t.Fatal("dynamic path accepted")
	}
}

func TestReferenceDiscoveryIgnoresIndirectEnvironment(t *testing.T) {
	dep := testDeployment("a", "app")
	dep.Spec.Template.Spec.Containers[0].Env = []core.EnvVar{
		{Name: "DIRECT", Value: "vault:kv/data/direct#password"},
		{Name: "POD", ValueFrom: &core.EnvVarSource{FieldRef: &core.ObjectFieldSelector{FieldPath: "metadata.name"}}},
		{Name: "CPU", ValueFrom: &core.EnvVarSource{ResourceFieldRef: &core.ResourceFieldSelector{Resource: "limits.cpu"}}},
		{Name: "CONFIG", ValueFrom: &core.EnvVarSource{ConfigMapKeyRef: &core.ConfigMapKeySelector{LocalObjectReference: core.LocalObjectReference{Name: "settings"}, Key: "key"}}},
		{Name: "SECRET", ValueFrom: &core.EnvVarSource{SecretKeyRef: &core.SecretKeySelector{LocalObjectReference: core.LocalObjectReference{Name: "secrets"}, Key: "key"}}},
	}
	dep.Spec.Template.Spec.Containers[0].EnvFrom = []core.EnvFromSource{
		{ConfigMapRef: &core.ConfigMapEnvSource{LocalObjectReference: core.LocalObjectReference{Name: "settings"}}},
		{SecretRef: &core.SecretEnvSource{LocalObjectReference: core.LocalObjectReference{Name: "secrets"}}},
	}
	dep.Spec.Template.Spec.InitContainers = []core.Container{{Name: "init", Env: []core.EnvVar{
		{Name: "DIRECT", Value: "vault:kv/data/init#key"},
		{Name: "POD", ValueFrom: &core.EnvVarSource{FieldRef: &core.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
	}}}
	dep.Spec.Template.Annotations[prefix+"vault-from-path"] = "kv/data/from-path"
	w, _ := workloadOf(dep)
	s, err := parseSource(w, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(s.Paths, ","); got != "kv/data/direct,kv/data/from-path,kv/data/init" {
		t.Fatalf("unexpected discovered paths: %s", got)
	}
}

func TestPlatformCAFileTakesPrecedenceOverWebhookSecret(t *testing.T) {
	dep := testDeployment("a", "app")
	dep.Spec.Template.Annotations[prefix+"vault-tls-secret"] = "webhook-ca"
	w, _ := workloadOf(dep)
	o := testOptions()
	o.CAFile = "/platform/ca.crt"
	s, err := parseSource(w, o)
	if err != nil || s.CAFile != o.CAFile || s.CASecret != "" {
		t.Fatalf("platform CA not selected: file=%q secret=%q err=%v", s.CAFile, s.CASecret, err)
	}
	o.CAFile = ""
	if _, err := parseSource(w, o); err == nil {
		t.Fatal("unapproved CA secret accepted when no platform CA is configured")
	}
}

func TestWorkloadAnnotationOptIn(t *testing.T) {
	cases := []struct {
		name, workloadAnnotation, templateAnnotation, workloadLabel, templateLabel string
		want                                                                       bool
	}{
		{"workload annotation", "true", "", "", "", true},
		{"template annotation", "", "true", "", "", true},
		{"labels alone ignored", "", "", "true", "true", false},
		{"disabled annotations", "false", "false", "true", "true", false},
		{"either annotation enables", "true", "false", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dep := testDeployment("team-a", "app")
			dep.Annotations = map[string]string{OptIn: tc.workloadAnnotation}
			dep.Spec.Template.Annotations[OptIn] = tc.templateAnnotation
			dep.Labels = map[string]string{OptIn: tc.workloadLabel}
			dep.Spec.Template.Labels = map[string]string{OptIn: tc.templateLabel}
			w, err := workloadOf(dep)
			if err != nil || w.Enabled() != tc.want {
				t.Fatalf("enabled=%v want=%v err=%v", w.Enabled(), tc.want, err)
			}
		})
	}
}

func TestWebhookTLSAnnotation(t *testing.T) {
	for _, tc := range []struct {
		name, modern, legacy string
		want                 bool
		invalid              bool
	}{
		{"default", "", "", false, false},
		{"modern true", "true", "", true, false},
		{"legacy true", "", "true", true, false},
		{"modern false overrides legacy", "false", "true", false, false},
		{"invalid rejected", "invalid", "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dep := testDeployment("team-a", "app")
			if tc.modern != "" {
				dep.Spec.Template.Annotations[prefix+"vault-skip-verify"] = tc.modern
			}
			if tc.legacy != "" {
				dep.Spec.Template.Annotations[deprecatedPrefix+"vault-skip-verify"] = tc.legacy
			}
			dep.Spec.Template.Annotations["secrets-reloader.security.bank-vaults.io/reloader-skip-tls-verify"] = "true"
			dep.Labels = map[string]string{prefix + "vault-skip-verify": "true", "secrets-reloader.security.bank-vaults.io/reloader-skip-tls-verify": "true"}
			w, _ := workloadOf(dep)
			s, err := parseSource(w, testOptions())
			if (err != nil) != tc.invalid || (!tc.invalid && s.SkipTLSVerify != tc.want) {
				t.Fatalf("skip=%v err=%v", s.SkipTLSVerify, err)
			}
		})
	}
}

func TestVaultErrorIdentifiesOperationAndHTTPStatus(t *testing.T) {
	for _, stage := range []string{"login", "metadata"} {
		t.Run(stage, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/login") && stage == "metadata" {
					_, _ = fmt.Fprint(w, `{"auth":{"client_token":"token-secret","lease_duration":600}}`)
					return
				}
				w.WriteHeader(http.StatusForbidden)
				_, _ = fmt.Fprint(w, "private upstream response")
			}))
			defer srv.Close()
			o := testOptions()
			var logs bytes.Buffer
			o.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			o.AllowedAddresses = []string{srv.URL}
			o.JWTFile = filepath.Join(t.TempDir(), "jwt")
			if err := os.WriteFile(o.JWTFile, []byte("secret-jwt"), 0600); err != nil {
				t.Fatal(err)
			}
			r := NewVaultReader(o, fake.NewClientset())
			defer r.Close()
			_, err := r.Read(context.Background(), Source{Address: srv.URL, AuthMount: "kubernetes", Role: "app", Tenant: "team-a"}, "kv/data/demo")
			var pe *PollError
			if !errors.As(err, &pe) || pe.Code != "VaultAccessDenied" || pe.Stage != stage || pe.HTTPStatus != 403 {
				t.Fatalf("unexpected error: %#v", err)
			}
			if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "secret") {
				t.Fatal("sensitive content leaked")
			}
			for _, sensitive := range []string{"private upstream response", "secret-jwt", "token-secret"} {
				if strings.Contains(logs.String(), sensitive) {
					t.Fatalf("debug log leaked %q", sensitive)
				}
			}
		})
	}
}

func TestSkipTLSVerificationIsExplicitAndIsolated(t *testing.T) {
	var logins atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/login") {
			logins.Add(1)
			_, _ = fmt.Fprint(w, `{"auth":{"client_token":"test","lease_duration":600}}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"data":{"current_version":1,"created_time":"k","versions":{"1":{"created_time":"v"}}}}`)
	}))
	defer srv.Close()
	o := testOptions()
	o.AllowedAddresses = []string{srv.URL}
	o.JWTFile = filepath.Join(t.TempDir(), "jwt")
	if err := os.WriteFile(o.JWTFile, []byte("test-jwt"), 0600); err != nil {
		t.Fatal(err)
	}
	dep := testDeployment("team-a", "app")
	dep.Spec.Template.Annotations[prefix+"vault-addr"] = srv.URL
	w, _ := workloadOf(dep)
	verified, err := parseSource(w, o)
	if err != nil {
		t.Fatal(err)
	}
	reader := NewVaultReader(o, fake.NewClientset())
	defer reader.Close()
	if _, err = reader.Read(context.Background(), verified, "kv/data/demo"); err == nil || logins.Load() != 0 {
		t.Fatalf("untrusted server accepted or received JWT: %v", err)
	}
	var tlsErr *PollError
	if !errors.As(err, &tlsErr) || tlsErr.Code != "VaultTLSUnknownAuthority" || tlsErr.Stage != "login" {
		t.Fatalf("TLS error missing detail: %v", err)
	}
	dep.Spec.Template.Annotations[prefix+"vault-skip-verify"] = "true"
	w, _ = workloadOf(dep)
	insecure, err := parseSource(w, o)
	if err != nil || !insecure.SkipTLSVerify {
		t.Fatalf("webhook annotation not recognized: %+v %v", insecure, err)
	}
	if verified.ContextKey() == insecure.ContextKey() {
		t.Fatal("verified and unverified TLS contexts must not share a client")
	}
	// Bypass does not require a configured CA file to exist.
	insecure.CAFile = filepath.Join(t.TempDir(), "missing-ca")
	if version, err := reader.Read(context.Background(), insecure, "kv/data/demo"); err != nil || version.Version != 1 {
		t.Fatalf("explicit bypass failed: %+v %v", version, err)
	}
	if _, err = reader.Read(context.Background(), verified, "kv/data/demo"); err == nil || logins.Load() != 1 {
		t.Fatalf("secure client reused insecure connection: %v", err)
	}
	dep.Spec.Template.Annotations[prefix+"vault-addr"] = "https://unapproved.example"
	w, _ = workloadOf(dep)
	if _, err := parseSource(w, o); err == nil {
		t.Fatal("TLS bypass must not bypass the Vault origin allowlist")
	}
	b, _ := json.Marshal(verified)
	if strings.Contains(string(b), "SkipTLSVerify") {
		t.Fatal("default source serialization must remain compatible with existing status hashes")
	}
}
