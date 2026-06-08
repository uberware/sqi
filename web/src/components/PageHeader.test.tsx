import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import PageHeader from '@/components/PageHeader'

describe('PageHeader', () => {
  it('renders the title as a level-1 heading', () => {
    render(<PageHeader title="Farm Overview" />)
    expect(screen.getByRole('heading', { level: 1, name: 'Farm Overview' })).toBeInTheDocument()
  })

  it('does not render a subtitle paragraph when subtitle is omitted', () => {
    const { container } = render(<PageHeader title="Jobs" />)
    expect(container.querySelector('p')).toBeNull()
  })

  it('renders the subtitle when provided', () => {
    render(<PageHeader title="Jobs" subtitle="All submitted jobs" />)
    expect(screen.getByText('All submitted jobs')).toBeInTheDocument()
  })

  it('does not render an action slot when action is omitted', () => {
    const { container } = render(<PageHeader title="Workers" />)
    expect(container.querySelector('button')).toBeNull()
  })

  it('renders the action slot when provided', () => {
    render(<PageHeader title="Workers" action={<button>Disable all</button>} />)
    expect(screen.getByRole('button', { name: 'Disable all' })).toBeInTheDocument()
  })

  it('renders both subtitle and action together', () => {
    render(
      <PageHeader
        title="Submit Job"
        subtitle="Submit a raw OpenJD template"
        action={<button>Load example</button>}
      />,
    )
    expect(screen.getByRole('heading', { level: 1, name: 'Submit Job' })).toBeInTheDocument()
    expect(screen.getByText('Submit a raw OpenJD template')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Load example' })).toBeInTheDocument()
  })
})
