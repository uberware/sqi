// SPDX-License-Identifier: AGPL-3.0-only

package ui

// Unit tests for the embedded web UI handler. They run against the real
// embedded bundle (package web) so the tests also confirm the embed wiring is
// intact. The content checks verify the React app shell is present (id="root")
// rather than depending on human-readable text that may change.

import (
	"bytes"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestHandler(t *testing.T) *Handler {
	t.Helper()
	h, err := NewHandler(slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h
}

// do issues a request through the handler and returns the recorded response.
func do(t *testing.T, h *Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestServesShellAtRoot(t *testing.T) {
	h := newTestHandler(t)
	rec := do(t, h, http.MethodGet, "/")

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /: status = %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("GET /: Content-Type = %q, want text/html", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("GET /: Cache-Control = %q, want no-cache", cc)
	}
	if body := rec.Body.String(); !strings.Contains(body, `id="root"`) {
		t.Errorf("GET /: body does not contain React app shell (id=root)")
	}
}

func TestServesConcreteAsset(t *testing.T) {
	h := newTestHandler(t)
	rec := do(t, h, http.MethodGet, "/index.html")

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /index.html: status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), `id="root"`) {
		t.Errorf("GET /index.html: body does not contain React app shell (id=root)")
	}
}

func TestDotPathCleansToRootShell(t *testing.T) {
	h := newTestHandler(t)
	// "/." cleans to "/", which must serve the shell.
	rec := do(t, h, http.MethodGet, "/.")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /.: status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), `id="root"`) {
		t.Errorf("GET /.: expected SPA shell body (id=root)")
	}
}

func TestHeadUnknownAssetIs404(t *testing.T) {
	h := newTestHandler(t)
	rec := do(t, h, http.MethodHead, "/missing.js")
	if rec.Code != http.StatusNotFound {
		t.Errorf("HEAD /missing.js: status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestSPAFallbackUnderUIPrefix(t *testing.T) {
	h := newTestHandler(t)
	for _, target := range []string{"/ui", "/ui/", "/ui/jobs/123", "/ui/workers"} {
		rec := do(t, h, http.MethodGet, target)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: status = %d, want %d (SPA fallback)", target, rec.Code, http.StatusOK)
			continue
		}
		if !strings.Contains(rec.Body.String(), `id="root"`) {
			t.Errorf("GET %s: expected SPA shell body (id=root)", target)
		}
	}
}

func TestUnknownAssetOutsideUIIs404(t *testing.T) {
	h := newTestHandler(t)
	for _, target := range []string{"/missing.js", "/assets/nope.css", "/favicon.ico", "/random/path"} {
		rec := do(t, h, http.MethodGet, target)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want %d", target, rec.Code, http.StatusNotFound)
		}
	}
}

func TestNonGETMethodNotAllowed(t *testing.T) {
	h := newTestHandler(t)
	rec := do(t, h, http.MethodPost, "/")

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /: status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
	if allow := rec.Header().Get("Allow"); allow != "GET, HEAD" {
		t.Errorf("POST /: Allow = %q, want %q", allow, "GET, HEAD")
	}
}

func TestHeadRequestHasNoBody(t *testing.T) {
	h := newTestHandler(t)
	rec := do(t, h, http.MethodHead, "/")

	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD /: status = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD /: body length = %d, want 0", rec.Body.Len())
	}
}

func TestPathTraversalIsContained(t *testing.T) {
	h := newTestHandler(t)
	// A traversal attempt collapses to /etc/passwd after path.Clean, which is
	// not an embedded file and is not under /ui, so it must 404 rather than
	// escape the embedded tree.
	rec := do(t, h, http.MethodGet, "/../../../../etc/passwd")
	if rec.Code != http.StatusNotFound {
		t.Errorf("traversal: status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestServeAssetBuffersNonSeekableFS(t *testing.T) {
	const body = "<!doctype html><title>buffered</title>"
	// A handler backed by an fs.FS whose files do not implement io.Seeker
	// exercises the buffered (io.ReadAll) fallback in serveAsset, which the
	// always-seekable embed.FS never reaches in production.
	h := &Handler{assets: nonSeekFS{data: []byte(body)}, logger: slog.New(slog.DiscardHandler)}
	rec := do(t, h, http.MethodGet, "/")

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /: status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != body {
		t.Errorf("GET /: body = %q, want %q", got, body)
	}
}

// ── non-seekable fs.FS test double ──────────────────────────────────────────────

// nonSeekFS serves a single index.html whose file does not implement io.Seeker.
type nonSeekFS struct {
	data []byte
}

func (fsys nonSeekFS) Open(name string) (fs.File, error) {
	if name != indexFile {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return &nonSeekFile{r: bytes.NewReader(fsys.data), size: int64(len(fsys.data))}, nil
}

// nonSeekFile intentionally omits a Seek method so it satisfies fs.File but not
// io.ReadSeeker.
type nonSeekFile struct {
	r    *bytes.Reader
	size int64
}

func (f *nonSeekFile) Read(p []byte) (int, error) { return f.r.Read(p) }
func (*nonSeekFile) Close() error                 { return nil }
func (f *nonSeekFile) Stat() (fs.FileInfo, error) { return nonSeekInfo{size: f.size}, nil }

// nonSeekInfo is a minimal fs.FileInfo for nonSeekFile.
type nonSeekInfo struct {
	size int64
}

func (nonSeekInfo) Name() string       { return indexFile }
func (i nonSeekInfo) Size() int64      { return i.size }
func (nonSeekInfo) Mode() fs.FileMode  { return 0o444 }
func (nonSeekInfo) ModTime() time.Time { return time.Time{} }
func (nonSeekInfo) IsDir() bool        { return false }
func (nonSeekInfo) Sys() any           { return nil }
