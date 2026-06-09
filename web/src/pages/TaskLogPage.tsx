// SPDX-License-Identifier: AGPL-3.0-only

import { useParams, Link } from 'react-router-dom'
import PageHeader from '@/components/PageHeader'

/**
 * Placeholder route for task log viewing.
 * Full implementation is covered by tasks 49–56 (log viewer section).
 */
export default function TaskLogPage() {
  const { id: jobId, taskId } = useParams<{ id: string; taskId: string }>()

  return (
    <div>
      <PageHeader
        title="Task Logs"
        subtitle={taskId ?? ''}
        action={
          <Link to={`/jobs/${jobId ?? ''}`} style={{ fontSize: '0.875rem' }}>
            ← Back to job
          </Link>
        }
      />
      <p style={{ padding: '2rem', color: 'var(--color-text-muted)', fontSize: '0.875rem' }}>
        Log viewer — coming in tasks 49–56.
      </p>
    </div>
  )
}
