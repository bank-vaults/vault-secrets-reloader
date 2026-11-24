// Copyright © 2026 Contributors
// SPDX-License-Identifier: Apache-2.0

// Package v1alpha1 defines the experimental namespace opt-in API.
package v1alpha1

import (
	"encoding/json"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const Group = "secrets-reloader.security.bank-vaults.io"

var GroupVersion = schema.GroupVersion{Group: Group, Version: "v1alpha1"}
var Resource = GroupVersion.WithResource("vaultreloaders")

type VaultReloaderSpec struct {
	Suspend bool `json:"suspend,omitempty"`
}
type Version struct {
	Version    int64  `json:"version"`
	Created    string `json:"created,omitempty"`
	KeyCreated string `json:"keyCreated,omitempty"`
}
type TargetState struct {
	Kind        string             `json:"kind"`
	Name        string             `json:"name"`
	UID         string             `json:"uid"`
	Config      string             `json:"config"`
	Versions    map[string]Version `json:"versions,omitempty"`
	Desired     string             `json:"desiredRevision,omitempty"`
	Requested   string             `json:"requestedRevision,omitempty"`
	Observed    string             `json:"observedRevision,omitempty"`
	Phase       string             `json:"phase"`
	LastPoll    *metav1.Time       `json:"lastSuccessfulPoll,omitempty"`
	RequestedAt *metav1.Time       `json:"requestedAt,omitempty"`
	Message     string             `json:"message,omitempty"`
}
type VaultReloaderStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
	Targets            []TargetState      `json:"targets,omitempty"`
}
type VaultReloader struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              VaultReloaderSpec   `json:"spec,omitempty"`
	Status            VaultReloaderStatus `json:"status,omitempty"`
}

func (r *VaultReloader) DeepCopyObject() runtime.Object {
	if r == nil {
		return (*VaultReloader)(nil)
	}
	b, _ := json.Marshal(r)
	out := new(VaultReloader)
	_ = json.Unmarshal(b, out)
	return out
}
