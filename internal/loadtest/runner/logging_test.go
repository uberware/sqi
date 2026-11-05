// SPDX-License-Identifier: AGPL-3.0-or-later

package runner

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestWithLevelFloor(t *testing.T) {
	var buf bytes.Buffer
	base := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	l := withLevelFloor(base, slog.LevelWarn).With("worker", "w1").WithGroup("g")
	l.InfoContext(t.Context(), "quiet")
	l.DebugContext(t.Context(), "quieter")
	l.WarnContext(t.Context(), "loud", "k", "v")
	out := buf.String()
	if strings.Contains(out, "quiet") {
		t.Errorf("records below the floor were logged:\n%s", out)
	}
	if !strings.Contains(out, "loud") || !strings.Contains(out, "worker=w1") || !strings.Contains(out, "g.k=v") {
		t.Errorf("a warning, its attributes or its group were lost:\n%s", out)
	}
}
