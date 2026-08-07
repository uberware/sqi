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
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := mapPath(tc.s, tc.rules, tc.dst); got != tc.want {
				t.Errorf("mapPath(%q, ...) = %q, want %q", tc.s, got, tc.want)
			}
		})
	}
}
