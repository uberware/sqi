// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi } from 'vitest'
import { render, screen, fireEvent } from '@testing-library/react'
import RetryPolicyFields from './RetryPolicyFields'

function renderFields(overrides: Partial<Parameters<typeof RetryPolicyFields>[0]> = {}) {
  const onMaxAttemptsChange = vi.fn()
  const onRetryDelaySecondsChange = vi.fn()
  const onFailureLimitChange = vi.fn()
  render(
    <RetryPolicyFields
      maxAttempts=""
      onMaxAttemptsChange={onMaxAttemptsChange}
      retryDelaySeconds=""
      onRetryDelaySecondsChange={onRetryDelaySecondsChange}
      failureLimit=""
      onFailureLimitChange={onFailureLimitChange}
      placeholder="Inherit"
      {...overrides}
    />,
  )
  return { onMaxAttemptsChange, onRetryDelaySecondsChange, onFailureLimitChange }
}

describe('RetryPolicyFields', () => {
  it('renders the three inputs with full labels by default', () => {
    renderFields()
    expect(screen.getByLabelText('Max attempts per task')).toBeInTheDocument()
    expect(screen.getByLabelText('Retry delay (seconds)')).toBeInTheDocument()
    expect(screen.getByLabelText('Failure limit (auto-park after N failures)')).toBeInTheDocument()
  })

  it('renders compact labels with labelVariant="short"', () => {
    renderFields({ labelVariant: 'short' })
    expect(screen.getByLabelText('Max attempts')).toBeInTheDocument()
    expect(screen.getByLabelText('Retry delay (seconds)')).toBeInTheDocument()
    expect(screen.getByLabelText('Failure limit')).toBeInTheDocument()
  })

  it('keeps the stable input ids used by the host forms', () => {
    renderFields()
    expect(screen.getByLabelText('Max attempts per task')).toHaveAttribute('id', 'maxAttempts')
    expect(screen.getByLabelText('Retry delay (seconds)')).toHaveAttribute(
      'id',
      'retryDelaySeconds',
    )
    expect(screen.getByLabelText('Failure limit (auto-park after N failures)')).toHaveAttribute(
      'id',
      'failureLimit',
    )
  })

  it('applies the shared placeholder and per-field min attributes', () => {
    renderFields({ placeholder: 'Inherit farm default' })
    const maxAttempts = screen.getByLabelText('Max attempts per task')
    const retryDelay = screen.getByLabelText('Retry delay (seconds)')
    const failureLimit = screen.getByLabelText('Failure limit (auto-park after N failures)')
    for (const input of [maxAttempts, retryDelay, failureLimit]) {
      expect(input).toHaveAttribute('placeholder', 'Inherit farm default')
      expect(input).toHaveAttribute('type', 'number')
    }
    expect(maxAttempts).toHaveAttribute('min', '1')
    expect(retryDelay).toHaveAttribute('min', '0')
    // 0 must be enterable: an explicit 0 disables the inherited failure limit.
    expect(failureLimit).toHaveAttribute('min', '0')
  })

  it('shows the provided values', () => {
    renderFields({ maxAttempts: '5', retryDelaySeconds: '30', failureLimit: '10' })
    expect(screen.getByLabelText('Max attempts per task')).toHaveValue(5)
    expect(screen.getByLabelText('Retry delay (seconds)')).toHaveValue(30)
    expect(screen.getByLabelText('Failure limit (auto-park after N failures)')).toHaveValue(10)
  })

  it('routes each input change to its own handler', () => {
    const handlers = renderFields()
    fireEvent.change(screen.getByLabelText('Max attempts per task'), { target: { value: '3' } })
    fireEvent.change(screen.getByLabelText('Retry delay (seconds)'), { target: { value: '15' } })
    fireEvent.change(screen.getByLabelText('Failure limit (auto-park after N failures)'), {
      target: { value: '8' },
    })
    expect(handlers.onMaxAttemptsChange).toHaveBeenCalledWith('3')
    expect(handlers.onRetryDelaySecondsChange).toHaveBeenCalledWith('15')
    expect(handlers.onFailureLimitChange).toHaveBeenCalledWith('8')
  })
})
