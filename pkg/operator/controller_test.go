// Copyright © 2026 Contributors
// SPDX-License-Identifier: Apache-2.0
package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/bank-vaults/vault-secrets-reloader/api/v1alpha1"
	apps "k8s.io/api/apps/v1"
	authz "k8s.io/api/authorization/v1"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
)

func testDeployment(ns, name string) *apps.Deployment {
	return &apps.Deployment{
		ObjectMeta: meta.ObjectMeta{Namespace: ns, Name: name, UID: types.UID(ns + "-" + name), ResourceVersion: "1", Generation: 1},
		Spec:       apps.DeploymentSpec{Selector: &meta.LabelSelector{MatchLabels: map[string]string{"app": name}}, Template: core.PodTemplateSpec{ObjectMeta: meta.ObjectMeta{Annotations: map[string]string{OptIn: "true", VaultRoleAnnotation: "reloader-role", prefix + "vault-addr": "http://vault.test"}}, Spec: core.PodSpec{Containers: []core.Container{{Name: "app", Image: "test", Env: []core.EnvVar{{Name: "PASSWORD", Value: "vault:kv/data/demo#password"}}}}}}},
	}
}

func TestReconcileIntervalValidation(t *testing.T) {
	if DefaultOptions().ReconcileInterval != time.Minute {
		t.Fatal("default reconciliation interval must be one minute")
	}
	for _, value := range []time.Duration{0, -time.Second} {
		o := testOptions()
		o.ReconcileInterval = value
		if err := o.Validate(); err == nil {
			t.Fatalf("accepted reconciliation interval %v", value)
		}
	}
	o := testOptions()
	o.ReconcileInterval = 30 * time.Second
	if err := o.Validate(); err != nil {
		t.Fatalf("valid reconciliation interval rejected: %v", err)
	}
}

func TestNamespaceReconcileIntervalAndWorkloadEvents(t *testing.T) {
	c, k, d, _, _ := testController(t, "team-a")
	c.options.ReconcileInterval = 3 * time.Second
	c.options.AccessInterval = time.Minute
	k.PrependReactor("create", "selfsubjectaccessreviews", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &authz.SelfSubjectAccessReview{Status: authz.SubjectAccessReviewStatus{Allowed: true}}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	getCount := func() int {
		n := 0
		for _, action := range d.Actions() {
			if action.GetVerb() == "get" && action.GetResource() == api.Resource && action.GetNamespace() == "team-a" {
				n++
			}
		}
		return n
	}
	eventually(t, func() bool { return getCount() >= 2 })
	time.Sleep(200 * time.Millisecond)
	before := getCount()
	time.Sleep(1200 * time.Millisecond)
	if got := getCount(); got != before {
		t.Fatalf("unexpected periodic CR GET before configured interval: before=%d after=%d", before, got)
	}
	dep, err := k.AppsV1().Deployments("team-a").Get(ctx, "app", meta.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dep.Labels = map[string]string{"example": "changed"}
	if _, err = k.AppsV1().Deployments("team-a").Update(ctx, dep, meta.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && getCount() == before {
		time.Sleep(10 * time.Millisecond)
	}
	if getCount() == before {
		t.Fatal("workload event did not trigger reconciliation immediately")
	}
}
func testCR(ns string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": api.GroupVersion.String(), "kind": "VaultReloader", "metadata": map[string]interface{}{"namespace": ns, "name": "default", "uid": ns + "-cr", "resourceVersion": "1", "generation": int64(1)}, "spec": map[string]interface{}{"suspend": false},
	}}
}
func testController(t *testing.T, ns string) (*Controller, *fake.Clientset, *dynamicfake.FakeDynamicClient, *namespaceRuntime, []cache.SharedIndexInformer) {
	t.Helper()
	o := testOptions()
	o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	k := fake.NewClientset(testDeployment(ns, "app"))
	d := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{api.Resource: "VaultReloaderList"}, testCR(ns))
	reader := &metadataFake{calls: map[string]int{}, deny: map[string]bool{}, version: 1}
	c, err := NewController(o, k, d, reader)
	if err != nil {
		t.Fatal(err)
	}
	r := &namespaceRuntime{controller: c, namespace: ns, uid: types.UID(ns + "-cr"), generation: 1, owner: ns + "-cr"}
	informer := cache.NewSharedIndexInformer(&cache.ListWatch{}, &apps.Deployment{}, 0, cache.Indexers{})
	_ = informer.GetStore().Add(testDeployment(ns, "app"))
	return c, k, d, r, []cache.SharedIndexInformer{informer}
}
func seed(t *testing.T, c *Controller, w *apps.Deployment, version int64) {
	t.Helper()
	workload, _ := workloadOf(w)
	s, err := parseSource(workload, c.options)
	if err != nil {
		t.Fatal(err)
	}
	c.poller.mu.Lock()
	defer c.poller.mu.Unlock()
	for _, path := range s.Paths {
		e := c.poller.entries[pollKey(s, path)]
		if e == nil {
			t.Fatal("missing subscription")
		}
		e.observation = Observation{Version: api.Version{Version: version, Created: fmt.Sprint(version), KeyCreated: "k"}, Success: time.Now()}
	}
}
func patchCount(k *fake.Clientset) int {
	n := 0
	for _, a := range k.Actions() {
		if a.GetVerb() == "patch" && a.GetResource().Resource == "deployments" {
			n++
		}
	}
	return n
}
func TestPersistentIntentRetryAndDedup(t *testing.T) {
	c, k, _, r, list := testController(t, "a")
	ctx := context.Background()
	if err := r.reconcile(ctx, list); err != nil {
		t.Fatal(err)
	}
	seed(t, c, testDeployment("a", "app"), 1)
	if err := r.reconcile(ctx, list); err != nil {
		t.Fatal(err)
	}
	if patchCount(k) != 0 {
		t.Fatal("RecordOnly adoption patched workload")
	}
	var fail atomic.Bool
	fail.Store(true)
	k.PrependReactor("patch", "deployments", func(a ktesting.Action) (bool, runtime.Object, error) {
		if fail.Load() {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "app", fmt.Errorf("denied"))
		}
		return false, nil, nil
	})
	seed(t, c, testDeployment("a", "app"), 2)
	if err := r.reconcile(ctx, list); err != nil {
		t.Fatal(err)
	}
	cr, _ := c.getCR(ctx, "a")
	if len(cr.Status.Targets) != 1 || cr.Status.Targets[0].Desired == "" {
		t.Fatal("pending intent lost after failed patch")
	}
	fail.Store(false)
	r.retries = nil
	if err := r.reconcile(ctx, list); err != nil {
		t.Fatal(err)
	}
	if patchCount(k) != 2 {
		t.Fatalf("want failed patch + retry, got %d", patchCount(k))
	}
	r.retries = nil
	if err := r.reconcile(ctx, list); err != nil {
		t.Fatal(err)
	}
	if patchCount(k) != 2 {
		t.Fatal("revision patched twice")
	}
	dep, _ := k.AppsV1().Deployments("a").Get(ctx, "app", meta.GetOptions{})
	if dep.Spec.Template.Annotations[RevisionAnnotation] == "" {
		t.Fatal("no revision marker")
	}
}

func TestLogVersionChangesAndVaultFailuresWithoutRepeating(t *testing.T) {
	c, _, _, r, list := testController(t, "team-a")
	var logs bytes.Buffer
	c.options.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx := context.Background()
	if err := r.reconcile(ctx, list); err != nil {
		t.Fatal(err)
	}
	seed(t, c, testDeployment("team-a", "app"), 1)
	if err := r.reconcile(ctx, list); err != nil {
		t.Fatal(err)
	}
	seed(t, c, testDeployment("team-a", "app"), 2)
	if err := r.reconcile(ctx, list); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "Vault secret version changed") || !strings.Contains(logs.String(), "workload rollout requested") {
		t.Fatalf("missing event logs: %s", logs.String())
	}
	if err := r.reconcile(ctx, list); err != nil {
		t.Fatal(err)
	}
	if strings.Count(logs.String(), "Vault secret version changed") != 1 {
		t.Fatalf("version log repeated: %s", logs.String())
	}
	c.poller.mu.Lock()
	for _, entry := range c.poller.entries {
		entry.observation.Err = "VaultAccessDenied"
		entry.observation.Stage = "login"
		entry.observation.HTTPStatus = 403
	}
	c.poller.mu.Unlock()
	if err := r.reconcile(ctx, list); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcile(ctx, list); err != nil {
		t.Fatal(err)
	}
	if strings.Count(logs.String(), "reason=VaultAccessDenied") != 1 || !strings.Contains(logs.String(), "stage=login http_status=403") {
		t.Fatalf("missing or repeated error: %s", logs.String())
	}
	cr, err := c.getCR(ctx, "team-a")
	if err != nil || cr.Status.Targets[0].Message != "stage=login http_status=403" {
		t.Fatalf("missing status details: %+v %v", cr, err)
	}
}
func TestMissingReloaderRoleDoesNotBlockOtherWorkloads(t *testing.T) {
	c, _, _, r, informers := testController(t, "team-a")
	missing := testDeployment("team-a", "missing-role")
	delete(missing.Spec.Template.Annotations, VaultRoleAnnotation)
	missing.Spec.Template.Annotations[prefix+"vault-role"] = "application-role"
	if err := informers[0].GetStore().Add(missing); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := r.reconcile(ctx, informers); err != nil {
		t.Fatal(err)
	}
	seed(t, c, testDeployment("team-a", "app"), 1)
	if err := r.reconcile(ctx, informers); err != nil {
		t.Fatal(err)
	}
	cr, err := c.getCR(ctx, "team-a")
	if err != nil {
		t.Fatal(err)
	}
	states := make(map[string]api.TargetState)
	for _, state := range cr.Status.Targets {
		states[state.Name] = state
	}
	if state := states["missing-role"]; state.Phase != "UnsupportedConfiguration" || !strings.Contains(state.Message, VaultRoleAnnotation) {
		t.Fatalf("missing role status: %+v", state)
	}
	if state := states["app"]; state.Phase != "BaselineRecorded" {
		t.Fatalf("valid workload was blocked: %+v", state)
	}
}

func TestStatusFailurePreventsWorkloadPatch(t *testing.T) {
	c, k, d, r, list := testController(t, "a")
	ctx := context.Background()
	_ = r.reconcile(ctx, list)
	seed(t, c, testDeployment("a", "app"), 1)
	_ = r.reconcile(ctx, list)
	seed(t, c, testDeployment("a", "app"), 2)
	d.PrependReactor("patch", "vaultreloaders", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("status storage unavailable")
	})
	if err := r.reconcile(ctx, list); err == nil {
		t.Fatal("status failure not returned")
	}
	if patchCount(k) != 0 {
		t.Fatal("workload patched without persisted intent")
	}
}
func TestUnknownPatchOutcomeRecoveredFromMarker(t *testing.T) {
	c, k, _, r, list := testController(t, "a")
	ctx := context.Background()
	_ = r.reconcile(ctx, list)
	seed(t, c, testDeployment("a", "app"), 1)
	_ = r.reconcile(ctx, list)
	// Patch commits but the client sees a timeout. Next reconcile must use the marker.
	once := true
	k.PrependReactor("patch", "deployments", func(a ktesting.Action) (bool, runtime.Object, error) {
		if !once {
			return false, nil, nil
		}
		once = false
		action := a.(ktesting.PatchAction)
		tracked, _ := k.Tracker().Get(apps.SchemeGroupVersion.WithResource("deployments"), "a", "app")
		dep := tracked.(*apps.Deployment).DeepCopy()
		var operations []map[string]interface{}
		_ = json.Unmarshal(action.GetPatch(), &operations)
		dep.Spec.Template.Annotations[RevisionAnnotation] = operations[len(operations)-1]["value"].(string)
		_ = k.Tracker().Update(apps.SchemeGroupVersion.WithResource("deployments"), dep, "a")
		return true, nil, context.DeadlineExceeded
	})
	seed(t, c, testDeployment("a", "app"), 2)
	_ = r.reconcile(ctx, list)
	r.retries = nil
	_ = r.reconcile(ctx, list)
	if patchCount(k) != 1 {
		t.Fatalf("ambiguous outcome caused %d patches", patchCount(k))
	}
	cr, _ := c.getCR(ctx, "a")
	if cr.Status.Targets[0].Requested != cr.Status.Targets[0].Desired {
		t.Fatal("marker did not recover requested revision")
	}
}
func TestReadFailureDoesNotLoseBaseline(t *testing.T) {
	c, k, _, r, list := testController(t, "a")
	ctx := context.Background()
	_ = r.reconcile(ctx, list)
	seed(t, c, testDeployment("a", "app"), 1)
	_ = r.reconcile(ctx, list)
	c.poller.mu.Lock()
	for _, e := range c.poller.entries {
		e.observation.Err = "VaultAccessDenied"
	}
	c.poller.mu.Unlock()
	_ = r.reconcile(ctx, list)
	cr, _ := c.getCR(ctx, "a")
	for _, v := range cr.Status.Targets[0].Versions {
		if v.Version != 1 {
			t.Fatal("baseline lost")
		}
	}
	seed(t, c, testDeployment("a", "app"), 2)
	_ = r.reconcile(ctx, list)
	if patchCount(k) != 1 {
		t.Fatal("recovery did not request rollout")
	}
}
func TestOptOutAndRecreateCancelOldIntent(t *testing.T) {
	c, k, _, r, list := testController(t, "a")
	ctx := context.Background()
	_ = r.reconcile(ctx, list)
	seed(t, c, testDeployment("a", "app"), 1)
	_ = r.reconcile(ctx, list)
	dep := testDeployment("a", "app")
	dep.Spec.Template.Annotations[OptIn] = "false"
	_ = list[0].GetStore().Update(dep)
	_ = r.reconcile(ctx, list)
	cr, _ := c.getCR(ctx, "a")
	if len(cr.Status.Targets) != 0 {
		t.Fatal("opt-out retained state")
	}
	if patchCount(k) != 0 {
		t.Fatal("opt-out patched")
	}
	dep.UID = "new-uid"
	dep.Spec.Template.Annotations[OptIn] = "true"
	_ = list[0].GetStore().Update(dep)
	_ = r.reconcile(ctx, list)
	cr, _ = c.getCR(ctx, "a")
	if cr.Status.Targets[0].UID != "new-uid" {
		t.Fatal("new UID reused old state")
	}
}
func TestSupervisorScopedWatchersAndPermissionRecovery(t *testing.T) {
	c, k, d, _, _ := testController(t, "a")
	o := c.options
	o.AccessInterval = 50 * time.Millisecond
	o.RetryMin = 20 * time.Millisecond
	c.options = o
	var permitted atomic.Bool
	k.PrependReactor("create", "selfsubjectaccessreviews", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &authz.SelfSubjectAccessReview{Status: authz.SubjectAccessReviewStatus{Allowed: permitted.Load()}}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("controller did not stop")
		}
	}()
	eventually(t, func() bool { return c.Ready() && c.metrics.AccessDenied.Load() > 0 })
	for _, a := range k.Actions() {
		if a.GetVerb() == "list" || a.GetVerb() == "watch" {
			t.Fatal("workloads watched before authorization")
		}
	}
	permitted.Store(true)
	eventually(t, func() bool {
		n := 0
		for _, a := range k.Actions() {
			if a.GetVerb() == "watch" {
				if a.GetNamespace() != "a" {
					t.Fatalf("cluster-wide or cross-namespace watch: %v", a)
				}
				n++
			}
		}
		return n >= 3
	})
	eventually(t, func() bool {
		cr, err := c.getCR(context.Background(), "a")
		if err != nil {
			return false
		}
		for _, cond := range cr.Status.Conditions {
			if cond.Type == "AccessReady" && cond.Status == meta.ConditionTrue {
				return true
			}
		}
		return false
	})
	permitted.Store(false)
	eventually(t, func() bool {
		cr, err := c.getCR(context.Background(), "a")
		if err != nil {
			return false
		}
		for _, cond := range cr.Status.Conditions {
			if cond.Type == "AccessReady" && cond.Status == meta.ConditionFalse {
				return true
			}
		}
		return false
	})
	permitted.Store(true)
	eventually(t, func() bool {
		n := 0
		for _, a := range k.Actions() {
			if a.GetVerb() == "watch" {
				n++
			}
		}
		return n >= 6
	})
	cr, err := d.Resource(api.Resource).Namespace("a").Get(ctx, "default", meta.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedField(cr.Object, true, "spec", "suspend")
	cr.SetGeneration(2)
	_, err = d.Resource(api.Resource).Namespace("a").Update(ctx, cr, meta.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return c.metrics.Namespaces.Load() == 0 })
	for _, a := range k.Actions() {
		if strings.Contains(a.GetResource().Resource, "secret") {
			t.Fatalf("unexpected Secret access: %v", a)
		}
	}
}
func TestConditionalPatchRejectsChangedResourceVersion(t *testing.T) {
	k := fake.NewClientset(testDeployment("a", "app"))
	w, _ := workloadOf(testDeployment("a", "app"))
	dep := testDeployment("a", "app")
	dep.ResourceVersion = "2"
	_ = k.Tracker().Update(apps.SchemeGroupVersion.WithResource("deployments"), dep, "a")
	if err := patchRevision(context.Background(), k, w, "revision"); err == nil {
		t.Fatal("stale precondition accepted")
	}
}
func TestRolloutStrategies(t *testing.T) {
	dep := testDeployment("a", "app")
	dep.Spec.Paused = true
	w, _ := workloadOf(dep)
	if ready, _ := rolledOut(w); ready {
		t.Fatal("paused Deployment complete")
	}
	ss := &apps.StatefulSet{Spec: apps.StatefulSetSpec{UpdateStrategy: apps.StatefulSetUpdateStrategy{Type: apps.OnDeleteStatefulSetStrategyType}}}
	w, _ = workloadOf(ss)
	if ready, reason := rolledOut(w); ready || reason != "OnDelete" {
		t.Fatal("OnDelete counted complete")
	}
	ds := &apps.DaemonSet{Spec: apps.DaemonSetSpec{UpdateStrategy: apps.DaemonSetUpdateStrategy{Type: apps.OnDeleteDaemonSetStrategyType}}}
	w, _ = workloadOf(ds)
	if ready, _ := rolledOut(w); ready {
		t.Fatal("OnDelete DaemonSet complete")
	}
}

func TestFailedConsumerDoesNotRepeatSuccessfulConsumer(t *testing.T) {
	c, k, _, r, list := testController(t, "b")
	ctx := context.Background()
	second := testDeployment("b", "second")
	_, err := k.AppsV1().Deployments("b").Create(ctx, second, meta.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = list[0].GetStore().Add(second)
	_ = r.reconcile(ctx, list)
	seed(t, c, testDeployment("b", "app"), 1)
	_ = r.reconcile(ctx, list)
	var fail atomic.Bool
	fail.Store(true)
	k.PrependReactor("patch", "deployments", func(a ktesting.Action) (bool, runtime.Object, error) {
		if a.(ktesting.PatchAction).GetName() == "app" && fail.Load() {
			return true, nil, fmt.Errorf("synthetic failure")
		}
		return false, nil, nil
	})
	seed(t, c, testDeployment("b", "app"), 2)
	_ = r.reconcile(ctx, list)
	fail.Store(false)
	r.retries = nil
	_ = r.reconcile(ctx, list)
	attempts := map[string]int{}
	for _, a := range k.Actions() {
		if a.GetVerb() == "patch" && a.GetResource().Resource == "deployments" {
			attempts[a.(ktesting.PatchAction).GetName()]++
		}
	}
	if attempts["app"] != 2 || attempts["second"] != 1 {
		t.Fatalf("partial success was not isolated: %v", attempts)
	}
}
func TestLeaderElectionShutdownCancelsRuntime(t *testing.T) {
	c, _, _, _, _ := testController(t, "leader")
	c.options.LeaderElection = true
	c.options.LeaderNamespace = "infra-ns"
	c.options.Identity = "unique-test-process"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	eventually(t, func() bool { return c.Ready() })
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("leader runtime did not cancel")
	}
	if c.Ready() {
		t.Fatal("stopped leader reports ready")
	}
}
func TestStateSizeLimitPreventsPatch(t *testing.T) {
	c, k, _, r, list := testController(t, "bounded")
	ctx := context.Background()
	_ = r.reconcile(ctx, list)
	seed(t, c, testDeployment("bounded", "app"), 1)
	_ = r.reconcile(ctx, list)
	c.options.MaxStateBytes = 100
	seed(t, c, testDeployment("bounded", "app"), 2)
	if err := r.reconcile(ctx, list); err == nil {
		t.Fatal("oversized status silently accepted")
	}
	if patchCount(k) > 0 {
		t.Fatal("patch made without durable intent")
	}
}

func TestRemovingAllReferencesClearsPendingMembership(t *testing.T) {
	c, _, _, r, list := testController(t, "refs")
	ctx := context.Background()
	_ = r.reconcile(ctx, list)
	seed(t, c, testDeployment("refs", "app"), 1)
	_ = r.reconcile(ctx, list)
	dep := testDeployment("refs", "app")
	dep.Spec.Template.Spec.Containers[0].Env = nil
	_ = list[0].GetStore().Update(dep)
	if err := r.reconcile(ctx, list); err != nil {
		t.Fatal(err)
	}
	cr, _ := c.getCR(ctx, "refs")
	if len(cr.Status.Targets) != 0 || len(c.poller.entries) != 0 {
		t.Fatal("removed references retained state or polling")
	}
}

func TestStatefulSetCacheMustSyncBeforePolling(t *testing.T) {
	c, k, _, _, _ := testController(t, "cache")
	c.options.SyncTimeout = 150 * time.Millisecond
	c.options.RetryMin = 20 * time.Millisecond
	k.PrependReactor("create", "selfsubjectaccessreviews", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &authz.SelfSubjectAccessReview{Status: authz.SubjectAccessReviewStatus{Allowed: true}}, nil
	})
	k.PrependReactor("list", "statefulsets", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("synthetic StatefulSet list failure")
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("shutdown timed out")
		}
	}()
	eventually(t, func() bool {
		for _, a := range k.Actions() {
			if a.GetVerb() == "list" && a.GetResource().Resource == "statefulsets" {
				return true
			}
		}
		return false
	})
	time.Sleep(250 * time.Millisecond)
	if c.metrics.Polls.Load() != 0 || patchCount(k) != 0 {
		t.Fatal("poll/patch started before StatefulSet cache sync")
	}
}
