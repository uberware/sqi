// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"errors"
	"time"
)

// PublishIngestMarkers publishes an empty message on [SubjectMarkerWorker] and
// on [SubjectMarkerTask], and returns once the broker has stored both or
// their timeouts have passed. A consumer that handles one has handled every
// message stored in its stream before it. Each publish waits at most timeout
// on its own, so a stall on one stream does not cut the other short. Both
// publishes are attempted; the errors of those that failed are returned
// joined.
func (c *Client) PublishIngestMarkers(ctx context.Context, timeout time.Duration) error {
	return publishMarkers(ctx, timeout, c.publish)
}

// publishMarkers is [Client.PublishIngestMarkers] over publish.
func publishMarkers(
	ctx context.Context, timeout time.Duration, publish func(context.Context, string, []byte) error,
) error {
	var errs []error
	for _, subject := range []string{SubjectMarkerWorker, SubjectMarkerTask} {
		pubCtx, cancel := context.WithTimeout(ctx, timeout)
		errs = append(errs, publish(pubCtx, subject, nil))
		cancel()
	}
	return errors.Join(errs...)
}
