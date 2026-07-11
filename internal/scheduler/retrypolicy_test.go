package scheduler

import (
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
)

//nolint:modernize // ptr helper is more readable than *new(int) wrapper
func ptr(n int) *int { return &n }

func TestResolveRetryPolicy_Precedence(t *testing.T) {
	def := RetryPolicy{MaxAttempts: 3, RetryDelay: 30 * time.Second, FailureLimit: 0}
	tests := []struct {
		name             string
		job              store.Job
		queue            store.Queue
		farm             store.Farm
		wantMax, wantLim int
		wantDelay        time.Duration
	}{
		{"all inherit", store.Job{}, store.Queue{}, store.Farm{}, 3, 0, 30 * time.Second},
		//nolint:modernize // ptr is more readable than new()
		{"farm overrides", store.Job{}, store.Queue{}, store.Farm{MaxAttempts: ptr(5)}, 5, 0, 30 * time.Second},
		//nolint:modernize // ptr is more readable than new()
		{"queue beats farm", store.Job{}, store.Queue{MaxAttempts: ptr(4)}, store.Farm{MaxAttempts: ptr(5)}, 4, 0, 30 * time.Second},
		//nolint:modernize // ptr is more readable than new()
		{"job beats all", store.Job{MaxAttempts: ptr(2)}, store.Queue{MaxAttempts: ptr(4)}, store.Farm{MaxAttempts: ptr(5)}, 2, 0, 30 * time.Second},
		//nolint:modernize // ptr is more readable than new()
		{"delay + limit from job", store.Job{RetryDelaySeconds: ptr(10), FailureLimit: ptr(25)}, store.Queue{}, store.Farm{}, 3, 25, 10 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveRetryPolicy(tt.job, tt.queue, tt.farm, def)
			if got.MaxAttempts != tt.wantMax || got.FailureLimit != tt.wantLim || got.RetryDelay != tt.wantDelay {
				t.Fatalf("got %+v want max=%d lim=%d delay=%s", got, tt.wantMax, tt.wantLim, tt.wantDelay)
			}
		})
	}
}
