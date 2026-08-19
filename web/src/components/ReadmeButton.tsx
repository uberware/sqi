// SPDX-License-Identifier: AGPL-3.0-or-later

import { Link } from 'react-router'
import IconButton from '@/components/IconButton'
import { Document } from '@/components/icons'

interface ReadmeButtonProps {
  /** Destination, normally a detail route carrying `?tab=readme`. */
  to: string
  /** Accessible name, e.g. "View readme for maya-render". */
  label: string
  /**
   * Whether the item is known to have a readme. False renders a disabled
   * control titled "No readme".
   *
   * The preset list passes true unconditionally: the remote library index
   * deliberately carries no readme, so a list row cannot know. The detail page
   * it links to then shows whatever is actually there.
   */
  hasReadme: boolean
  /**
   * Open in a new tab. Used from the submit flow's product picker, where a
   * same-tab navigation would abandon the job the user is in the middle of
   * starting.
   */
  newTab?: boolean
  className?: string | undefined
}

/**
 * Row/card affordance for jumping to an item's rendered readme.
 *
 * Renders a real link when a readme exists — so cmd/middle-click open it in a
 * new tab as with any link — and a disabled button when it does not, since an
 * anchor has no disabled state.
 */
export default function ReadmeButton({
  to,
  label,
  hasReadme,
  newTab = false,
  className,
}: ReadmeButtonProps) {
  if (!hasReadme) {
    return (
      <IconButton
        icon={<Document />}
        label={label}
        title="No readme"
        disabled
        className={className}
      />
    )
  }
  return (
    <Link
      to={to}
      className={className}
      aria-label={label}
      title="View readme"
      {...(newTab ? { target: '_blank', rel: 'noopener noreferrer' } : {})}
    >
      <Document />
    </Link>
  )
}
