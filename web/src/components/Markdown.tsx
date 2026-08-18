// SPDX-License-Identifier: AGPL-3.0-or-later
import { createElement, type ReactElement, type ReactNode } from 'react'
import styles from './Markdown.module.css'

/** Schemes a link may use. Everything else renders as plain text — readmes are
 * user-authored (products.manage, and auth is OFF by default) and preset
 * readmes come from a remote index, so this allowlist is the only control. */
const SAFE_SCHEMES = ['http:', 'https:', 'mailto:']

/** Returns href with its control and whitespace characters stripped when the
 * scheme is allowlisted, or null when it is not. Lowercasing is applied only
 * to the extracted scheme for the allowlist comparison, never to the
 * returned string -- the caller renders this return value directly, so the
 * string that is checked and the string that reaches the DOM are the same
 * value, not merely argued-equivalent. Whitespace and control characters are
 * stripped before matching: `java\tscript:` is a spelling browsers have
 * historically accepted, and a naive prefix test would miss it. */
function safeHref(href: string): string | null {
  // Strips every character at or below U+0020 -- space, tab, newline and
  // the C0 controls. A class such as /[ -]/ would strip spaces and hyphens
  // but leave the tab in `java\tscript:` intact, which is the case the
  // security test covers.
  // eslint-disable-next-line no-control-regex -- intentional: strip C0 controls, see comment above
  const stripped = href.replace(/[\u0000-\u0020]/g, '')
  const colon = stripped.indexOf(':')
  if (colon === -1) return null
  const scheme = stripped.slice(0, colon + 1).toLowerCase()
  return SAFE_SCHEMES.includes(scheme) ? stripped : null
}

const INLINE = /(\*\*[^*]+\*\*|\*[^*]+\*|_[^_]+_|`[^`]+`|\[[^\]]*\]\([^)\s]*\))/g

/** Parse inline spans within one line. Unmatched syntax falls through as
 * literal text by construction: anything the regex does not capture is emitted
 * verbatim, which is also why raw HTML shows up as visible text. */
function inline(text: string, keyPrefix: string): ReactNode[] {
  const out: ReactNode[] = []
  let last = 0
  let match: RegExpExecArray | null
  INLINE.lastIndex = 0
  while ((match = INLINE.exec(text)) !== null) {
    const token = match[0]
    if (match.index > last) out.push(text.slice(last, match.index))
    const key = `${keyPrefix}-${String(match.index)}`
    if (token.startsWith('**')) {
      out.push(<strong key={key}>{token.slice(2, -2)}</strong>)
    } else if (token.startsWith('`')) {
      out.push(<code key={key}>{token.slice(1, -1)}</code>)
    } else if (token.startsWith('[')) {
      const split = token.indexOf('](')
      const label = token.slice(1, split)
      const href = token.slice(split + 2, -1)
      // An image is a '!' followed by link syntax. The '!' has already been
      // emitted as text, so emitting the token as text too degrades the whole
      // construct to literal characters rather than to a link labelled `alt`.
      const isImage = match.index > 0 && text[match.index - 1] === '!'
      const safe = isImage ? null : safeHref(href)
      if (safe !== null) {
        out.push(
          <a key={key} href={safe} target="_blank" rel="noopener noreferrer">
            {label}
          </a>,
        )
      } else {
        out.push(token)
      }
    } else {
      out.push(<em key={key}>{token.slice(1, -1)}</em>)
    }
    last = match.index + token.length
  }
  if (last < text.length) out.push(text.slice(last))
  return out
}

/** Heading tag for N leading '#', offset so readme headings nest under the
 * page's existing h1 (PageHeader) and h2 (product/preset title), clamped at h6. */
function headingTag(hashes: number): 'h3' | 'h4' | 'h5' | 'h6' {
  const tags = ['h3', 'h4', 'h5', 'h6'] as const
  return tags[Math.min(hashes - 1, tags.length - 1)] ?? 'h6'
}

/** Render a restricted Markdown subset as React elements.
 *
 * Supported: paragraphs, unordered and ordered lists, fenced code blocks, ATX
 * headings, and the inline spans in INLINE. Images, tables, blockquotes,
 * reference links and raw HTML are NOT supported and degrade to literal text.
 * This renderer never sets raw HTML on the DOM, deliberately — see the test. */
export default function Markdown({ source }: { source: string }): ReactElement {
  const lines = source.split('\n')
  const blocks: ReactNode[] = []
  let para: string[] = []
  let list: { ordered: boolean; items: string[] } | null = null

  const flushPara = (): void => {
    if (para.length === 0) return
    const key = `p${String(blocks.length)}`
    blocks.push(<p key={key}>{inline(para.join(' '), key)}</p>)
    para = []
  }
  const flushList = (): void => {
    if (list === null) return
    const { ordered, items } = list
    const key = `l${String(blocks.length)}`
    const children = items.map((item, i) => (
      <li key={`${key}-${String(i)}`}>{inline(item, `${key}-${String(i)}`)}</li>
    ))
    blocks.push(ordered ? <ol key={key}>{children}</ol> : <ul key={key}>{children}</ul>)
    list = null
  }

  for (let i = 0; i < lines.length; i++) {
    const line = lines[i] ?? ''

    if (line.startsWith('```')) {
      flushPara()
      flushList()
      const body: string[] = []
      i++
      while (i < lines.length && !(lines[i] ?? '').startsWith('```')) {
        body.push(lines[i] ?? '')
        i++
      }
      blocks.push(
        <pre key={`c${String(blocks.length)}`}>
          <code>{body.join('\n')}</code>
        </pre>,
      )
      continue
    }

    const heading = /^(#{1,6})\s+(.*)$/.exec(line)
    if (heading) {
      flushPara()
      flushList()
      const Tag = headingTag((heading[1] ?? '').length)
      const key = `h${String(blocks.length)}`
      // createElement rather than JSX: a capitalized JSX tag bound to a
      // variable reads to the linter as a component created during render,
      // even though Tag is always one of the intrinsic strings 'h3'..'h6'.
      blocks.push(createElement(Tag, { key }, inline(heading[2] ?? '', key)))
      continue
    }

    const bullet = /^[-*]\s+(.*)$/.exec(line)
    const numbered = /^\d+\.\s+(.*)$/.exec(line)
    if (bullet !== null || numbered !== null) {
      flushPara()
      const ordered = numbered !== null
      const item = (numbered ?? bullet)?.[1] ?? ''
      if (list === null || list.ordered !== ordered) {
        flushList()
        list = { ordered, items: [] }
      }
      list.items.push(item)
      continue
    }

    if (line.trim() === '') {
      flushPara()
      flushList()
      continue
    }
    flushList()
    para.push(line)
  }
  flushPara()
  flushList()

  return <div className={styles.markdown}>{blocks}</div>
}
