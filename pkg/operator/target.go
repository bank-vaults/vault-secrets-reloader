// Copyright © 2026 Contributors
// SPDX-License-Identifier: Apache-2.0
package operator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	api "github.com/bank-vaults/vault-secrets-reloader/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

func getWorkload(ctx context.Context, k kubernetes.Interface, ns, kind, name string) (Workload, error) {
	var obj interface{}
	var err error
	switch kind {
	case "Deployment":
		obj, err = k.AppsV1().Deployments(ns).Get(ctx, name, meta.GetOptions{})
	case "StatefulSet":
		obj, err = k.AppsV1().StatefulSets(ns).Get(ctx, name, meta.GetOptions{})
	case "DaemonSet":
		obj, err = k.AppsV1().DaemonSets(ns).Get(ctx, name, meta.GetOptions{})
	default:
		return Workload{}, errors.New("unsupported kind")
	}
	if err != nil {
		return Workload{}, err
	}
	return workloadOf(obj)
}
func patchRevision(ctx context.Context, k kubernetes.Interface, w Workload, revision string) error {
	if w.Namespace == "" || w.UID == "" || w.ResourceVersion == "" {
		return errors.New("missing workload precondition")
	}
	escaped := strings.ReplaceAll(strings.ReplaceAll(RevisionAnnotation, "~", "~0"), "/", "~1")
	operations := []map[string]interface{}{
		{"op": "test", "path": "/metadata/uid", "value": string(w.UID)},
		{"op": "test", "path": "/metadata/resourceVersion", "value": w.ResourceVersion},
	}
	if w.Template.Annotations == nil {
		operations = append(operations, map[string]interface{}{"op": "add", "path": "/spec/template/metadata/annotations", "value": map[string]string{}})
	}
	operations = append(operations, map[string]interface{}{"op": "add", "path": "/spec/template/metadata/annotations/" + escaped, "value": revision})
	data, err := json.Marshal(operations)
	if err != nil {
		return err
	}
	switch w.Kind {
	case "Deployment":
		_, err = k.AppsV1().Deployments(w.Namespace).Patch(ctx, w.Name, types.JSONPatchType, data, meta.PatchOptions{})
	case "StatefulSet":
		_, err = k.AppsV1().StatefulSets(w.Namespace).Patch(ctx, w.Name, types.JSONPatchType, data, meta.PatchOptions{})
	case "DaemonSet":
		_, err = k.AppsV1().DaemonSets(w.Namespace).Patch(ctx, w.Name, types.JSONPatchType, data, meta.PatchOptions{})
	default:
		err = errors.New("unsupported kind")
	}
	return err
}
func (r *namespaceRuntime) reconcileTarget(parent context.Context, persisted api.TargetState, expected Source) error {
	c := r.controller
	ctx, cancel := context.WithTimeout(parent, c.options.RequestTimeout)
	defer cancel()
	cr, err := c.getCR(ctx, r.namespace)
	if err != nil {
		return err
	}
	if cr.UID != r.uid || cr.Generation != r.generation || cr.Spec.Suspend || cr.DeletionTimestamp != nil {
		return errors.New("namespace no longer eligible")
	}
	index := -1
	for i, state := range cr.Status.Targets {
		if state.UID == persisted.UID && state.Kind == persisted.Kind && state.Name == persisted.Name && state.Desired == persisted.Desired && state.Config == persisted.Config {
			index = i
			break
		}
	}
	if index < 0 {
		return errors.New("persisted intent changed")
	}
	state := cr.Status.Targets[index]
	for _, o := range c.poller.Snapshot(expected) {
		if o.Pending || o.Err != "" {
			return errors.New("metadata not currently usable")
		}
	}
	w, err := getWorkload(ctx, c.kube, r.namespace, state.Kind, state.Name)
	if err != nil {
		return err
	}
	if string(w.UID) != state.UID || !w.Enabled() {
		return errors.New("workload no longer eligible")
	}
	fresh, err := r.sourceFor(ctx, w)
	if err != nil || digest(fresh) != state.Config {
		return errors.New("workload configuration changed")
	}
	if w.Template.Annotations[RevisionAnnotation] != state.Desired {
		c.options.Logger.Debug("patching workload revision", "namespace", r.namespace, "kind", state.Kind, "name", state.Name)
		if err = patchRevision(ctx, c.kube, w, state.Desired); err != nil {
			c.options.Logger.Warn("workload patch failed", "namespace", r.namespace, "kind", state.Kind, "name", state.Name, "reason", apierrors.ReasonForError(err))
			c.metrics.PatchErrors.Add(1)
			failed := copyStatus(cr.Status)
			failed.Targets[index].Phase = "UpdateFailed"
			_ = c.writeStatus(ctx, cr, failed)
			return errors.New("workload patch failed")
		}
		c.metrics.Patches.Add(1)
		c.options.Logger.Info("workload rollout requested", "namespace", r.namespace, "kind", state.Kind, "name", state.Name)
		// Status follows the patch. If it fails, the marker allows recovery without a second patch.
		state.Requested = state.Desired
		now := meta.Now()
		state.RequestedAt = &now
		state.Phase = "Requested"
	} else {
		if state.Requested != state.Desired || state.RequestedAt == nil {
			now := meta.Now()
			state.RequestedAt = &now
		}
		state.Requested = state.Desired
		complete, phase := rolledOut(w)
		if complete {
			state.Observed = state.Desired
			state.Phase = "RolledOut"
		} else {
			state.Phase = phase
			if state.RequestedAt != nil && time.Since(state.RequestedAt.Time) > c.options.RolloutTimeout {
				state.Phase = "Stalled"
			}
		}
	}
	status := cr.Status
	status.Targets = append([]api.TargetState{}, cr.Status.Targets...)
	status.Targets[index] = state
	if err = c.writeStatus(ctx, cr, status); err != nil {
		return fmt.Errorf("persisting rollout state: %w", err)
	}
	if state.Phase == "RolledOut" && persisted.Observed != state.Observed {
		c.options.Logger.Info("workload rollout completed", "namespace", r.namespace, "kind", state.Kind, "name", state.Name)
	}
	return nil
}
