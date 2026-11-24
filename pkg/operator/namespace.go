// Copyright © 2026 Contributors
// SPDX-License-Identifier: Apache-2.0
package operator

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"sync"
	"time"

	api "github.com/bank-vaults/vault-secrets-reloader/api/v1alpha1"
	authz "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
)

type namespaceRuntime struct {
	controller   *Controller
	namespace    string
	uid          types.UID
	generation   int64
	owner        string
	cancel       context.CancelFunc
	done         chan struct{}
	retries      map[string]retryState
	pendingCount int64
	stalledCount int64
}

type retryState struct {
	due      time.Time
	failures int
}

func (r *namespaceRuntime) run(ctx context.Context) {
	c := r.controller
	c.options.Logger.Info("namespace monitoring started", "namespace", r.namespace)
	defer c.options.Logger.Info("namespace monitoring stopped", "namespace", r.namespace)
	c.metrics.Namespaces.Add(1)
	defer c.metrics.Namespaces.Add(-1)
	defer func() { c.metrics.Pending.Add(-r.pendingCount); c.metrics.Stalled.Add(-r.stalledCount) }()
	defer c.poller.RemoveOwner(r.owner)
	delay := c.options.RetryMin
	for ctx.Err() == nil {
		if err := r.checkAccess(ctx); err != nil {
			c.metrics.AccessDenied.Add(1)
			r.reportAccess(ctx, false)
			c.options.Logger.Warn("namespace access unavailable", "namespace", r.namespace, "reason", err.Error())
			if !waitContext(ctx, delay+time.Duration(rand.Float64()*float64(delay)/5)) {
				return
			}
			delay *= 2
			if delay > c.options.RetryMax {
				delay = c.options.RetryMax
			}
			continue
		}
		delay = c.options.RetryMin
		err := r.watch(ctx)
		c.poller.RemoveOwner(r.owner)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			c.options.Logger.Warn("namespace runtime restarting", "namespace", r.namespace)
		}
		if !waitContext(ctx, delay) {
			return
		}
	}
}

func waitContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
func (r *namespaceRuntime) checkAccess(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, r.controller.options.RequestTimeout)
	defer cancel()
	for _, resource := range []string{"deployments", "statefulsets", "daemonsets"} {
		for _, verb := range []string{"get", "list", "watch", "patch"} {
			r.controller.options.Logger.Debug("checking namespace access", "namespace", r.namespace, "resource", resource, "verb", verb)
			review, err := r.controller.kube.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authz.SelfSubjectAccessReview{Spec: authz.SelfSubjectAccessReviewSpec{ResourceAttributes: &authz.ResourceAttributes{Namespace: r.namespace, Group: "apps", Resource: resource, Verb: verb}}}, meta.CreateOptions{})
			if err != nil {
				return fmt.Errorf("access review failed for %s %s: %s", verb, resource, apierrors.ReasonForError(err))
			}
			if !review.Status.Allowed {
				return fmt.Errorf("access denied for %s %s", verb, resource)
			}
		}
	}
	return nil
}
func (r *namespaceRuntime) reportAccess(ctx context.Context, allowed bool) {
	cr, err := r.controller.getCR(ctx, r.namespace)
	if err != nil || cr.UID != r.uid {
		return
	}
	status := copyStatus(cr.Status)
	condition := meta.ConditionFalse
	reason := "AccessDenied"
	if allowed {
		condition = meta.ConditionTrue
		reason = "Authorized"
	}
	apimeta.SetStatusCondition(&status.Conditions, meta.Condition{Type: "AccessReady", Status: condition, Reason: reason, Message: "Namespace workload authorization", ObservedGeneration: cr.Generation, LastTransitionTime: meta.Now()})
	if !allowed {
		apimeta.SetStatusCondition(&status.Conditions, meta.Condition{Type: "Ready", Status: meta.ConditionFalse, Reason: reason, Message: "Namespace workload access unavailable", ObservedGeneration: cr.Generation, LastTransitionTime: meta.Now()})
	}
	_ = r.controller.writeStatus(ctx, cr, status)
}
func (r *namespaceRuntime) watch(parent context.Context) error {
	if r.namespace == "" {
		return errors.New("empty watch namespace")
	}
	ctx, cancel := context.WithCancel(parent)
	factory := informers.NewSharedInformerFactoryWithOptions(r.controller.kube, 0, informers.WithNamespace(r.namespace))
	defer func() { cancel(); factory.Shutdown() }()
	list := []cache.SharedIndexInformer{factory.Apps().V1().Deployments().Informer(), factory.Apps().V1().StatefulSets().Informer(), factory.Apps().V1().DaemonSets().Informer()}
	r.controller.options.Logger.Debug("starting workload informers", "namespace", r.namespace)
	changed := make(chan struct{}, 1)
	watchErrors := make(chan error, 1)
	signal := func(event string, obj interface{}) {
		if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
			obj = tombstone.Obj
		}
		if w, err := workloadOf(obj); err == nil {
			r.controller.options.Logger.Debug("workload informer event", "namespace", r.namespace, "event", event, "kind", w.Kind, "name", w.Name)
		}
		select {
		case changed <- struct{}{}:
		default:
		}
	}
	registrations := make([]cache.ResourceEventHandlerRegistration, 0, len(list))
	for _, informer := range list {
		reg, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{AddFunc: func(obj interface{}) { signal("add", obj) }, UpdateFunc: func(_, obj interface{}) { signal("update", obj) }, DeleteFunc: func(obj interface{}) { signal("delete", obj) }})
		if err != nil {
			return err
		}
		registrations = append(registrations, reg)
		if err = informer.SetWatchErrorHandlerWithContext(func(_ context.Context, _ *cache.Reflector, err error) {
			select {
			case watchErrors <- err:
			default:
			}
		}); err != nil {
			return err
		}
	}
	factory.Start(ctx.Done())
	syncctx, stop := context.WithTimeout(ctx, r.controller.options.SyncTimeout)
	checks := []cache.InformerSynced{}
	for i, informer := range list {
		checks = append(checks, informer.HasSynced, registrations[i].HasSynced)
	}
	synced := cache.WaitForCacheSync(syncctx.Done(), checks...)
	stop()
	if !synced {
		r.reportAccess(ctx, false)
		return errors.New("namespace caches did not synchronize")
	}
	r.controller.options.Logger.Debug("workload informer caches synchronized", "namespace", r.namespace)
	r.reportAccess(ctx, true)
	tick := time.NewTicker(r.controller.options.ReconcileInterval)
	defer tick.Stop()
	access := time.NewTicker(r.controller.options.AccessInterval)
	defer access.Stop()
	var reconcileMu sync.Mutex
	reconcile := func() {
		reconcileMu.Lock()
		defer reconcileMu.Unlock()
		if err := r.reconcile(ctx, list); err != nil {
			r.controller.options.Logger.Warn("namespace reconciliation deferred", "namespace", r.namespace)
		}
	}
	reconcile()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-changed:
			reconcile()
		case <-tick.C:
			reconcile()
		case <-watchErrors: // Stop stale-cache processing until a fresh factory is synchronized.
			return errors.New("workload watch interrupted")
		case <-access.C:
			if err := r.checkAccess(ctx); err != nil {
				r.reportAccess(ctx, false)
				return err
			}
		}
	}
}
func (r *namespaceRuntime) reconcile(parent context.Context, list []cache.SharedIndexInformer) error {
	c := r.controller
	ctx, cancel := context.WithTimeout(parent, c.options.RequestTimeout)
	defer cancel()
	cr, err := c.getCR(ctx, r.namespace)
	if err != nil {
		return err
	}
	if cr.UID != r.uid || cr.Generation != r.generation || cr.Spec.Suspend || cr.DeletionTimestamp != nil {
		return errors.New("namespace configuration changed")
	}
	workloads := []Workload{}
	for _, informer := range list {
		for _, obj := range informer.GetStore().List() {
			w, e := workloadOf(obj)
			if e == nil && w.Enabled() {
				workloads = append(workloads, w)
			}
		}
	}
	sort.Slice(workloads, func(i, j int) bool { return workloads[i].Key() < workloads[j].Key() })
	c.options.Logger.Debug("reconciling namespace", "namespace", r.namespace, "enabled_workloads", len(workloads))
	if len(workloads) > c.options.MaxTargets {
		return r.problem(ctx, cr, "TargetLimitExceeded")
	}
	alive := map[string]bool{}
	for _, w := range workloads {
		alive[w.Key()] = true
	}
	for key := range r.retries {
		if !alive[key] {
			delete(r.retries, key)
		}
	}
	status := api.VaultReloaderStatus{ObservedGeneration: cr.Generation, Conditions: append([]meta.Condition{}, cr.Status.Conditions...), Targets: []api.TargetState{}}
	old := map[string]api.TargetState{}
	for _, t := range cr.Status.Targets {
		old[t.Kind+"/"+t.Name+"/"+t.UID] = t
	}
	sources := []Source{}
	byWorkload := map[string]Source{}
	invalid := map[string]string{}
	ignored := map[string]bool{}
	for _, w := range workloads {
		source, e := r.sourceFor(ctx, w)
		if e != nil {
			invalid[w.Key()] = e.Error()
			continue
		}
		if len(source.Paths) == 0 {
			c.options.Logger.Debug("workload has no supported unversioned Vault references", "namespace", r.namespace, "kind", w.Kind, "name", w.Name)
			ignored[w.Key()] = true
			continue
		}
		sources = append(sources, source)
		byWorkload[w.Key()] = source
	}
	if err = c.poller.ReplaceOwner(r.owner, sources); err != nil {
		return r.problem(ctx, cr, "MetadataCacheLimitExceeded")
	}
	healthy := true
	pending := false
	for _, w := range workloads {
		if ignored[w.Key()] {
			delete(r.retries, w.Key())
			continue
		}
		previous, exists := old[w.Key()]
		if !exists {
			previous = api.TargetState{Kind: w.Kind, Name: w.Name, UID: string(w.UID)}
		}
		if message := invalid[w.Key()]; message != "" {
			previous.Message = message
			previous.Phase = "UnsupportedConfiguration"
			status.Targets = append(status.Targets, previous)
			healthy = false
			continue
		}
		previous.Message = ""
		source := byWorkload[w.Key()]
		config := digest(source)
		observations := c.poller.Snapshot(source)
		versions := map[string]api.Version{}
		ready := true
		pollErrorSeen := false
		last := time.Time{}
		for _, path := range source.Paths {
			o := observations[path]
			if o.Pending || o.Err != "" {
				ready = false
				if o.Err != "" && !pollErrorSeen {
					pollErrorSeen = true
					previous.Phase = o.Err
					if o.Stage != "" || o.HTTPStatus != 0 {
						previous.Message = fmt.Sprintf("stage=%s http_status=%d", o.Stage, o.HTTPStatus)
					}
				} else if !pollErrorSeen {
					previous.Phase = "WaitingForMetadata"
				}
			}
			versions[digest(path)] = o.Version
			if last.IsZero() || o.Success.Before(last) {
				last = o.Success
			}
		}
		if !ready {
			healthy = false
			status.Targets = append(status.Targets, previous)
			continue
		}
		stamp := meta.NewTime(last)
		previous.LastPoll = &stamp
		desired := revision(w, string(cr.UID), source, versions)
		if !exists || previous.Config != config {
			previous.Config = config
			previous.Versions = versions
			previous.Desired = ""
			previous.Requested = ""
			previous.Observed = ""
			previous.RequestedAt = nil
			previous.Phase = "BaselineRecorded"
			// RecordOnly on initial adoption; configuration/source identity change also establishes a new baseline.
		} else if digest(previous.Versions) != digest(versions) {
			previous.Versions = versions
			previous.Desired = desired
			previous.Phase = "Pending"
		}
		if previous.Desired != "" && previous.Observed != previous.Desired {
			pending = true
		}
		status.Targets = append(status.Targets, previous)
	}
	set := func(kind string, ok bool, reason string) {
		value := meta.ConditionFalse
		if ok {
			value = meta.ConditionTrue
		}
		apimeta.SetStatusCondition(&status.Conditions, meta.Condition{Type: kind, Status: value, Reason: reason, Message: reason, ObservedGeneration: cr.Generation, LastTransitionTime: meta.Now()})
	}
	set("AccessReady", true, "CachesSynchronized")
	set("VaultReady", healthy, "MetadataObservation")
	set("Reconciling", pending, "ReconciliationState")
	set("Ready", healthy && !pending, "ReconciliationState")
	var pendingCount, stalledCount int64
	for _, state := range status.Targets {
		if state.Desired != "" && state.Observed != state.Desired {
			pendingCount++
		}
		if state.Phase == "Stalled" {
			stalledCount++
		}
	}
	c.metrics.Pending.Add(pendingCount - r.pendingCount)
	r.pendingCount = pendingCount
	c.metrics.Stalled.Add(stalledCount - r.stalledCount)
	r.stalledCount = stalledCount
	// All pending intent must reach persistent status before any workload patch.
	if err = c.writeStatus(ctx, cr, status); err != nil {
		if errors.Is(err, errStatusTooLarge) {
			return r.problem(ctx, cr, "StateLimitExceeded")
		}
		return err
	}
	for _, state := range status.Targets {
		prior, existed := old[state.Kind+"/"+state.Name+"/"+state.UID]
		attrs := []any{"namespace", r.namespace, "kind", state.Kind, "name", state.Name}
		if state.Phase != prior.Phase || state.Message != prior.Message {
			if state.Phase == "UnsupportedConfiguration" || (state.Phase != "WaitingForMetadata" && !healthyPhase(state.Phase)) {
				c.options.Logger.Warn("workload requires attention", append(attrs, "reason", state.Phase, "detail", state.Message)...)
			} else if existed && prior.Phase != "WaitingForMetadata" && !healthyPhase(prior.Phase) && healthyPhase(state.Phase) {
				c.options.Logger.Info("workload recovered", append(attrs, "previous_reason", prior.Phase, "phase", state.Phase)...)
			} else {
				c.options.Logger.Debug("workload state changed", append(attrs, "phase", state.Phase)...)
			}
		}
		if existed && prior.Config != "" && prior.Config == state.Config {
			for pathID, current := range state.Versions {
				if previous, ok := prior.Versions[pathID]; ok && previous != current {
					c.options.Logger.Info("Vault secret version changed", append(attrs, "path_id", pathID, "previous_version", previous.Version, "version", current.Version)...)
				}
			}
		}
	}
	for _, state := range status.Targets {
		if parent.Err() != nil {
			return parent.Err()
		}
		if state.Desired == "" || invalid[state.Kind+"/"+state.Name+"/"+state.UID] != "" {
			continue
		}
		source, ok := byWorkload[state.Kind+"/"+state.Name+"/"+state.UID]
		if state.Observed == state.Desired {
			unchanged := false
			for _, w := range workloads {
				if w.Key() == state.Kind+"/"+state.Name+"/"+state.UID && w.Template.Annotations[RevisionAnnotation] == state.Desired {
					unchanged = true
					break
				}
			}
			if unchanged {
				continue
			}
		}
		if !ok {
			continue
		}
		key := state.Kind + "/" + state.Name + "/" + state.UID
		if r.retries == nil {
			r.retries = map[string]retryState{}
		}
		retry := r.retries[key]
		if time.Now().Before(retry.due) {
			continue
		}
		if err = r.reconcileTarget(parent, state, source); err != nil {
			retry.failures++
			delay := c.options.RetryMin
			for i := 1; i < retry.failures && delay < c.options.RetryMax; i++ {
				delay *= 2
			}
			if delay > c.options.RetryMax {
				delay = c.options.RetryMax
			}
			retry.due = time.Now().Add(delay)
			r.retries[key] = retry
			c.options.Logger.Warn("workload reconciliation deferred", "namespace", r.namespace, "kind", state.Kind, "name", state.Name)
		} else {
			delete(r.retries, key)
		}
	}
	return nil
}

func healthyPhase(phase string) bool {
	switch phase {
	case "", "BaselineRecorded", "Pending", "Requested", "RolledOut", "RollingOut":
		return true
	default:
		return false
	}
}
func (r *namespaceRuntime) problem(ctx context.Context, cr *api.VaultReloader, reason string) error {
	status := copyStatus(cr.Status)
	apimeta.SetStatusCondition(&status.Conditions, meta.Condition{Type: "Ready", Status: meta.ConditionFalse, Reason: reason, Message: reason, ObservedGeneration: cr.Generation, LastTransitionTime: meta.Now()})
	if err := r.controller.writeStatus(ctx, cr, status); err != nil {
		return err
	}
	return fmt.Errorf("%s", reason)
}

func (r *namespaceRuntime) sourceFor(_ context.Context, w Workload) (Source, error) {
	return parseSource(w, r.controller.options)
}
