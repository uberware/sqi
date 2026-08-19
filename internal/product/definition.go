// SPDX-License-Identifier: AGPL-3.0-or-later

// Package product is the catalog layer over OpenJD templates: it parses product
// definition files, holds the embedded built-ins, validates inline templates,
// and overlays read-only built-ins on the store via Catalog.
package product

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/store"
)

// slugPattern constrains product names: lowercase letters, digits, '_' and '-',
// with at most one '/' namespace separator (e.g. "studio/maya-render").
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*(/[a-z0-9][a-z0-9_-]*)?$`)

// validateName returns an error if name is not a valid product slug.
func validateName(name string) error {
	if name == "" {
		return errors.New("product: name is required")
	}
	if err := checkLen("name", name, MaxNameLen); err != nil {
		return err
	}
	if !slugPattern.MatchString(name) {
		return fmt.Errorf("product: name %q is not a valid slug (lowercase, digits, '-', '_', one optional '/')", name)
	}
	return nil
}

// ValidateOptions bounds one [ValidateTemplate] or [ParseDefinition] call.
//
// It exists because a product template is validated from places with very
// different trust and configuration properties. POST /api/v1/products and
// PUT /api/v1/products/{name} accept an ARBITRARY client-supplied body,
// anonymously when auth is off (the default), and need both the operator's
// configured EXPR limits and a wall-clock backstop. The preset routes
// (GET /api/v1/presets/{name}, POST /api/v1/presets/{name}/install) validate a
// sha256-pinned body from an operator-configured index -- not client-chosen
// content, but still one anonymous, repeatable request per validation, so they
// take both too. The built-in loader runs from package init, where no
// configuration exists at all, and can take neither.
//
// The zero value reproduces the pre-H1 behavior exactly: limit enforcement off,
// default EXPR limits, no deadline.
type ValidateOptions struct {
	// EnforceLimits gates OpenJD's quantitative limit checks; see
	// [openjd.ValidateOptions].
	EnforceLimits bool

	// ExprLimits is the operator's configured bound on what this template's
	// EXPR expression walk may spend (sub-project E4d).
	//
	// The zero value means "use the defaults", which is what every caller with
	// no operator configuration to offer gets. Before EXPR sub-project H1 that
	// was the ONLY thing this path could get, so an operator who tightened the
	// four knobs found product template validation still running on the
	// defaults -- invisible while the walk is gated on EXPR being
	// StatusSupported, and live the moment H2 flips it.
	//
	// EVERY ROUTE THAT REACHES THIS PACKAGE FROM AN HTTP REQUEST MUST SET IT.
	// H1's own whole-branch review found the preset install path still on the
	// defaults after the create/update route was fixed -- two routes behind the
	// same permission (policy.ProductsManage), both ending in a catalog write,
	// behaving differently. The limits question is not about who supplies the
	// content: an operator who tightened a knob asked for it to be enforced
	// wherever validation happens.
	ExprLimits openjd.ExprLimits

	// Deadline, when non-zero, is an absolute wall-clock instant after which
	// validation stops and [ValidateTemplate] returns an error wrapping
	// [expr.ErrDeadlineExceeded] -- H1's backstop.
	//
	// PER REQUEST, never stored: it is computed from a configured duration at
	// the top of one HTTP request. See [openjd.ExprLimits]' Deadline field for
	// what storing one on a long-lived value does.
	Deadline time.Time
}

// openjdOptions maps these options onto the [openjd.ValidateOptions] the
// validator takes.
//
// A named method rather than an inline literal for the reason internal/server's
// ExprLimitsFromConfig is a named function: a struct literal copying one
// options type into another is the shape where a field can be dropped, renamed
// or pointed at the wrong member and still compile, start and serve. Left
// inline it would also be unreachable from a test asserting on the mapping
// itself, field by field, rather than on one downstream consequence at a time.
func (o ValidateOptions) openjdOptions() openjd.ValidateOptions {
	return openjd.ValidateOptions{
		EnforceLimits: o.EnforceLimits,
		ExprLimits:    o.ExprLimits,
		Deadline:      o.Deadline,
	}
}

// ValidateTemplate parses rawTemplate as OpenJD in the given format and runs the
// standard validation under opts. It returns nil when the template is valid, an
// error carrying the OpenJD ValidationErrors when it is not, or an error
// wrapping [expr.ErrDeadlineExceeded] when opts.Deadline passed mid-validation.
//
// Those last two are different claims and callers must keep them apart: the
// ValidationErrors say the template is wrong, and a client that retries it will
// get the same answer forever; a deadline says THIS SERVER gave up on a
// template that might well validate on an idle machine. internal/api answers
// 400 for the first and 503 for the second. Match the deadline structurally
// with errors.Is(err, expr.ErrDeadlineExceeded), never by reading the message.
func ValidateTemplate(rawTemplate string, format store.TemplateFormat, opts ValidateOptions) error {
	pf := openjd.FormatYAML
	if format == store.TemplateFormatJSON {
		pf = openjd.FormatJSON
	}
	tmpl, err := openjd.Parse([]byte(rawTemplate), pf)
	if err != nil {
		return fmt.Errorf("product: template parse: %w", err)
	}
	return validateParsed(tmpl, opts.openjdOptions())
}

// validateParsed runs the validation tail shared by [ValidateTemplate] and its
// tests.
//
// It was split out as a seam so a test could drive the validation tail with an
// already-parsed template while the expression walk was still gated off in
// production (openjd.ValidateOptions' since-deleted
// CheckEXPRExpressionsWhileUnsupported). Sub-project H2 made EXPR
// StatusSupported, so [ValidateTemplate] now reaches the same walk on its own
// and the tests that needed the override drive the exported entry point
// instead. The seam is kept because it is still the cheapest way to assert on
// the tail alone, with a template a test built rather than a string it had to
// serialize.
func validateParsed(tmpl *openjd.JobTemplate, o openjd.ValidateOptions) error {
	errs, ferr := openjd.ValidateWithBudget(tmpl, o)
	if ferr != nil {
		// Wrapped, not replaced: the sentinel must survive to the HTTP layer,
		// which tells this outcome from a validation failure with errors.Is.
		return fmt.Errorf("product: template validation: %w", ferr)
	}
	if len(errs) > 0 {
		return fmt.Errorf("product: template validation: %w", errs)
	}
	return nil
}

// definitionFile is the on-disk YAML shape of a product definition: metadata
// plus an inline OpenJD template.
type definitionFile struct {
	Name        string    `yaml:"name"`
	Title       string    `yaml:"title"`
	Description string    `yaml:"description"`
	Readme      string    `yaml:"readme"`
	Category    string    `yaml:"category"`
	Version     string    `yaml:"version"`
	Template    yaml.Node `yaml:"template"`
}

// ParseDefinition parses a YAML product definition file (metadata + inline
// template) into a store.Product. Source is left empty for the caller to set.
// The inline template is re-serialized to YAML and validated under opts; a
// malformed template is an error, and so is a breach of opts.Deadline (wrapping
// [expr.ErrDeadlineExceeded] -- see [ValidateTemplate] for why the two must be
// told apart).
//
// opts is a REQUIRED argument rather than an implicit default because the
// callers differ in what they can and must supply, and an implicit default is
// how the preset routes silently kept validating on openjd.DefaultExprLimits()
// after the create/update route was fixed:
//
//   - [LoadBuiltins], from package init, has no configuration to offer and
//     passes only EnforceLimits. That one is a genuine exemption.
//   - internal/presetlib, reached from GET /api/v1/presets/{name} and
//     POST /api/v1/presets/{name}/install, passes the operator's limits and a
//     per-request deadline.
func ParseDefinition(data []byte, opts ValidateOptions) (store.Product, error) {
	var df definitionFile
	if err := yaml.Unmarshal(data, &df); err != nil {
		return store.Product{}, fmt.Errorf("product: definition parse: %w", err)
	}
	if err := validateName(df.Name); err != nil {
		return store.Product{}, err
	}
	if df.Title == "" {
		return store.Product{}, errors.New("product: title is required")
	}
	if df.Template.IsZero() {
		return store.Product{}, errors.New("product: template is required")
	}
	rawTemplate, err := yaml.Marshal(&df.Template)
	if err != nil {
		return store.Product{}, fmt.Errorf("product: re-serialize template: %w", err)
	}
	p := store.Product{
		Name:        df.Name,
		Title:       df.Title,
		Description: df.Description,
		Readme:      df.Readme,
		Category:    df.Category,
		Version:     df.Version,
		Format:      store.TemplateFormatYAML,
	}
	if err := ValidateMetadata(p); err != nil {
		return store.Product{}, err
	}
	if err := ValidateTemplate(string(rawTemplate), store.TemplateFormatYAML, opts); err != nil {
		return store.Product{}, err
	}
	p.Template = string(rawTemplate)
	return p, nil
}
