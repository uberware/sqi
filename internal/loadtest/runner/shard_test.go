// SPDX-License-Identifier: AGPL-3.0-or-later

package runner

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/loadtest/report"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/target"
)

func TestNewRun_ShardNeedsAttach(t *testing.T) {
	sc, err := scenario.Builtin("smoke")
	if err != nil {
		t.Fatal(err)
	}
	_, err = newRun(Options{Scenario: sc, RunID: "r", Shard: report.ShardSpec{Index: 0, Of: 2}})
	if err == nil || !strings.Contains(err.Error(), "attach") {
		t.Errorf("a managed shard: err = %v, want a refusal naming attach", err)
	}
	if _, err := newRun(Options{Scenario: sc, RunID: "r", Shard: report.ShardSpec{Index: 0, Of: 2}, Attach: &target.AttachConfig{}}); err != nil {
		t.Errorf("an attached shard: %v", err)
	}
}
