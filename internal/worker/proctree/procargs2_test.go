// SPDX-License-Identifier: AGPL-3.0-or-later

package proctree

import (
	"encoding/binary"
	"slices"
	"testing"
)

// procArgs2 builds a KERN_PROCARGS2 buffer: argc, the executable path padded
// with NULs, argv, then the environment, each NUL-terminated.
func procArgs2(argc int32, exe string, pad int, argv, env []string) []byte {
	b := binary.LittleEndian.AppendUint32(nil, uint32(argc))
	b = append(b, exe...)
	for range pad {
		b = append(b, 0)
	}
	for _, s := range argv {
		b = append(append(b, s...), 0)
	}
	for _, s := range env {
		b = append(append(b, s...), 0)
	}
	return append(b, 0)
}

func TestParseProcArgs2(t *testing.T) {
	tests := []struct {
		name    string
		buf     []byte
		want    []string
		wantErr bool
	}{
		{
			name: "argv then env",
			buf:  procArgs2(2, "/bin/sh", 3, []string{"sh", "-c"}, []string{"A=1", "SQI_PROCESS_TREE=t"}),
			want: []string{"A=1", "SQI_PROCESS_TREE=t"},
		},
		{
			name: "no argv",
			buf:  procArgs2(0, "/bin/sh", 1, nil, []string{"A=1"}),
			want: []string{"A=1"},
		},
		{
			name: "no env",
			buf:  procArgs2(1, "/bin/sh", 1, []string{"sh"}, nil),
			want: nil,
		},
		{name: "shorter than argc", buf: []byte{1, 0}, wantErr: true},
		{
			name:    "argv cut short",
			buf:     procArgs2(3, "/bin/sh", 1, []string{"sh"}, nil)[:12],
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseProcArgs2(tt.buf)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseProcArgs2 err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && !slices.Equal(got, tt.want) {
				t.Errorf("parseProcArgs2 = %q, want %q", got, tt.want)
			}
		})
	}
}
