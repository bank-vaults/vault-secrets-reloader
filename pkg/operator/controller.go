// Copyright © 2026 Contributors
// SPDX-License-Identifier: Apache-2.0
package operator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	api "github.com/bank-vaults/vault-secrets-reloader/api/v1alpha1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

var errStatusTooLarge = errors.New("status size limit exceeded")

type Controller struct {
	options Options
	kube    kubernetes.Interface
	dynamic dynamic.Interface
	poller  *Poller
	metrics *Metrics
	ready   atomic.Bool
}

func NewController(o Options, k kubernetes.Interface, d dynamic.Interface, reader MetadataReader) (*Controller, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	metrics := &Metrics{}
	if reader == nil {
		reader = NewVaultReader(o, k)
	}
	return &Controller{options: o, kube: k, dynamic: d, poller: NewPoller(o, reader, metrics), metrics: metrics}, nil
}
func (c *Controller) Ready() bool       { return c.ready.Load() }
func (c *Controller) Metrics() *Metrics { return c.metrics }
func (c *Controller) Run(ctx context.Context) error {
	if !c.options.LeaderElection {
		return c.run(ctx)
	}
	ctxElection, cancelElection := context.WithCancel(ctx)
	defer cancelElection()
	baseLock := &resourcelock.LeaseLock{LeaseMeta: meta.ObjectMeta{Namespace: c.options.LeaderNamespace, Name: c.options.LeaderName}, Client: c.kube.CoordinationV1(), LockConfig: resourcelock.ResourceLockConfig{Identity: c.options.Identity}}
	lock := &trackedLock{Interface: baseLock, identity: c.options.Identity}
	result := make(chan error, 1)
	elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock: lock, LeaseDuration: 15 * time.Second, RenewDeadline: 10 * time.Second, RetryPeriod: 2 * time.Second,
		ReleaseOnCancel: false,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(leaderCtx context.Context) { result <- c.run(leaderCtx); cancelElection() },
			OnStoppedLeading: func() { c.ready.Store(false) },
		},
	})
	if err != nil {
		return err
	}
	elector.Run(ctxElection)
	if lock.acquired.Load() {
		err = <-result
		if err != nil {
			return err
		}
		if ctx.Err() == nil {
			return errors.New("leadership lost; restart required")
		}
	}
	return nil
}

type trackedLock struct {
	resourcelock.Interface
	identity string
	acquired atomic.Bool
}

func (l *trackedLock) Create(ctx context.Context, record resourcelock.LeaderElectionRecord) error {
	err := l.Interface.Create(ctx, record)
	if err == nil && record.HolderIdentity == l.identity {
		l.acquired.Store(true)
	}
	return err
}
func (l *trackedLock) Update(ctx context.Context, record resourcelock.LeaderElectionRecord) error {
	err := l.Interface.Update(ctx, record)
	if err == nil && record.HolderIdentity == l.identity {
		l.acquired.Store(true)
	}
	return err
}

func (c *Controller) run(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	defer c.ready.Store(false)
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(c.dynamic, 0, meta.NamespaceAll, nil)
	informer := factory.ForResource(api.Resource).Informer()
	changed := make(chan struct{}, 1)
	notify := func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	}
	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(interface{}) { notify() }, DeleteFunc: func(interface{}) { notify() },
		UpdateFunc: func(old, new interface{}) {
			a, aok := old.(*unstructured.Unstructured)
			b, bok := new.(*unstructured.Unstructured)
			if !aok || !bok || a.GetUID() != b.GetUID() || a.GetGeneration() != b.GetGeneration() || !reflect.DeepEqual(a.GetDeletionTimestamp(), b.GetDeletionTimestamp()) {
				notify()
			}
		},
	})
	if err != nil {
		return err
	}
	factory.Start(ctx.Done())
	defer func() { cancel(); factory.Shutdown() }()
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		return ctx.Err()
	}
	c.ready.Store(true)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); c.poller.Run(ctx) }()
	type statusTask struct {
		cr          *api.VaultReloader
		reason, key string
		signature   string
	}
	statusTasks := make(chan statusTask, c.options.MaxNamespaces)
	var queued sync.Map
	queueStatus := func(cr *api.VaultReloader, reason string) {
		key := cr.Namespace
		signature := fmt.Sprintf("%s/%d/%s", cr.UID, cr.Generation, reason)
		if previous, already := queued.Load(key); already && previous == signature {
			return
		}
		queued.Store(key, signature)
		select {
		case statusTasks <- statusTask{cr: cr, reason: reason, key: key, signature: signature}:
		default:
			queued.CompareAndDelete(key, signature)
		}
	}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case task := <-statusTasks:
					requestCtx, stop := context.WithTimeout(ctx, c.options.RequestTimeout)
					fresh, e := c.getCR(requestCtx, task.cr.Namespace)
					if e == nil && fresh.UID == task.cr.UID && fresh.Generation == task.cr.Generation {
						status := copyStatus(fresh.Status)
						status.ObservedGeneration = fresh.Generation
						for _, kind := range []string{"Ready", "Reconciling"} {
							apimeta.SetStatusCondition(&status.Conditions, meta.Condition{Type: kind, Status: meta.ConditionFalse, Reason: task.reason, Message: task.reason, ObservedGeneration: fresh.Generation, LastTransitionTime: meta.Now()})
						}
						e = c.writeStatus(requestCtx, fresh, status)
					}
					stop()
					if e != nil {
						queued.CompareAndDelete(task.key, task.signature)
					}
				}
			}
		}()
	}
	running := map[string]*namespaceRuntime{}
	defer func() {
		for _, r := range running {
			r.cancel()
		}
		cancel()
		wg.Wait()
	}()
	reconcile := func() {
		desired := map[string]*api.VaultReloader{}
		suspended := []*api.VaultReloader{}
		for _, obj := range informer.GetStore().List() {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok || u.GetName() != "default" || u.GetNamespace() == "" || u.GetDeletionTimestamp() != nil {
				continue
			}
			cr, e := decodeCR(u)
			if e != nil {
				continue
			}
			if !cr.Spec.Suspend {
				desired[cr.Namespace] = cr
			} else {
				suspended = append(suspended, cr)
			}
		}
		queued.Range(func(key, value interface{}) bool {
			ns := key.(string)
			present := desired[ns] != nil
			if !present {
				for _, cr := range suspended {
					if cr.Namespace == ns {
						present = true
						break
					}
				}
			}
			if !present {
				queued.CompareAndDelete(key, value)
			}
			return true
		})
		for ns, r := range running {
			cr := desired[ns]
			if cr == nil || cr.UID != r.uid || cr.Generation != r.generation {
				r.cancel()
				<-r.done
				delete(running, ns)
			}
		}
		for _, cr := range suspended {
			queueStatus(cr, "Suspended")
		}

		for ns, cr := range desired {
			if running[ns] != nil {
				continue
			}
			if len(running) >= c.options.MaxNamespaces {
				c.options.Logger.Warn("namespace runtime limit reached")
				queueStatus(cr, "NamespaceLimitExceeded")
				continue
			}
			rctx, rcancel := context.WithCancel(ctx)
			r := &namespaceRuntime{controller: c, namespace: ns, uid: cr.UID, generation: cr.Generation, cancel: rcancel, done: make(chan struct{}), owner: fmt.Sprintf("%s/%s", ns, cr.UID)}
			running[ns] = r
			wg.Add(1)
			go func() { defer wg.Done(); defer close(r.done); r.run(rctx) }()
		}
	}
	reconcile()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-changed:
			reconcile()
		case <-ticker.C:
			reconcile()
		}
	}
}
func decodeCR(u *unstructured.Unstructured) (*api.VaultReloader, error) {
	cr := new(api.VaultReloader)
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, cr)
	return cr, err
}
func (c *Controller) getCR(ctx context.Context, ns string) (*api.VaultReloader, error) {
	u, err := c.dynamic.Resource(api.Resource).Namespace(ns).Get(ctx, "default", meta.GetOptions{})
	if err != nil {
		return nil, err
	}
	return decodeCR(u)
}
func (c *Controller) writeStatus(ctx context.Context, cr *api.VaultReloader, status api.VaultReloaderStatus) error {
	if reflect.DeepEqual(cr.Status, status) {
		return nil
	}
	body, err := json.Marshal([]map[string]interface{}{
		{"op": "test", "path": "/metadata/uid", "value": string(cr.UID)},
		{"op": "test", "path": "/metadata/resourceVersion", "value": cr.ResourceVersion},
		{"op": "add", "path": "/status", "value": status},
	})
	if err != nil {
		return err
	}
	if len(body) > c.options.MaxStateBytes {
		return errStatusTooLarge
	}
	_, err = c.dynamic.Resource(api.Resource).Namespace(cr.Namespace).Patch(ctx, cr.Name, types.JSONPatchType, body, meta.PatchOptions{}, "status")
	return err // conflicts must be retried by the next reconcile after a direct GET
}

func copyStatus(status api.VaultReloaderStatus) api.VaultReloaderStatus {
	b, _ := json.Marshal(status)
	var result api.VaultReloaderStatus
	_ = json.Unmarshal(b, &result)
	return result
}
