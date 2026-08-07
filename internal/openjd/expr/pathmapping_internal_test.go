// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import "testing"

func TestMapPath(t *testing.T) {
	tests := []struct {
		name  string
		s     string
		rules []PathMapRule
		dst   PathFormat
		want  string
	}{
		{
			name:  "posix prefix maps and transfers the remainder",
			s:     "/projects/shot01/render.exr",
			rules: []PathMapRule{{PathMapPOSIX, "/projects", "/mnt/projects"}},
			dst:   PathPOSIX,
			want:  "/mnt/projects/shot01/render.exr",
		},
		{
			name:  "windows source matches case-insensitively, remainder re-expressed in posix dst",
			s:     "C:/studio/project/scene.ma",
			rules: []PathMapRule{{PathMapWindows, `C:\studio`, "/mnt/studio"}},
			dst:   PathPOSIX,
			want:  "/mnt/studio/project/scene.ma",
		},
		{
			name:  "windows source matches a different-case drive and directory",
			s:     "c:/STUDIO/project/scene.ma",
			rules: []PathMapRule{{PathMapWindows, `C:\studio`, "/mnt/studio"}},
			dst:   PathPOSIX,
			want:  "/mnt/studio/project/scene.ma",
		},
		{
			name:  "windows source is separator-insensitive (forward slash input)",
			s:     `C:\studio\project`,
			rules: []PathMapRule{{PathMapWindows, "C:/studio", "/mnt/studio"}},
			dst:   PathPOSIX,
			want:  "/mnt/studio/project",
		},
		{
			name:  "uri prefix maps on a path boundary with no normalization",
			s:     "s3://bucket/assets/tex/wood.png",
			rules: []PathMapRule{{PathMapURI, "s3://bucket/assets", "/mnt/assets"}},
			dst:   PathPOSIX,
			want:  "/mnt/assets/tex/wood.png",
		},
		{
			name:  "uri boundary non-match falls through to passthrough",
			s:     "s3://bucket/assets2/tex.png",
			rules: []PathMapRule{{PathMapURI, "s3://bucket/assets", "/mnt/assets"}},
			dst:   PathPOSIX,
			want:  "s3://bucket/assets2/tex.png",
		},
		{
			name: "longest source path wins and processing stops at first match",
			s:    "/a/b/c/file",
			rules: []PathMapRule{
				{PathMapPOSIX, "/a", "/short"},
				{PathMapPOSIX, "/a/b", "/long"},
			},
			dst:  PathPOSIX,
			want: "/long/c/file",
		},
		{
			name:  "component-boundary non-match passes through",
			s:     "/foobar/x",
			rules: []PathMapRule{{PathMapPOSIX, "/foo", "/bar"}},
			dst:   PathPOSIX,
			want:  "/foobar/x",
		},
		{
			name:  "no rule matches: passthrough verbatim",
			s:     "/other/x",
			rules: []PathMapRule{{PathMapPOSIX, "/projects", "/mnt"}},
			dst:   PathPOSIX,
			want:  "/other/x",
		},
		{
			name:  "nil rules: passthrough",
			s:     "/anything",
			rules: nil,
			dst:   PathPOSIX,
			want:  "/anything",
		},
		{
			name:  "empty source path rule is skipped",
			s:     "/x",
			rules: []PathMapRule{{PathMapPOSIX, "", "/dest"}},
			dst:   PathPOSIX,
			want:  "/x",
		},
		{
			// Two equal-length rules, so mapPath's stable sort leaves them in
			// this order. The first rule's own destination "/b" is itself a
			// prefix the SECOND rule's source would match — so an
			// implementation that kept scanning and applying every matching
			// rule (rather than stopping at the first) would additionally
			// rewrite "/b/x" to "/c/x". First-match-wins must stop after
			// the first rule fires.
			name: "processing stops at the first match: a later rule must not re-apply to the result",
			s:    "/a/x",
			rules: []PathMapRule{
				{PathMapPOSIX, "/a", "/b"},
				{PathMapPOSIX, "/b", "/c"},
			},
			dst:  PathPOSIX,
			want: "/b/x",
		},
		{
			// Windows sources match case-insensitively (see the other cases
			// above); POSIX ones do not. A comparator that dropped that
			// distinction (e.g. strings.EqualFold unconditionally) would
			// match "/Projects" against "/projects/x" and this would fail.
			name:  "posix source matching is case-sensitive: a differently-cased source does not match",
			s:     "/projects/x",
			rules: []PathMapRule{{PathMapPOSIX, "/Projects", "/mnt"}},
			dst:   PathPOSIX,
			want:  "/projects/x",
		},
		{
			// dst governs how the DESTINATION and the transferred remainder
			// are re-expressed, independent of the source's own flavor — a
			// POSIX source can map into a Windows destination. Every other
			// case in this table uses dst: PathPOSIX, which would let a bug
			// that ignored dst entirely (e.g. always joining with "/") pass
			// unnoticed.
			name:  "destination and remainder are re-expressed in a windows dst",
			s:     "/projects/shot01/render.exr",
			rules: []PathMapRule{{PathMapPOSIX, "/projects", `Z:\renders`}},
			dst:   PathWindows,
			want:  `Z:\renders\shot01\render.exr`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := mapPath(tc.s, tc.rules, tc.dst); got != tc.want {
				t.Errorf("mapPath(%q, ...) = %q, want %q", tc.s, got, tc.want)
			}
		})
	}
}
