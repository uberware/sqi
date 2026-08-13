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
	if !slugPattern.MatchString(name) {
		return fmt.Errorf("product: name %q is not a valid slug (lowercase, digits, '-', '_', one optional '/')", name)
	}
	return nil
}

// ValidateOptions bounds one [ValidateTemplate] call.
//
// It exists because a product template is validated from two very different
// places. [ParseDefinition] reads operator-supplied definition files and the
// built-ins compiled into this binary; POST /api/v1/products and
// PUT /api/v1/products/{name} accept an ARBITRARY client-supplied body,
// anonymously when auth is off (the default). The second caller needs the
// operator's configured EXPR limits and a wall-clock backstop; the first has
// neither available, and does not need them.
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
// inline it would also be unreachable from a test, because nothing this mapping
// controls is observable while the expression walk is gated.
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
// It is split out so a test can drive it with the expression walk force-enabled
// (openjd.ValidateOptions' CheckEXPRExpressionsWhileUnsupported). Production
// never sets that field, and while EXPR is StatusInProgress the walk never runs
// -- so without this seam the deadline plumbing below would have no test that
// can reach it, and "the deadline is wired" would be indistinguishable from
// "the deadline is dropped on the floor".
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
	Category    string    `yaml:"category"`
	Version     string    `yaml:"version"`
	Template    yaml.Node `yaml:"template"`
}

// ParseDefinition parses a YAML product definition file (metadata + inline
// template) into a store.Product. Source is left empty for the caller to set.
// The inline template is re-serialized to YAML and validated; a malformed
// template is an error.
func ParseDefinition(data []byte) (store.Product, error) {
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
	// No ExprLimits and no deadline, deliberately. This path validates
	// OPERATOR-supplied content -- the built-ins embedded in this binary (loaded
	// from package init, where no configuration exists yet), definition files on
	// disk, and sha256-pinned definitions fetched from an operator-configured
	// preset library. None of it is an anonymous request, and the built-in
	// loader in particular runs before any config has been parsed. The
	// client-supplied route, POST/PUT /api/v1/products, calls ValidateTemplate
	// directly and passes both.
	if err := ValidateTemplate(string(rawTemplate), store.TemplateFormatYAML,
		ValidateOptions{EnforceLimits: true}); err != nil {
		return store.Product{}, err
	}
	return store.Product{
		Name:        df.Name,
		Title:       df.Title,
		Description: df.Description,
		Category:    df.Category,
		Version:     df.Version,
		Template:    string(rawTemplate),
		Format:      store.TemplateFormatYAML,
	}, nil
}
