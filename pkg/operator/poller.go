// Copyright © 2026 Contributors
// SPDX-License-Identifier: Apache-2.0
package operator

import (
	"context"
	"errors"
	"math/rand/v2"
	"sort"
	"sync"
	"time"

	api "github.com/bank-vaults/vault-secrets-reloader/api/v1alpha1"
)

type Observation struct {
	Version    api.Version
	Success    time.Time
	Err        string
	Stage      string
	HTTPStatus int
	Pending    bool
}
type pollEntry struct {
	key         string
	source      Source
	path        string
	owners      map[string]bool
	observation Observation
	due         time.Time
	failures    int
	running     bool
	cancel      context.CancelFunc
}
type Poller struct {
	mu             sync.Mutex
	options        Options
	reader         MetadataReader
	entries        map[string]*pollEntry
	endpointActive map[string]int
	tenantActive   map[string]int
	endpointNext   map[string]time.Time
	active         int
	cursor         int
	metrics        *Metrics
	wg             sync.WaitGroup
}

func NewPoller(o Options, r MetadataReader, m *Metrics) *Poller {
	return &Poller{options: o, reader: r, metrics: m, entries: map[string]*pollEntry{}, endpointActive: map[string]int{}, tenantActive: map[string]int{}, endpointNext: map[string]time.Time{}}
}
func pollKey(s Source, path string) string { return s.ContextKey() + "/" + path }

// ReplaceOwner replaces all subscriptions of one namespace runtime epoch atomically.
func (p *Poller) ReplaceOwner(owner string, sources []Source) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	desired := map[string]struct {
		s    Source
		path string
	}{}
	for _, s := range sources {
		for _, path := range s.Paths {
			desired[pollKey(s, path)] = struct {
				s    Source
				path string
			}{s, path}
		}
	}
	removedContexts := map[string]bool{}
	additions := 0
	for key := range desired {
		if p.entries[key] == nil {
			additions++
		}
	}
	if len(p.entries)+additions > p.options.MaxEntries {
		return errors.New("metadata cache entry limit exceeded")
	}
	for key, e := range p.entries {
		if e.owners[owner] {
			if _, ok := desired[key]; !ok {
				delete(e.owners, owner)
				if len(e.owners) == 0 {
					if e.cancel != nil {
						e.cancel()
					}
					delete(p.entries, key)
					removedContexts[e.source.ContextKey()] = true
				}
			}
		}
	}
	for key, d := range desired {
		e := p.entries[key]
		if e == nil {
			e = &pollEntry{key: key, source: d.s, path: d.path, owners: map[string]bool{}}
			p.entries[key] = e
		} else if !e.owners[owner] {
			p.metrics.Deduplicated.Add(1)
		}
		e.owners[owner] = true
	}
	p.releaseUnusedLocked(removedContexts)
	return nil
}
func (p *Poller) RemoveOwner(owner string) {
	removedContexts := map[string]bool{}
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, e := range p.entries {
		delete(e.owners, owner)
		if len(e.owners) == 0 {
			if e.cancel != nil {
				e.cancel()
			}
			delete(p.entries, key)
			removedContexts[e.source.ContextKey()] = true
		}
	}
	p.releaseUnusedLocked(removedContexts)
}
func (p *Poller) Snapshot(s Source) map[string]Observation {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]Observation{}
	for _, path := range s.Paths {
		e := p.entries[pollKey(s, path)]
		if e == nil {
			out[path] = Observation{Pending: true}
			continue
		}
		o := e.observation
		if o.Success.IsZero() {
			o.Pending = true
		}
		if o.Err == "" && time.Since(o.Success) > 2*p.options.PollInterval+p.options.RequestTimeout && !o.Success.IsZero() {
			o.Err = "MetadataStale"
		}
		out[path] = o
	}
	return out
}
func (p *Poller) Run(ctx context.Context) {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	defer func() {
		p.mu.Lock()
		for _, e := range p.entries {
			if e.cancel != nil {
				e.cancel()
			}
		}
		p.mu.Unlock()
		p.wg.Wait()
		p.reader.Close()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			p.dispatch(ctx, now)
		}
	}
}
func (p *Poller) dispatch(ctx context.Context, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	tenants := map[string][]*pollEntry{}
	for _, e := range p.entries {
		tenants[e.source.Tenant] = append(tenants[e.source.Tenant], e)
	}
	names := make([]string, 0, len(tenants))
	for name := range tenants {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return
	}
	start := p.cursor % len(names)
	for n := 0; n < len(names) && p.active < p.options.Workers; n++ {
		tenant := names[(start+n)%len(names)]
		entries := tenants[tenant]
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].due.Equal(entries[j].due) {
				return entries[i].key < entries[j].key
			}
			return entries[i].due.Before(entries[j].due)
		})
		for _, e := range entries {
			if e.running || e.due.After(now) {
				continue
			}
			if p.active >= p.options.Workers || p.tenantActive[tenant] >= p.options.PerTenantWorkers {
				break
			}
			endpoint := e.source.Address
			if p.endpointActive[endpoint] >= p.options.PerVaultWorkers || p.endpointNext[endpoint].After(now) {
				continue
			}
			p.endpointNext[endpoint] = now.Add(time.Duration(float64(time.Second) / p.options.VaultQPS))
			p.active++
			p.tenantActive[tenant]++
			p.endpointActive[endpoint]++
			e.running = true
			reqctx, cancel := context.WithTimeout(ctx, p.options.RequestTimeout)
			e.cancel = cancel
			p.wg.Add(1)
			go p.execute(reqctx, e, cancel)
		}
	}
	p.cursor = (start + 1) % len(names)
}
func (p *Poller) execute(ctx context.Context, e *pollEntry, cancel context.CancelFunc) {
	defer p.wg.Done()
	defer cancel()
	p.metrics.Polls.Add(1)
	version, err := p.reader.Read(ctx, e.source, e.path)
	now := time.Now()
	attrs := []any{"namespace", e.source.Tenant, "source_id", e.source.ContextKey()[:12], "path_id", digest(e.path)[:12]}
	if err != nil {
		var pe *PollError
		if errors.As(err, &pe) {
			attrs = append(attrs, "reason", pe.Code, "stage", pe.Stage, "http_status", pe.HTTPStatus)
		} else {
			attrs = append(attrs, "reason", "VaultPollFailed")
		}
		p.options.Logger.Debug("Vault metadata poll failed", attrs...)
	} else {
		p.options.Logger.Debug("Vault metadata poll succeeded", append(attrs, "version", version.Version)...)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.active--
	p.tenantActive[e.source.Tenant]--
	if p.tenantActive[e.source.Tenant] == 0 {
		delete(p.tenantActive, e.source.Tenant)
	}
	p.endpointActive[e.source.Address]--
	if p.endpointActive[e.source.Address] == 0 {
		delete(p.endpointActive, e.source.Address)
	}
	if p.entries[e.key] != e {
		return
	}
	e.running = false
	e.cancel = nil
	if err != nil {
		p.metrics.PollErrors.Add(1)
		e.failures++
		delay := p.options.RetryMin
		for i := 1; i < e.failures && delay < p.options.RetryMax; i++ {
			delay *= 2
		}
		if delay > p.options.RetryMax {
			delay = p.options.RetryMax
		}
		e.observation.Err = "VaultPollFailed"
		e.observation.Stage = ""
		e.observation.HTTPStatus = 0
		var pe *PollError
		if errors.As(err, &pe) {
			e.observation.Err = pe.Code
			e.observation.Stage = pe.Stage
			e.observation.HTTPStatus = pe.HTTPStatus
			if pe.RetryAfter > delay {
				delay = pe.RetryAfter
			}
		}
		e.due = now.Add(delay + time.Duration(rand.Float64()*float64(delay)/5))
		return
	}
	e.failures = 0
	p.metrics.LastSuccessfulPoll.Store(now.Unix())
	e.observation = Observation{Version: version, Success: now}
	e.due = now.Add(p.options.PollInterval + time.Duration(rand.Float64()*float64(p.options.PollInterval)/10))
}

func (p *Poller) releaseUnusedLocked(candidates map[string]bool) {
	releaser, ok := p.reader.(interface{ Forget(string) })
	if !ok || len(candidates) == 0 {
		return
	}
	for _, entry := range p.entries {
		delete(candidates, entry.source.ContextKey())
	}
	for key := range candidates {
		releaser.Forget(key)
	}
}
