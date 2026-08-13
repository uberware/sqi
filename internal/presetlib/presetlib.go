// SPDX-License-Identifier: AGPL-3.0-or-later

// Package presetlib is the client for a community preset library: it fetches a
// static JSON index (cached in memory) and, on install, fetches and verifies a
// single preset definition. The configured index URL is the trust boundary; the
// per-entry sha256 is an integrity + update-detection check, not authorship.
package presetlib

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/uberware/sqi/internal/product"
	"github.com/uberware/sqi/internal/store"
)

// DefaultCacheTTL is how long a fetched index is reused before re-fetching.
const DefaultCacheTTL = 5 * time.Minute

// ErrNotConfigured is returned when no library URL is configured.
var ErrNotConfigured = errors.New("presetlib: no preset library configured")

// ErrFingerprintMismatch is returned when a downloaded definition's sha256 does
// not match the fingerprint the index vouched for.
var ErrFingerprintMismatch = errors.New("presetlib: definition fingerprint mismatch")

// IndexEntry is one preset listed in the library index. Definition is a path
// resolved relative to the index URL.
type IndexEntry struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Category    string `json:"category"`
	Version     string `json:"version"`
	Definition  string `json:"definition"`
	Sha256      string `json:"sha256"`
}

type indexDoc struct {
	Presets []IndexEntry `json:"presets"`
}

// Service fetches and caches the library index and fetches preset definitions.
type Service struct {
	rawURL string
	client *http.Client
	ttl    time.Duration

	mu       sync.Mutex
	cached   []IndexEntry
	cachedAt time.Time
}

// New returns a Service for the index at rawURL (empty ⇒ disabled), caching the
// index for ttl.
func New(rawURL string, ttl time.Duration) *Service {
	return &Service{
		rawURL: rawURL,
		client: &http.Client{Timeout: 15 * time.Second},
		ttl:    ttl,
	}
}

// Configured reports whether a library URL is set.
func (s *Service) Configured() bool { return s.rawURL != "" }

// FetchIndex returns the parsed index, served from cache within the TTL unless
// forceRefresh is set.
func (s *Service) FetchIndex(ctx context.Context, forceRefresh bool) ([]IndexEntry, error) {
	if !s.Configured() {
		return nil, ErrNotConfigured
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !forceRefresh && s.cached != nil && time.Since(s.cachedAt) < s.ttl {
		return s.cached, nil
	}
	body, err := s.get(ctx, s.rawURL)
	if err != nil {
		// On a failed refresh, keep serving the last good cache if we have one.
		if s.cached != nil {
			return s.cached, fmt.Errorf("presetlib: refresh failed (serving cached): %w", err)
		}
		return nil, err
	}
	var doc indexDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("presetlib: parse index: %w", err)
	}
	s.cached = doc.Presets
	s.cachedAt = time.Now()
	return s.cached, nil
}

// FetchDefinition downloads the entry's definition (resolved relative to the
// index URL), verifies its sha256, and parses it into a store.Product under
// opts.
//
// opts carries the operator's EXPR limits and this request's wall-clock
// backstop, because both preset routes that call this are reachable from an
// anonymous HTTP request when auth is off. The sha256 pinning bounds WHAT is
// validated -- the body is vouched for by the operator's index, not chosen by
// the caller -- but it does not bound how often, or for how long, this server
// is asked to validate it. See [product.ValidateOptions].
func (s *Service) FetchDefinition(
	ctx context.Context, entry IndexEntry, opts product.ValidateOptions,
) (store.Product, error) {
	if !s.Configured() {
		return store.Product{}, ErrNotConfigured
	}
	defURL, err := s.resolve(entry.Definition)
	if err != nil {
		return store.Product{}, err
	}
	body, err := s.get(ctx, defURL)
	if err != nil {
		return store.Product{}, err
	}
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != entry.Sha256 {
		return store.Product{}, fmt.Errorf("%w: index=%s got=%s", ErrFingerprintMismatch, entry.Sha256, got)
	}
	p, err := product.ParseDefinition(body, opts)
	if err != nil {
		return store.Product{}, fmt.Errorf("presetlib: %w", err)
	}
	return p, nil
}

// resolve turns a relative definition path into an absolute URL against the
// index URL.
func (s *Service) resolve(ref string) (string, error) {
	base, err := url.Parse(s.rawURL)
	if err != nil {
		return "", fmt.Errorf("presetlib: bad index url: %w", err)
	}
	rel, err := url.Parse(ref)
	if err != nil {
		return "", fmt.Errorf("presetlib: bad definition ref %q: %w", ref, err)
	}
	return base.ResolveReference(rel).String(), nil
}

func (s *Service) get(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("presetlib: fetch %s: %w", u, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("presetlib: fetch %s: status %d", u, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 5<<20)) // 5 MiB cap
}
