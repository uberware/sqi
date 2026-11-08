// SPDX-License-Identifier: AGPL-3.0-or-later

package preempt

import "net/http"

// Client returns the HTTP client the watcher polls with.
func (w *Watcher) Client() *http.Client { return w.client }
