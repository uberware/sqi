// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import SubmitPausedField from './SubmitPausedField'

describe('SubmitPausedField', () => {
  it('reports a tick through onChange', async () => {
    const onChange = vi.fn()
    render(<SubmitPausedField checked={false} onChange={onChange} />)
    await userEvent.setup().click(screen.getByRole('checkbox', { name: /submit paused/i }))
    expect(onChange).toHaveBeenCalledWith(true)
  })

  it('reflects the checked prop', () => {
    render(<SubmitPausedField checked onChange={() => undefined} />)
    expect(screen.getByRole('checkbox', { name: /submit paused/i })).toBeChecked()
  })

  it('explains that nothing runs until the job is resumed', () => {
    render(<SubmitPausedField checked={false} onChange={() => undefined} />)
    expect(screen.getByRole('checkbox', { name: /submit paused/i })).toHaveAccessibleDescription(
      /nothing runs until you resume the job/i,
    )
  })
})
