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
// Product templates are validated by callers with different trust and
// configuration. POST /api/v1/products and PUT /api/v1/products/{name} accept
// an arbitrary client-supplied body, anonymously when auth is off (the
// default), and need both the operator's configured EXPR limits and a
// wall-clock deadline. The preset routes (GET /api/v1/presets/{name},
// POST /api/v1/presets/{name}/install) validate a sha256-pinned body from an
// operator-configured index. That content is not client-chosen, but each
// validation is still one repeatable request, so they take both too. The
// built-in loader runs from package init, where no configuration exists, and
// takes neither.
//
// The zero value means limit enforcement off, default EXPR limits, no
// deadline.
type ValidateOptions struct {
	// EnforceLimits gates OpenJD's quantitative limit checks; see
	// [openjd.ValidateOptions].
	EnforceLimits bool

	// ExprLimits is the operator's configured bound on what this template's
	// EXPR expression walk may spend. The zero value means the defaults.
	//
	// Every route that reaches this package from an HTTP request must set it,
	// or validation runs on the defaults even when the operator tightened the
	// four limits. The product create/update route and the preset install
	// route sit behind the same permission (policy.ProductsManage), both end
	// in a catalog write, and must enforce the same limits whoever supplied
	// the content.
	ExprLimits openjd.ExprLimits

	// Deadline, when non-zero, is an absolute wall-clock instant after which
	// validation stops and [ValidateTemplate] returns an error wrapping
	// [expr.ErrDeadlineExceeded].
	//
	// It is set per request and never stored: it is computed from a
	// configured duration at the start of one HTTP request. See the Deadline
	// field of [openjd.ExprLimits] for what a deadline stored on a long-lived
	// value does.
	Deadline time.Time
}

// openjdOptions maps these options onto the [openjd.ValidateOptions] the
// validator takes.
//
// It is a named method, like internal/server's ExprLimitsFromConfig, so a test
// can assert on the mapping field by field: a struct literal copying one
// options type into another can drop or misassign a field and still compile.
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
// Callers must tell the last two apart. ValidationErrors mean the template is
// wrong and a retry gets the same answer; a deadline means this server stopped
// on a template that may validate on an idle machine. internal/api answers 400
// for the first and 503 for the second. Match the deadline with
// errors.Is(err, expr.ErrDeadlineExceeded), not by reading the message.
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
// It is separate so a test can assert on the tail with a template it built,
// without serializing it first.
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
// [expr.ErrDeadlineExceeded]; see [ValidateTemplate] for why the two must be
// told apart).
//
// opts is required rather than defaulted because callers differ in what they
// can supply, and a default would let the preset routes validate on
// openjd.DefaultExprLimits() while the create/update route honors the
// operator's configuration:
//
//   - loadBuiltins, from package init, has no configuration to offer and
//     passes only EnforceLimits.
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
