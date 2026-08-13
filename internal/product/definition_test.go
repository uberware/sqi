// SPDX-License-Identifier: AGPL-3.0-or-later

package product_test

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/product"
	"github.com/uberware/sqi/internal/store"
)

const goodTemplate = `specificationVersion: jobtemplate-2023-09
name: Demo
steps:
  - name: Run
    script:
      actions:
        onRun:
          command: echo
          args: ["hi"]`

func TestValidateTemplate(t *testing.T) {
	if err := product.ValidateTemplate(goodTemplate, store.TemplateFormatYAML, product.ValidateOptions{EnforceLimits: true}); err != nil {
		t.Fatalf("good template rejected: %v", err)
	}
	if err := product.ValidateTemplate("specificationVersion: wrong\nsteps: []", store.TemplateFormatYAML, product.ValidateOptions{EnforceLimits: true}); err == nil {
		t.Fatal("malformed template accepted")
	}
}

func TestParseDefinition(t *testing.T) {
	def := `name: python
title: Run a Python Script
description: Runs Python.
category: General
version: 1.0.0
template:
  specificationVersion: jobtemplate-2023-09
  name: Python
  steps:
    - name: Run
      script:
        actions:
          onRun:
            command: python3
            args: ["-c", "print(1)"]`

	p, err := product.ParseDefinition([]byte(def), product.ValidateOptions{EnforceLimits: true})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.Name != "python" || p.Title != "Run a Python Script" || p.Version != "1.0.0" {
		t.Fatalf("metadata: %+v", p)
	}
	if p.Format != store.TemplateFormatYAML {
		t.Fatalf("format: %q", p.Format)
	}
	if !strings.Contains(p.Template, "specificationVersion: jobtemplate-2023-09") {
		t.Fatalf("template not captured: %q", p.Template)
	}
	// The captured template must itself validate.
	if err := product.ValidateTemplate(p.Template, p.Format, product.ValidateOptions{EnforceLimits: true}); err != nil {
		t.Fatalf("captured template invalid: %v", err)
	}
}

func TestParseDefinition_Errors(t *testing.T) {
	cases := map[string]string{
		"missing name":     "title: X\ntemplate:\n  specificationVersion: jobtemplate-2023-09\n  name: Y\n  steps: []",
		"missing template": "name: x\ntitle: X",
		"bad slug":         "name: 'Bad Name'\ntitle: X\ntemplate:\n  specificationVersion: jobtemplate-2023-09\n  name: Y\n  steps: []",
		"bad template":     "name: x\ntitle: X\ntemplate:\n  specificationVersion: nope\n  steps: []",
	}
	for label, def := range cases {
		t.Run(label, func(t *testing.T) {
			if _, err := product.ParseDefinition([]byte(def), product.ValidateOptions{EnforceLimits: true}); err == nil {
				t.Fatalf("%s: expected error, got nil", label)
			}
		})
	}
}
