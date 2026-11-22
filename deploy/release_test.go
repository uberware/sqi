// SPDX-License-Identifier: AGPL-3.0-or-later

// Package deploy_test checks that the release archives configured in .goreleaser.yaml carry the
// files an operator installs from them: the service definitions and the example configurations.
package deploy_test

import (
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// archiveFile is one entry of an archive's files list: a bare glob, or a mapping naming it in src.
type archiveFile struct {
	Src string
}

func (f *archiveFile) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		f.Src = n.Value
		return nil
	}
	var m struct {
		Src string `yaml:"src"`
	}
	if err := n.Decode(&m); err != nil {
		return err
	}
	f.Src = m.Src
	return nil
}

type goreleaserConfig struct {
	Archives []struct {
		ID    string        `yaml:"id"`
		Files []archiveFile `yaml:"files"`
	} `yaml:"archives"`
}

// archiveEntry is one files entry of one archive, with the repository files its glob matches.
type archiveEntry struct {
	archive, src string
	matches      []string
}

func archiveEntries(t *testing.T) []archiveEntry {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", ".goreleaser.yaml"))
	if err != nil {
		t.Fatalf("read .goreleaser.yaml: %v", err)
	}
	var cfg goreleaserConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse .goreleaser.yaml: %v", err)
	}
	var out []archiveEntry
	for _, a := range cfg.Archives {
		for _, f := range a.Files {
			found, err := filepath.Glob(filepath.Join("..", filepath.FromSlash(f.Src)))
			if err != nil {
				t.Fatalf("archive %q: bad glob %q: %v", a.ID, f.Src, err)
			}
			e := archiveEntry{archive: a.ID, src: f.Src}
			for _, m := range found {
				rel, err := filepath.Rel("..", m)
				if err != nil {
					t.Fatalf("rel %q: %v", m, err)
				}
				e.matches = append(e.matches, filepath.ToSlash(rel))
			}
			out = append(out, e)
		}
	}
	return out
}

func TestArchiveFiles_Exist(t *testing.T) {
	entries := archiveEntries(t)
	if len(entries) == 0 {
		t.Fatal("no archive files configured")
	}
	for _, e := range entries {
		if len(e.matches) == 0 {
			t.Errorf("archive %q: files entry %q matches nothing in the repository", e.archive, e.src)
		}
	}
}

func TestArchiveFiles_ShipTheInstallFiles(t *testing.T) {
	shipped := map[string]bool{}
	for _, e := range archiveEntries(t) {
		for _, m := range e.matches {
			shipped[m] = true
		}
	}
	want := []string{
		"deploy/systemd/README.md",
		"deploy/systemd/sqi-server.service",
		"deploy/systemd/sqi-worker.service",
		"deploy/systemd/sqi-worker@.service",
		"deploy/launchd/net.uberware.sqi-worker.plist",
		"config/sqi-server.example.yaml",
		"config/sqi-worker.example.yaml",
	}
	for _, p := range want {
		if !shipped[p] {
			got := make([]string, 0, len(shipped))
			for k := range shipped {
				got = append(got, k)
			}
			sort.Strings(got)
			t.Errorf("archives do not ship %s (shipped: %v)", p, got)
		}
	}
}

// TestSystemdReadme_LinksWorkInTheArchive pins that the systemd README, which
// the archives ship on its own, links to the documentation by absolute URL: a
// relative link points outside the extracted archive.
func TestSystemdReadme_LinksWorkInTheArchive(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("systemd", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(string(raw), "\n") {
		for _, part := range strings.Split(line, "](")[1:] {
			target, _, _ := strings.Cut(part, ")")
			if !strings.HasPrefix(target, "https://") && !strings.HasPrefix(target, "#") {
				t.Errorf("README.md:%d links to %q, which does not resolve inside the archive", i+1, target)
			}
		}
	}
}

// plistValue reads the next element of a property list: a string, or an array of strings.
func plistValue(t *testing.T, dec *xml.Decoder) any {
	t.Helper()
	for {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("plist: %v", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "string":
			var s string
			if err := dec.DecodeElement(&s, &se); err != nil {
				t.Fatalf("plist string: %v", err)
			}
			return s
		case "array":
			var items []string
			for {
				tok, err := dec.Token()
				if err != nil {
					t.Fatalf("plist array: %v", err)
				}
				switch el := tok.(type) {
				case xml.StartElement:
					var s string
					if err := dec.DecodeElement(&s, &el); err != nil {
						t.Fatalf("plist array item: %v", err)
					}
					items = append(items, s)
				case xml.EndElement:
					return items
				}
			}
		default:
			// true, false, dict: skip the element whole.
			if err := dec.Skip(); err != nil {
				t.Fatalf("plist skip: %v", err)
			}
			return nil
		}
	}
}

func TestLaunchdPlist_WellFormed(t *testing.T) {
	f, err := os.Open(filepath.Join("launchd", "net.uberware.sqi-worker.plist"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	dec := xml.NewDecoder(f)
	// Top-level dict: skip to its first child, then read key/value pairs at depth one.
	depth := 0
	top := map[string]any{}
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("plist is not well-formed XML: %v", err)
		}
		switch el := tok.(type) {
		case xml.StartElement:
			depth++
			if el.Name.Local == "key" && depth == 3 {
				var key string
				if err := dec.DecodeElement(&key, &el); err != nil {
					t.Fatal(err)
				}
				depth--
				top[key] = plistValue(t, dec)
			}
		case xml.EndElement:
			depth--
		}
	}

	if got := top["Label"]; got != "net.uberware.sqi-worker" {
		t.Errorf("Label = %v, want net.uberware.sqi-worker", got)
	}
	want := []string{
		"/bin/sh",
		"-c",
		`exec /usr/local/bin/sqi-worker start --config "$HOME/.sqi/sqi-worker.yaml"`,
	}
	got, ok := top["ProgramArguments"].([]string)
	if !ok {
		t.Fatalf("ProgramArguments = %v, want an array of strings", top["ProgramArguments"])
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ProgramArguments = %q, want %q", got, want)
	}
	for _, k := range []string{"RunAtLoad", "KeepAlive", "EnvironmentVariables"} {
		if _, ok := top[k]; !ok {
			t.Errorf("plist has no %s key", k)
		}
	}
	if strings.Contains(strings.Join(got, " "), "YOURUSERNAME") {
		t.Error("plist carries a placeholder user name")
	}
}
