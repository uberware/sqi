// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import Tabs from './Tabs'

const TABS = [
  { id: 'readme', label: 'Readme' },
  { id: 'template', label: 'OpenJD Template' },
]

describe('Tabs', () => {
  it('exposes a tablist whose tabs are named by their labels', () => {
    render(
      <Tabs tabs={TABS} active="readme" onChange={vi.fn()} label="Product sections">
        <p>panel body</p>
      </Tabs>,
    )
    expect(screen.getByRole('tablist', { name: 'Product sections' })).toBeInTheDocument()
    expect(screen.getByRole('tab', { name: 'Readme' })).toBeInTheDocument()
    expect(screen.getByRole('tab', { name: 'OpenJD Template' })).toBeInTheDocument()
  })

  it('marks only the active tab as selected', () => {
    render(
      <Tabs tabs={TABS} active="template" onChange={vi.fn()} label="Product sections">
        <p>panel body</p>
      </Tabs>,
    )
    expect(screen.getByRole('tab', { name: 'Readme' })).toHaveAttribute('aria-selected', 'false')
    expect(screen.getByRole('tab', { name: 'OpenJD Template' })).toHaveAttribute(
      'aria-selected',
      'true',
    )
  })

  it('renders children in a tabpanel labelled by the active tab', () => {
    render(
      <Tabs tabs={TABS} active="readme" onChange={vi.fn()} label="Product sections">
        <p>panel body</p>
      </Tabs>,
    )
    const panel = screen.getByRole('tabpanel', { name: 'Readme' })
    expect(panel).toHaveTextContent('panel body')
  })

  it('calls onChange with the tab id when a tab is clicked', async () => {
    const user = userEvent.setup()
    const onChange = vi.fn()
    render(
      <Tabs tabs={TABS} active="readme" onChange={onChange} label="Product sections">
        <p>panel body</p>
      </Tabs>,
    )
    await user.click(screen.getByRole('tab', { name: 'OpenJD Template' }))
    expect(onChange).toHaveBeenCalledWith('template')
  })

  // Roving tabindex: only the active tab is reachable by Tab, and the arrow
  // keys move between tabs. Without this a keyboard user has to step through
  // every tab to reach the panel.
  it('keeps only the active tab in the tab order', () => {
    render(
      <Tabs tabs={TABS} active="readme" onChange={vi.fn()} label="Product sections">
        <p>panel body</p>
      </Tabs>,
    )
    expect(screen.getByRole('tab', { name: 'Readme' })).toHaveAttribute('tabindex', '0')
    expect(screen.getByRole('tab', { name: 'OpenJD Template' })).toHaveAttribute('tabindex', '-1')
  })

  it('moves to the next tab on ArrowRight', async () => {
    const user = userEvent.setup()
    const onChange = vi.fn()
    render(
      <Tabs tabs={TABS} active="readme" onChange={onChange} label="Product sections">
        <p>panel body</p>
      </Tabs>,
    )
    screen.getByRole('tab', { name: 'Readme' }).focus()
    await user.keyboard('{ArrowRight}')
    expect(onChange).toHaveBeenCalledWith('template')
  })

  it('wraps from the last tab to the first on ArrowRight', async () => {
    const user = userEvent.setup()
    const onChange = vi.fn()
    render(
      <Tabs tabs={TABS} active="template" onChange={onChange} label="Product sections">
        <p>panel body</p>
      </Tabs>,
    )
    screen.getByRole('tab', { name: 'OpenJD Template' }).focus()
    await user.keyboard('{ArrowRight}')
    expect(onChange).toHaveBeenCalledWith('readme')
  })

  it('moves to the previous tab on ArrowLeft', async () => {
    const user = userEvent.setup()
    const onChange = vi.fn()
    render(
      <Tabs tabs={TABS} active="template" onChange={onChange} label="Product sections">
        <p>panel body</p>
      </Tabs>,
    )
    screen.getByRole('tab', { name: 'OpenJD Template' }).focus()
    await user.keyboard('{ArrowLeft}')
    expect(onChange).toHaveBeenCalledWith('readme')
  })

  it('jumps to the first tab on Home and the last on End', async () => {
    const user = userEvent.setup()
    const onChange = vi.fn()
    render(
      <Tabs tabs={TABS} active="template" onChange={onChange} label="Product sections">
        <p>panel body</p>
      </Tabs>,
    )
    screen.getByRole('tab', { name: 'OpenJD Template' }).focus()
    await user.keyboard('{Home}')
    expect(onChange).toHaveBeenLastCalledWith('readme')
    await user.keyboard('{End}')
    expect(onChange).toHaveBeenLastCalledWith('template')
  })

  describe('disabled tabs', () => {
    const WITH_DISABLED = [
      { id: 'readme', label: 'Readme', disabled: true },
      { id: 'template', label: 'OpenJD Template' },
    ]

    it('marks a disabled tab as disabled', () => {
      render(
        <Tabs tabs={WITH_DISABLED} active="template" onChange={vi.fn()} label="Product sections">
          <p>panel body</p>
        </Tabs>,
      )
      expect(screen.getByRole('tab', { name: 'Readme' })).toBeDisabled()
    })

    it('does not call onChange when a disabled tab is clicked', async () => {
      const user = userEvent.setup()
      const onChange = vi.fn()
      render(
        <Tabs tabs={WITH_DISABLED} active="template" onChange={onChange} label="Product sections">
          <p>panel body</p>
        </Tabs>,
      )
      await user.click(screen.getByRole('tab', { name: 'Readme' }))
      expect(onChange).not.toHaveBeenCalled()
    })

    // Arrow keys must step OVER a disabled tab rather than landing on it,
    // otherwise the keyboard user gets stuck on a tab that cannot activate.
    it('skips a disabled tab when arrowing', async () => {
      const user = userEvent.setup()
      const onChange = vi.fn()
      const three = [
        { id: 'a', label: 'A' },
        { id: 'b', label: 'B', disabled: true },
        { id: 'c', label: 'C' },
      ]
      render(
        <Tabs tabs={three} active="a" onChange={onChange} label="Sections">
          <p>panel body</p>
        </Tabs>,
      )
      screen.getByRole('tab', { name: 'A' }).focus()
      await user.keyboard('{ArrowRight}')
      expect(onChange).toHaveBeenCalledWith('c')
    })
  })
})
