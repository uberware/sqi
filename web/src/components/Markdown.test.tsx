// SPDX-License-Identifier: AGPL-3.0-or-later
import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import Markdown from './Markdown'

describe('Markdown', () => {
  it('renders paragraphs split on blank lines', () => {
    render(<Markdown source={'First para.\n\nSecond para.'} />)
    expect(screen.getByText('First para.')).toBeInTheDocument()
    expect(screen.getByText('Second para.')).toBeInTheDocument()
  })

  it('renders inline bold, italic and code', () => {
    render(<Markdown source={'a **bold** and *italic* and `code` here'} />)
    expect(screen.getByText('bold').tagName).toBe('STRONG')
    expect(screen.getByText('italic').tagName).toBe('EM')
    expect(screen.getByText('code').tagName).toBe('CODE')
  })

  it('renders unordered lists', () => {
    render(<Markdown source={'- one\n- two'} />)
    expect(screen.getAllByRole('listitem')).toHaveLength(2)
  })

  it('renders ordered lists', () => {
    const { container } = render(<Markdown source={'1. one\n2. two'} />)
    expect(container.querySelector('ol')).not.toBeNull()
    expect(screen.getAllByRole('listitem')).toHaveLength(2)
  })

  it('renders fenced code blocks verbatim, without inline parsing', () => {
    render(<Markdown source={'```\nnot **bold** here\n```'} />)
    expect(screen.getByText(/not \*\*bold\*\* here/)).toBeInTheDocument()
    expect(screen.queryByText('bold')).not.toBeInTheDocument()
  })

  // Both detail pages nest h1 (PageHeader) -> h2 (product/preset title) ->
  // readme, so readme headings start at h3 to keep the document outline valid.
  it('offsets headings so the outline stays correct', () => {
    render(<Markdown source={'# One\n\n## Two\n\n### Three\n\n#### Four\n\n##### Five'} />)
    expect(screen.getByText('One').tagName).toBe('H3')
    expect(screen.getByText('Two').tagName).toBe('H4')
    expect(screen.getByText('Three').tagName).toBe('H5')
    expect(screen.getByText('Four').tagName).toBe('H6')
    expect(screen.getByText('Five').tagName).toBe('H6')
  })

  it('renders http and mailto links', () => {
    render(<Markdown source={'[docs](https://example.com/x) [mail](mailto:a@b.c)'} />)
    expect(screen.getByRole('link', { name: 'docs' })).toHaveAttribute(
      'href',
      'https://example.com/x',
    )
    expect(screen.getByRole('link', { name: 'mail' })).toHaveAttribute('href', 'mailto:a@b.c')
  })

  it('gives external links rel="noopener noreferrer"', () => {
    render(<Markdown source={'[docs](https://example.com/)'} />)
    expect(screen.getByRole('link', { name: 'docs' })).toHaveAttribute('rel', 'noopener noreferrer')
  })

  it('degrades unsupported syntax to literal text', () => {
    render(<Markdown source={'| a | b |'} />)
    expect(screen.getByText(/\| a \| b \|/)).toBeInTheDocument()
    expect(screen.queryByRole('table')).not.toBeInTheDocument()
  })

  describe('security', () => {
    it.each([
      ['javascript:alert(1)'],
      ['JaVaScRiPt:alert(1)'],
      ['  javascript:alert(1)'],
      ['java\tscript:alert(1)'],
      ['data:text/html,<script>alert(1)</script>'],
      ['vbscript:msgbox(1)'],
    ])('renders %s as text, not a link', (href) => {
      render(<Markdown source={`[click](${href})`} />)
      expect(screen.queryByRole('link')).not.toBeInTheDocument()
      expect(screen.getByText(/click/)).toBeInTheDocument()
    })

    it('renders raw HTML as visible text', () => {
      const { container } = render(
        <Markdown source={'<script>alert(1)</script> and <img src=x onerror=alert(1)>'} />,
      )
      expect(container.querySelector('script')).toBeNull()
      expect(container.querySelector('img')).toBeNull()
      expect(screen.getByText(/<script>alert\(1\)<\/script>/)).toBeInTheDocument()
    })

    // Asserting only "no <img>" is not enough: the inline matcher sees the
    // [alt](url) half of the image and would happily render a LINK to the
    // beacon, which leaks the viewer's IP just as effectively on click.
    it('renders images as literal text, never as an img or a link', () => {
      const { container } = render(<Markdown source={'![alt](https://evil.test/beacon.png)'} />)
      expect(container.querySelector('img')).toBeNull()
      expect(screen.queryByRole('link')).not.toBeInTheDocument()
      expect(screen.getByText(/!\[alt\]\(https:\/\/evil\.test\/beacon\.png\)/)).toBeInTheDocument()
    })
  })

  // A structural guarantee, not a style rule: because the renderer only ever
  // returns React elements, a parser bug can produce wrong FORMATTING but never
  // script execution. That property is worth failing a test over.
  it('never uses dangerouslySetInnerHTML', async () => {
    const src = await import('./Markdown.tsx?raw')
    expect(src.default).not.toContain('dangerouslySetInnerHTML')
  })
})
