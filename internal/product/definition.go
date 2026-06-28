// SPDX-License-Identifier: AGPL-3.0-or-later

// Package product is the catalog layer over OpenJD templates: it parses product
// definition files, holds the embedded built-ins, validates inline templates,
// and overlays read-only built-ins on the store via Catalog.
package product

import (
	"errors"
	"fmt"
	"regexp"

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

// ValidateTemplate parses rawTemplate as OpenJD in the given format and runs the
// standard validation. It returns nil when the template is valid, or an error
// carrying the OpenJD ValidationErrors when not.
func ValidateTemplate(rawTemplate string, format store.TemplateFormat, enforceLimits bool) error {
	pf := openjd.FormatYAML
	if format == store.TemplateFormatJSON {
		pf = openjd.FormatJSON
	}
	tmpl, err := openjd.Parse([]byte(rawTemplate), pf)
	if err != nil {
		return fmt.Errorf("product: template parse: %w", err)
	}
	if errs := openjd.ValidateWithOptions(tmpl, openjd.ValidateOptions{EnforceLimits: enforceLimits}); len(errs) > 0 {
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
	if err := ValidateTemplate(string(rawTemplate), store.TemplateFormatYAML, true); err != nil {
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
