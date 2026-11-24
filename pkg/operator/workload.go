// Copyright © 2026 Contributors
// SPDX-License-Identifier: Apache-2.0
package operator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	api "github.com/bank-vaults/vault-secrets-reloader/api/v1alpha1"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const OptIn = "secrets-reloader.security.bank-vaults.io/reload-on-secret-change"
const VaultRoleAnnotation = "secrets-reloader.security.bank-vaults.io/vault-role"
const RevisionAnnotation = "secrets-reloader.security.bank-vaults.io/revision"
const prefix = "secrets-webhook.security.bank-vaults.io/"
const deprecatedPrefix = "vault.security.banzaicloud.io/"

type Workload struct {
	Kind, Namespace, Name string
	UID                   types.UID
	ResourceVersion       string
	Generation            int64
	Template              core.PodTemplateSpec
	Object                meta.Object
	Raw                   interface{}
}

func workloadOf(obj interface{}) (Workload, error) {
	w := Workload{Raw: obj}
	switch v := obj.(type) {
	case *apps.Deployment:
		w.Kind = "Deployment"
		w.Object = v
		w.Template = *v.Spec.Template.DeepCopy()
	case *apps.StatefulSet:
		w.Kind = "StatefulSet"
		w.Object = v
		w.Template = *v.Spec.Template.DeepCopy()
	case *apps.DaemonSet:
		w.Kind = "DaemonSet"
		w.Object = v
		w.Template = *v.Spec.Template.DeepCopy()
	default:
		return w, errors.New("unsupported workload kind")
	}
	w.Namespace = w.Object.GetNamespace()
	w.Name = w.Object.GetName()
	w.UID = w.Object.GetUID()
	w.ResourceVersion = w.Object.GetResourceVersion()
	w.Generation = w.Object.GetGeneration()
	return w, nil
}
func (w Workload) Key() string { return w.Kind + "/" + w.Name + "/" + string(w.UID) }
func (w Workload) Enabled() bool {
	return (w.Template.Annotations[OptIn] == "true" || w.Object.GetAnnotations()[OptIn] == "true") && w.Object.GetDeletionTimestamp() == nil
}
func annotation(a map[string]string, key, def string) string {
	if v, ok := a[prefix+key]; ok {
		return v
	}
	if v, ok := a[deprecatedPrefix+key]; ok {
		return v
	}
	return def
}

type Source struct {
	Address        string
	VaultNamespace string
	Method         string
	AuthMount      string
	Role           string
	Tenant         string
	CASecret       string
	CAFile         string
	SkipTLSVerify  bool     `json:"SkipTLSVerify,omitempty"`
	Paths          []string // data paths; converted to metadata paths without discovery
}

func digest(v interface{}) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func (s Source) ContextKey() string { s.Paths = nil; return digest(s) }
func parseSource(w Workload, o Options) (Source, error) {
	a := w.Template.Annotations
	s := Source{Tenant: w.Namespace, CAFile: o.CAFile,
		Address: annotation(a, "vault-addr", o.DefaultAddress), VaultNamespace: annotation(a, "vault-namespace", o.DefaultNamespace),
		Method: annotation(a, "vault-auth-method", o.DefaultAuthMethod), AuthMount: annotation(a, "vault-path", o.DefaultAuthMount),
		Role: strings.TrimSpace(a[VaultRoleAnnotation]), CASecret: annotation(a, "vault-tls-secret", "")}
	skip := annotation(a, "vault-skip-verify", "false")
	if skip != "true" && skip != "false" {
		return s, errors.New("vault-skip-verify annotation must be true or false")
	}
	s.SkipTLSVerify = skip == "true"
	if annotation(a, "provider", "vault") != "vault" {
		return s, errors.New("only the Vault provider is supported")
	}
	var err error
	s.Address, err = canonicalAddress(s.Address, o.AllowHTTP)
	if err != nil {
		return s, err
	}
	allowed := false
	for _, origin := range o.AllowedAddresses {
		v, e := canonicalAddress(origin, o.AllowHTTP)
		if e == nil && s.Address == v {
			allowed = true
		}
	}
	if !allowed {
		return s, errors.New("vault origin is not allowlisted")
	}
	if s.Method != "kubernetes" {
		return s, errors.New("only Kubernetes Vault auth is supported in namespaced mode")
	}
	if s.Role == "" {
		return s, errors.New("missing required " + VaultRoleAnnotation + " annotation")
	}
	if !validPath(s.AuthMount) {
		return s, errors.New("invalid Vault auth configuration")
	}
	// A platform-managed CA file takes precedence over the webhook's per-workload Secret.
	if s.CAFile != "" {
		s.CASecret = ""
	}
	if s.CASecret != "" && (!o.AllowCASecrets || !contains(o.AllowedCASecrets, w.Namespace+"/"+s.CASecret)) {
		return s, errors.New("CA Secret is not approved for reading")
	}
	set := map[string]bool{}
	add := func(ref string) error {
		if strings.Contains(ref, "${vault:") {
			return errors.New("inline Vault expressions are not supported")
		}
		if strings.HasPrefix(ref, ">>vault:") {
			return errors.New("vault write references are not supported")
		}
		if !strings.HasPrefix(ref, "vault:") {
			return nil
		}
		parts := strings.Split(strings.TrimPrefix(ref, "vault:"), "#")
		if len(parts) < 2 || parts[1] == "" {
			return errors.New("invalid Vault reference")
		}
		if len(parts) > 2 {
			return nil
		} // pinned version: never follow latest
		if strings.Contains(parts[0], "${") {
			return errors.New("dynamic Vault paths are not supported")
		}
		if _, e := metadataPath(parts[0]); e != nil {
			return e
		}
		set[parts[0]] = true
		return nil
	}
	containers := append(append([]core.Container{}, w.Template.Spec.Containers...), w.Template.Spec.InitContainers...)
	for _, c := range containers {
		for _, env := range c.Env {
			if env.ValueFrom != nil {
				continue
			}
			if e := add(env.Value); e != nil {
				return s, e
			}
		}
	}
	refs := a[prefix+"vault-from-path"]
	if refs == "" {
		refs = a[deprecatedPrefix+"vault-env-from-path"]
	}
	if refs != "" {
		for _, ref := range strings.Split(refs, ",") {
			parts := strings.Split(strings.TrimSpace(ref), "#")
			if len(parts) > 1 {
				continue
			}
			if _, e := metadataPath(parts[0]); e != nil {
				return s, e
			}
			set[parts[0]] = true
		}
	}
	for p := range set {
		s.Paths = append(s.Paths, p)
	}
	sort.Strings(s.Paths)
	if len(s.Paths) > o.MaxPaths {
		return s, errors.New("workload exceeds path limit")
	}
	return s, nil
}
func validPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "?#%\\") {
			return false
		}
	}
	return true
}
func metadataPath(data string) (string, error) {
	if !validPath(data) {
		return "", errors.New("invalid KV v2 data path")
	}
	parts := strings.Split(data, "/")
	boundary := -1
	for i, p := range parts {
		if p == "data" {
			if boundary != -1 {
				return "", errors.New("ambiguous data segment: cannot determine KV v2 mount")
			}
			boundary = i
		}
	}
	if boundary < 1 || boundary == len(parts)-1 {
		return "", errors.New("explicit KV v2 mount/data/path reference required")
	}
	parts[boundary] = "metadata"
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/"), nil
}
func revision(w Workload, crUID string, source Source, versions map[string]api.Version) string {
	return digest(struct {
		CR, Workload, Config string
		Versions             map[string]api.Version
	}{crUID, string(w.UID), digest(source), versions})
}
func rolledOut(w Workload) (bool, string) {
	switch v := w.Raw.(type) {
	case *apps.Deployment:
		if v.Spec.Paused {
			return false, "Paused"
		}
		n := int32(1)
		if v.Spec.Replicas != nil {
			n = *v.Spec.Replicas
		}
		return v.Status.ObservedGeneration >= v.Generation && v.Status.UpdatedReplicas == n && v.Status.Replicas == n && v.Status.AvailableReplicas == n, "RollingOut"
	case *apps.StatefulSet:
		if v.Spec.UpdateStrategy.Type == apps.OnDeleteStatefulSetStrategyType {
			return false, "OnDelete"
		}
		if v.Spec.UpdateStrategy.RollingUpdate != nil && v.Spec.UpdateStrategy.RollingUpdate.Partition != nil && *v.Spec.UpdateStrategy.RollingUpdate.Partition > 0 {
			return false, "Partitioned"
		}
		n := int32(1)
		if v.Spec.Replicas != nil {
			n = *v.Spec.Replicas
		}
		return v.Status.ObservedGeneration >= v.Generation && v.Status.UpdatedReplicas == n && v.Status.ReadyReplicas == n && v.Status.CurrentRevision == v.Status.UpdateRevision, "RollingOut"
	case *apps.DaemonSet:
		if v.Spec.UpdateStrategy.Type == apps.OnDeleteDaemonSetStrategyType {
			return false, "OnDelete"
		}
		return v.Status.ObservedGeneration >= v.Generation && v.Status.UpdatedNumberScheduled == v.Status.DesiredNumberScheduled && v.Status.NumberReady == v.Status.DesiredNumberScheduled && v.Status.NumberUnavailable == 0, "RollingOut"
	}
	return false, fmt.Sprintf("Unsupported:%s", w.Kind)
}
