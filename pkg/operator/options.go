// Copyright © 2026 Contributors
// SPDX-License-Identifier: Apache-2.0

// Package operator implements the experimental namespace opt-in controller.
package operator

import (
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

type Options struct {
	PollInterval      time.Duration
	ReconcileInterval time.Duration
	AccessInterval    time.Duration
	SyncTimeout       time.Duration
	RequestTimeout    time.Duration
	RetryMin          time.Duration
	RetryMax          time.Duration
	RolloutTimeout    time.Duration
	MaxNamespaces     int
	MaxTargets        int
	MaxPaths          int
	MaxStateBytes     int
	MaxEntries        int
	Workers           int
	PerVaultWorkers   int
	PerTenantWorkers  int
	VaultQPS          float64
	AllowedAddresses  []string
	AllowHTTP         bool
	JWTFile           string
	AllowCASecrets    bool
	AllowedCASecrets  []string // namespace/name, no wildcards
	CAFile            string
	DefaultAddress    string
	DefaultNamespace  string
	DefaultAuthMount  string
	DefaultAuthMethod string
	LeaderElection    bool
	LeaderNamespace   string
	LeaderName        string
	Identity          string
	Logger            *slog.Logger
}

func DefaultOptions() Options {
	return Options{
		PollInterval: time.Minute, ReconcileInterval: time.Minute, AccessInterval: time.Minute, SyncTimeout: 30 * time.Second,
		RequestTimeout: 10 * time.Second, RetryMin: time.Second, RetryMax: time.Minute,
		RolloutTimeout: 10 * time.Minute, MaxNamespaces: 128, MaxTargets: 200, MaxPaths: 100,
		MaxStateBytes: 512 * 1024, MaxEntries: 10000, Workers: 8, PerVaultWorkers: 4,
		PerTenantWorkers: 2, VaultQPS: 10,
		JWTFile:        "/var/run/secrets/vault/token",
		DefaultAddress: "https://vault:8200", DefaultAuthMount: "kubernetes", DefaultAuthMethod: "kubernetes",
		LeaderElection: true, LeaderName: "vault-secrets-reloader", Logger: slog.Default(),
	}
}
func (o Options) Validate() error {
	if o.PollInterval <= 0 || o.ReconcileInterval <= 0 || o.AccessInterval <= 0 || o.SyncTimeout <= 0 || o.RequestTimeout <= 0 || o.RetryMin <= 0 || o.RetryMax < o.RetryMin || o.RolloutTimeout <= 0 {
		return errors.New("intervals/timeouts must be positive and retryMax >= retryMin")
	}
	if o.MaxNamespaces < 1 || o.MaxTargets < 1 || o.MaxPaths < 1 || o.MaxEntries < 1 || o.MaxStateBytes < 1024 || o.Workers < 1 || o.PerVaultWorkers < 1 || o.PerTenantWorkers < 1 || o.VaultQPS <= 0 {
		return errors.New("limits must be positive")
	}
	if o.MaxTargets > 1000 || o.MaxPaths > 1000 {
		return errors.New("target/path limits must not exceed CRD schema limits (1000)")
	}
	if o.JWTFile == "" {
		return errors.New("controller JWT file is required")
	}
	if o.LeaderElection && (o.LeaderNamespace == "" || o.Identity == "") {
		return errors.New("leader namespace and unique identity are required")
	}
	if len(o.AllowedAddresses) == 0 {
		return errors.New("explicit Vault address allowlist is required in namespaced mode")
	}
	for _, a := range o.AllowedAddresses {
		if _, err := canonicalAddress(a, o.AllowHTTP); err != nil {
			return err
		}
	}
	return nil
}
func canonicalAddress(a string, allowHTTP bool) (string, error) {
	u, err := url.Parse(a)
	if err != nil {
		return "", errors.New("invalid Vault origin")
	}
	validScheme := u.Scheme == "https" || allowHTTP && u.Scheme == "http"
	validPath := u.Path == "" || u.Path == "/"
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !validPath || !validScheme {
		return "", errors.New("vault address must be an allowed HTTPS origin without credentials, query or path")
	}
	u.Host = strings.ToLower(u.Host)
	u.Path = ""
	return u.String(), nil
}
func contains(items []string, s string) bool {
	for _, v := range items {
		if v == s {
			return true
		}
	}
	return false
}

type Metrics struct {
	Namespaces         atomic.Int64
	Polls              atomic.Uint64
	PollErrors         atomic.Uint64
	Deduplicated       atomic.Uint64
	Patches            atomic.Uint64
	PatchErrors        atomic.Uint64
	AccessDenied       atomic.Uint64
	Pending            atomic.Int64
	Stalled            atomic.Int64
	LastSuccessfulPoll atomic.Int64
}
