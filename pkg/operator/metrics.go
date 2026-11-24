// Copyright © 2026 Contributors
// SPDX-License-Identifier: Apache-2.0
package operator

import (
	"fmt"
	"net/http"
	"time"
)

// ServeMetrics exposes aggregate counters without paths, identities or workload labels.
func (c *Controller) ServeMetrics(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	m := c.metrics
	_, _ = fmt.Fprintf(w, "vault_reloader_namespaces %d\nvault_reloader_polls_total %d\nvault_reloader_poll_errors_total %d\nvault_reloader_deduplicated_total %d\nvault_reloader_patches_total %d\nvault_reloader_patch_errors_total %d\nvault_reloader_access_denied_total %d\n", m.Namespaces.Load(), m.Polls.Load(), m.PollErrors.Load(), m.Deduplicated.Load(), m.Patches.Load(), m.PatchErrors.Load(), m.AccessDenied.Load())
	_, _ = fmt.Fprintf(w, "vault_reloader_pending_targets %d\nvault_reloader_stalled_targets %d\nvault_reloader_last_successful_poll_timestamp_seconds %d\n", m.Pending.Load(), m.Stalled.Load(), m.LastSuccessfulPoll.Load())
	c.poller.mu.Lock()
	defer c.poller.mu.Unlock()
	queued := 0
	for _, entry := range c.poller.entries {
		if !entry.running && !entry.due.After(time.Now()) {
			queued++
		}
	}
	_, _ = fmt.Fprintf(w, "vault_reloader_queue_depth %d\n", queued)
	_, _ = fmt.Fprintf(w, "vault_reloader_cache_entries %d\nvault_reloader_active_polls %d\n", len(c.poller.entries), c.poller.active)
}
