// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import CodeEditor from './CodeEditor'

describe('CodeEditor', () => {
  it('mounts the editor container and forwards the data-testid prop', () => {
    const onChange = vi.fn()
    render(<CodeEditor value="key: value" onChange={onChange} data-testid="cm-editor" />)
    expect(screen.getByTestId('cm-editor')).toBeInTheDocument()
  })

  it('accepts a JSON value without throwing', () => {
    const onChange = vi.fn()
    expect(() => render(<CodeEditor value='{"name": "test"}' onChange={onChange} />)).not.toThrow()
  })
})
