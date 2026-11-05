// SPDX-License-Identifier: AGPL-3.0-or-later

package scenario

import "testing"

func TestSplitTasks(t *testing.T) {
	tests := []struct {
		name                   string
		tasks, chunk           int
		wantFrames, wantBlocks int
		wantOK                 bool
	}{
		{"small", 10, 1, 10, 1, true},
		{"exactly the cap", 1024, 1, 1024, 1, true},
		{"chunked fits", 100, 10, 100, 1, true},
		{"chunked exactly the cap", 128, 8, 128, 1, true},
		{"250k tasks", 250000, 1, 1000, 250, true},
		{"1M tasks", 1000000, 1, 1000, 1000, true},
		{"chunked splits", 200, 10, 100, 2, true},
		{"prime above the cap", 1031, 1, 0, 0, false},
		{"chunk above the cap", 10, 2000, 0, 0, false},
		{"zero tasks", 0, 1, 0, 0, false},
		{"zero chunk", 10, 0, 0, 0, false},
		{"negative tasks", -5, 1, 0, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			frames, blocks, ok := SplitTasks(tc.tasks, tc.chunk)
			if ok != tc.wantOK {
				t.Fatalf("SplitTasks(%d, %d) ok = %v, want %v", tc.tasks, tc.chunk, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if frames != tc.wantFrames || blocks != tc.wantBlocks {
				t.Errorf("SplitTasks(%d, %d) = (%d, %d), want (%d, %d)",
					tc.tasks, tc.chunk, frames, blocks, tc.wantFrames, tc.wantBlocks)
			}
			if frames*blocks != tc.tasks {
				t.Errorf("frames*blocks = %d, want %d", frames*blocks, tc.tasks)
			}
			if frames*tc.chunk > 1024 || blocks > 1024 {
				t.Errorf("caps broken: frames*chunk=%d blocks=%d", frames*tc.chunk, blocks)
			}
			if tc.tasks*tc.chunk <= 1024 && blocks != 1 {
				t.Errorf("blocks = %d, want 1 when tasks*chunk fits", blocks)
			}
		})
	}
}

func TestSplitTasks_InvariantsHoldEverywhere(t *testing.T) {
	for _, chunk := range []int{1, 2, 3, 10, 100, 1024} {
		for tasks := 1; tasks <= 5000; tasks++ {
			frames, blocks, ok := SplitTasks(tasks, chunk)
			if !ok {
				continue
			}
			if frames*blocks != tasks || frames*chunk > 1024 || blocks > 1024 || blocks < 1 || frames < 1 {
				t.Fatalf("SplitTasks(%d, %d) = (%d, %d) breaks an invariant", tasks, chunk, frames, blocks)
			}
			if tasks*chunk <= 1024 && blocks != 1 {
				t.Fatalf("SplitTasks(%d, %d): blocks = %d, want 1", tasks, chunk, blocks)
			}
		}
	}
}
