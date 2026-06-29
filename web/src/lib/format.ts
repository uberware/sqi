// SPDX-License-Identifier: AGPL-3.0-or-later

import type { TemplateFormat } from '@/api/types'

/**
 * Infer JSON vs YAML from the first non-whitespace character of a template.
 * An OpenJD template is always a top-level object, so a leading `{`/`[` means
 * JSON and anything else is YAML.
 */
export function detectFormat(template: string): TemplateFormat {
  const first = template.trimStart()[0]
  return first === '{' || first === '[' ? 'json' : 'yaml'
}
