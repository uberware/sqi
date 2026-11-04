// SPDX-License-Identifier: AGPL-3.0-or-later

package scenario

// maxTaskParamValues is sqi's cap on the values of one task parameter
// (internal/openjd/validate.go).
const maxTaskParamValues = 1024

// SplitTasks divides tasksPerStep tasks over two task parameters so that no
// parameter exceeds sqi's 1024-value cap. The first (frames) is a frame range
// 1-frames*chunk; the second (blocks) multiplies it. It returns the factors
// with frames*blocks == tasksPerStep, frames*chunk <= 1024 and blocks <= 1024,
// preferring the largest frames, so blocks is 1 whenever the frame range fits.
// ok is false when no such factorization exists, or an argument is below 1.
func SplitTasks(tasksPerStep, chunk int) (frames, blocks int, ok bool) {
	if tasksPerStep < 1 || chunk < 1 {
		return 0, 0, false
	}
	maxFrames := maxTaskParamValues / chunk
	if tasksPerStep <= maxFrames {
		return tasksPerStep, 1, true
	}
	for f := maxFrames; f >= 1; f-- {
		if tasksPerStep%f == 0 && tasksPerStep/f <= maxTaskParamValues {
			return f, tasksPerStep / f, true
		}
	}
	return 0, 0, false
}
