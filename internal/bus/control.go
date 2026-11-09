// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import "context"

// RequestWorkerControl sends a power command to workerID as a core-NATS request
// on worker.control.<workerID> and returns the worker's reply. ctx carries the
// timeout. It returns nats.ErrNoResponders at once when the worker is not
// subscribed, which is how a worker that is asleep or disconnected answers.
func (c *Client) RequestWorkerControl(ctx context.Context, workerID string, data []byte) ([]byte, error) {
	msg, err := c.nc.RequestWithContext(ctx, WorkerControlSubject(workerID), data)
	if err != nil {
		return nil, err
	}
	return msg.Data, nil
}
