// Copyright © 2026 Contributors
// SPDX-License-Identifier: Apache-2.0
package operator

import (
	"context"
	"fmt"
	api "github.com/bank-vaults/vault-secrets-reloader/api/v1alpha1"
	"sync"
	"testing"
	"time"
)

type metadataFake struct {
	mu      sync.Mutex
	calls   map[string]int
	deny    map[string]bool
	version int64
	block   <-chan struct{}
}

func (f *metadataFake) Read(ctx context.Context, s Source, path string) (api.Version, error) {
	f.mu.Lock()
	f.calls[s.Tenant+"/"+path]++
	denied := f.deny[s.Tenant]
	n := f.version
	block := f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return api.Version{}, ctx.Err()
		}
	}
	if denied {
		return api.Version{}, &PollError{Code: "VaultAccessDenied"}
	}
	return api.Version{Version: n, Created: "v", KeyCreated: "k"}, nil
}
func (f *metadataFake) Close() {}
func TestPollerDedupAndSubscriberCancellation(t *testing.T) {
	release := make(chan struct{})
	f := &metadataFake{calls: map[string]int{}, deny: map[string]bool{}, version: 1, block: release}
	o := testOptions()
	p := NewPoller(o, f, &Metrics{})
	s := Source{Tenant: "a", Address: "http://vault.test", Paths: []string{"kv/data/x"}}
	if err := p.ReplaceOwner("a/1", []Source{s, s}); err != nil {
		t.Fatal(err)
	}
	_ = p.ReplaceOwner("a/2", []Source{s})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); p.Run(ctx) }()
	defer func() { cancel(); <-done }()
	eventually(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.calls["a/kv/data/x"] == 1 })
	p.RemoveOwner("a/1")
	close(release)
	eventually(t, func() bool { return p.Snapshot(s)["kv/data/x"].Version.Version == 1 })
	f.mu.Lock()
	calls := f.calls["a/kv/data/x"]
	f.mu.Unlock()
	if calls != 1 {
		t.Fatalf("expected singleflight, got %d", calls)
	}
	p.RemoveOwner("a/2")
	if !p.Snapshot(s)["kv/data/x"].Pending {
		t.Fatal("unused cache entry retained")
	}
}
func TestPollerFailureIsolationAndLastKnownGood(t *testing.T) {
	f := &metadataFake{calls: map[string]int{}, deny: map[string]bool{}, version: 1}
	o := testOptions()
	o.Workers = 3
	o.PerVaultWorkers = 2
	o.PerTenantWorkers = 1
	p := NewPoller(o, f, &Metrics{})
	sources := []Source{}
	for i := 0; i < 10; i++ {
		s := Source{Tenant: fmt.Sprint(i), Address: "http://shared-vault", Role: fmt.Sprint(i), Paths: []string{"kv/data/x"}}
		sources = append(sources, s)
		_ = p.ReplaceOwner(s.Tenant, []Source{s})
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); p.Run(ctx) }()
	defer func() { cancel(); <-done }()
	eventually(t, func() bool {
		for _, s := range sources {
			if p.Snapshot(s)["kv/data/x"].Version.Version != 1 {
				return false
			}
		}
		return true
	})
	f.mu.Lock()
	f.deny["0"] = true
	f.version = 2
	f.mu.Unlock()
	eventually(t, func() bool { return p.Snapshot(sources[0])["kv/data/x"].Err == "VaultAccessDenied" })
	if p.Snapshot(sources[0])["kv/data/x"].Version.Version != 1 {
		t.Fatal("read error discarded last known good")
	}
	eventually(t, func() bool {
		for _, s := range sources[1:] {
			if p.Snapshot(s)["kv/data/x"].Version.Version != 2 {
				return false
			}
		}
		return true
	})
	f.mu.Lock()
	f.deny["0"] = false
	f.mu.Unlock()
	eventually(t, func() bool { o := p.Snapshot(sources[0])["kv/data/x"]; return o.Err == "" && o.Version.Version == 2 })
}
func TestPollerBoundedEntries(t *testing.T) {
	o := testOptions()
	o.MaxEntries = 1
	p := NewPoller(o, &metadataFake{}, &Metrics{})
	s := Source{Tenant: "a", Paths: []string{"kv/data/1", "kv/data/2"}}
	if err := p.ReplaceOwner("a", []Source{s}); err == nil {
		t.Fatal("unbounded cache")
	}
	if len(p.entries) != 0 {
		t.Fatal("partial subscription after failed validation")
	}
}
func TestPollerIgnoresOldEpochResult(t *testing.T) {
	release := make(chan struct{})
	f := &metadataFake{calls: map[string]int{}, deny: map[string]bool{}, version: 1, block: release}
	o := testOptions()
	p := NewPoller(o, f, &Metrics{})
	s := Source{Tenant: "a", Address: "http://vault.test", Paths: []string{"kv/data/x"}}
	_ = p.ReplaceOwner("old", []Source{s})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); p.Run(ctx) }()
	defer func() { cancel(); <-done }()
	eventually(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.calls["a/kv/data/x"] == 1 })
	p.RemoveOwner("old")
	f.mu.Lock()
	f.version = 2
	f.block = nil
	f.mu.Unlock()
	_ = p.ReplaceOwner("new", []Source{s})
	close(release)
	eventually(t, func() bool { return p.Snapshot(s)["kv/data/x"].Version.Version == 2 })
	time.Sleep(20 * time.Millisecond)
	if p.Snapshot(s)["kv/data/x"].Version.Version != 2 {
		t.Fatal("old result overwritten new epoch")
	}
}
