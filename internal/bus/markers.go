// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"errors"
)

// PublishIngestMarkers publishes an empty message on [SubjectMarkerWorker] and
// on [SubjectMarkerTask], and returns once the broker has stored both. A
// consumer that handles one has handled every message stored in its stream
// before it. Both publishes are attempted; the errors of those that failed are
// returned joined.
func (c *Client) PublishIngestMarkers(ctx context.Context) error {
	return errors.Join(
		c.publish(ctx, SubjectMarkerWorker, nil),
		c.publish(ctx, SubjectMarkerTask, nil),
	)
}
