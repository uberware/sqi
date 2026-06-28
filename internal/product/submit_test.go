// SPDX-License-Identifier: AGPL-3.0-or-later

package product_test

// End-to-end matrix test: each built-in product (script / python / container)
// is submitted through the real Submitter backed by the fake store. No binary
// or NATS instance is required.

import (
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/product"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// seedFarmQueue inserts a farm and queue into st and returns their IDs.
func seedFarmQueue(t *testing.T, st *fake.Store) (farmID, queueID string) {
	t.Helper()
	ctx := t.Context()

	farm, err := st.CreateFarm(ctx, store.Farm{
		ID:   uuid.NewString(),
		Name: "test-farm-" + uuid.NewString()[:8],
	})
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	queue, err := st.CreateQueue(ctx, store.Queue{
		ID:     uuid.NewString(),
		FarmID: farm.ID,
		Name:   "test-queue-" + uuid.NewString()[:8],
	})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	return farm.ID, queue.ID
}

// assertDockerRequirement checks that the step carries an attr.worker.docker
// attribute requirement with anyOf=["true"].
func assertDockerRequirement(t *testing.T, step store.Step) {
	t.Helper()
	if step.HostRequirements == nil {
		t.Fatal("container step must have HostRequirements, got nil")
	}
	for _, attr := range step.HostRequirements.Attributes {
		if attr.Name == "attr.worker.docker" {
			if !slices.Contains(attr.AnyOf, "true") {
				t.Fatalf("attr.worker.docker found but anyOf does not contain \"true\": %v", attr.AnyOf)
			}
			return
		}
	}
	t.Fatal("container step HostRequirements.Attributes does not contain attr.worker.docker")
}

// TestBuiltinProductsSubmitEndToEnd verifies each built-in submits successfully
// through the real Submitter, producing at least one step and one task. The
// container product additionally carries the attr.worker.docker host requirement.
func TestBuiltinProductsSubmitEndToEnd(t *testing.T) {
	st := fake.New()
	farmID, queueID := seedFarmQueue(t, st)
	cat := product.NewCatalog(st)
	sub := openjd.NewSubmitter(st)

	cases := []struct {
		name   string
		params map[string]string
	}{
		{"script", map[string]string{"Command": "echo hi"}},
		{"python", map[string]string{"Script": "print(1)"}},
		{"container", map[string]string{"Image": "alpine:latest"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := cat.GetByName(t.Context(), tc.name)
			if err != nil {
				t.Fatalf("GetByName(%q): %v", tc.name, err)
			}
			res, err := sub.Submit(t.Context(), p.Template, p.Format, openjd.SubmitOptions{
				FarmID:     farmID,
				QueueID:    queueID,
				Parameters: tc.params,
			})
			if err != nil {
				t.Fatalf("Submit %q: %v", tc.name, err)
			}
			if len(res.Steps) == 0 || len(res.Tasks) == 0 {
				t.Fatalf("%s: expected steps and tasks; got steps=%d tasks=%d",
					tc.name, len(res.Steps), len(res.Tasks))
			}
			if tc.name == "container" {
				assertDockerRequirement(t, res.Steps[0])
			}
		})
	}
}
