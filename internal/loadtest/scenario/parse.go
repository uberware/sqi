// SPDX-License-Identifier: AGPL-3.0-or-later

package scenario

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"time"

	"gopkg.in/yaml.v3"
)

// Defaults for fields a scenario may leave out.
const (
	defaultDrainTimeout = 10 * time.Minute
	defaultHeartbeat    = 15 * time.Second
	defaultLogLineBytes = 120
	defaultQueue        = "default"
	defaultStore        = "sqlite"
	// defaultTaskSeconds is how long a task runs when the job class says nothing.
	defaultTaskSeconds = 5
)

// Parse decodes a scenario and applies its defaults. A key the schema does not
// know is an error, so a misspelled field cannot silently do nothing.
func Parse(data []byte) (*Scenario, error) {
	var s Scenario
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("scenario: empty document")
		}
		return nil, fmt.Errorf("scenario: %w", err)
	}
	s.ApplyDefaults()
	return &s, nil
}

// ApplyDefaults fills the fields a scenario may leave out. It is idempotent.
func (s *Scenario) ApplyDefaults() {
	if s.DrainTimeout.Duration == 0 {
		s.DrainTimeout.Duration = defaultDrainTimeout
	}
	if s.Server.Store == "" {
		s.Server.Store = defaultStore
	}
	if len(s.Farm.Queues) == 0 {
		s.Farm.Queues = []QueueSpec{{Name: defaultQueue}}
	}
	for i := range s.Workers {
		if s.Workers[i].Heartbeat.Duration == 0 {
			s.Workers[i].Heartbeat.Duration = defaultHeartbeat
		}
	}
	for i := range s.Jobs {
		j := &s.Jobs[i]
		if j.Steps == 0 {
			j.Steps = 1
		}
		if j.Chunk == 0 {
			j.Chunk = 1
		}
		if j.Queue == "" {
			j.Queue = s.Farm.Queues[0].Name
		}
		if j.Task.Duration == (Dist{}) {
			j.Task.Duration = Dist{Kind: DistConstant, Value: defaultTaskSeconds, Unit: "s"}
		}
		if j.Task.LogLines == (Dist{}) {
			j.Task.LogLines = Dist{Kind: DistConstant}
		}
		if j.Task.LogLineBytes == 0 {
			j.Task.LogLineBytes = defaultLogLineBytes
		}
	}
}
