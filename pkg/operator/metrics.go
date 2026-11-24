// Copyright © 2026 Contributors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

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
