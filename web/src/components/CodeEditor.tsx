// SPDX-License-Identifier: AGPL-3.0-or-later

import { useEffect, useLayoutEffect, useRef } from 'react'
import { EditorView, basicSetup } from 'codemirror'
import { EditorState } from '@codemirror/state'
import { json } from '@codemirror/lang-json'
import { yaml } from '@codemirror/lang-yaml'
import styles from './CodeEditor.module.css'

type Language = 'json' | 'yaml'

/** Infer JSON vs YAML from the first non-whitespace character. */
function detectLanguage(text: string): Language {
  const first = text.trimStart()[0]
  return first === '{' || first === '[' ? 'json' : 'yaml'
}

export interface CodeEditorProps {
  value: string
  onChange: (value: string) => void
  'aria-label'?: string
  'data-testid'?: string
}

/**
 * A CodeMirror 6 editor with YAML/JSON auto-detection, syntax highlighting,
 * line numbers, and basic bracket matching (all provided by basicSetup).
 * The language is re-detected on every render; the view is recreated only
 * when the detected language changes.
 */
export default function CodeEditor({
  value,
  onChange,
  'aria-label': ariaLabel,
  'data-testid': testId,
}: CodeEditorProps) {
  const containerRef = useRef<HTMLDivElement>(null)
  const viewRef = useRef<EditorView | null>(null)

  // Keep a stable ref to onChange so the EditorView listener never stales.
  const onChangeRef = useRef(onChange)
  useLayoutEffect(() => {
    onChangeRef.current = onChange
  })

  // Keep ariaLabel in a ref so the contentAttributes extension always reads
  // the latest value without needing to recreate the view.
  const ariaLabelRef = useRef(ariaLabel)
  useLayoutEffect(() => {
    ariaLabelRef.current = ariaLabel
  })

  const language = detectLanguage(value)

  // Recreate the editor when the detected language changes.
  useEffect(() => {
    const container = containerRef.current
    if (!container) return

    const langExtension = language === 'json' ? json() : yaml()

    // Preserve the cursor position across language-switch view recreations so
    // typing the first `{` does not jump the cursor back to offset 0.
    const prevAnchor = viewRef.current?.state.selection.main.head ?? 0

    const view = new EditorView({
      state: EditorState.create({
        doc: value,
        selection: { anchor: Math.min(prevAnchor, value.length) },
        extensions: [
          basicSetup,
          langExtension,
          // Thread aria-label through to the cm-content contenteditable element
          // so assistive technology can announce the field purpose.
          EditorView.contentAttributes.of(() => ({
            'aria-label': ariaLabelRef.current ?? '',
          })),
          EditorView.updateListener.of((update) => {
            if (update.docChanged) {
              onChangeRef.current(update.state.doc.toString())
            }
          }),
        ],
      }),
      parent: container,
    })

    viewRef.current = view
    return () => {
      view.destroy()
      viewRef.current = null
    }
    // Intentionally excludes `value` and `ariaLabel`: value is synced below;
    // ariaLabel is read dynamically via ref so we never need to recreate.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [language])

  // Sync value changes from outside (e.g. "Load example") without destroying
  // the view. Skip if the editor already holds this content — that would be the
  // normal case when the user is typing.
  useEffect(() => {
    const view = viewRef.current
    if (!view) return
    const current = view.state.doc.toString()
    if (current === value) return
    view.dispatch({
      changes: { from: 0, to: current.length, insert: value },
    })
  }, [value])

  return <div ref={containerRef} className={styles.editor} data-testid={testId} />
}
