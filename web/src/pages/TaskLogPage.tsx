// SPDX-License-Identifier: AGPL-3.0-or-later

import { Link, useParams } from 'react-router-dom'
import PageHeader from '@/components/PageHeader'
import LogViewer from '@/components/LogViewer'
import { useGetTask, useGetJob } from '@/api/queries'
import styles from './TaskLogPage.module.css'

export default function TaskLogPage() {
  const { id: jobId, taskId } = useParams<{ id: string; taskId: string }>()
  const resolvedJobId = jobId ?? ''
  const resolvedTaskId = taskId ?? ''

  const { data: task, isLoading, isError } = useGetTask(resolvedTaskId)
  const { data: job } = useGetJob(resolvedJobId)

  // Header action area (far right): the back link, with the job name beneath it
  // in a bold, larger sans-serif, both right-justified.
  const headerActions = (
    <div className={styles.headerActions}>
      <Link to={`/jobs/${resolvedJobId}`} className={styles.backLink}>
        ← Back to job
      </Link>
      {job?.name !== undefined && <span className={styles.jobName}>{job.name}</span>}
    </div>
  )

  // Subtitle (sans-serif, beneath the display-font "Logs" header): the short
  // task id, then the task status for context once it is known.
  const taskIdLabel = resolvedTaskId ? `Task ${resolvedTaskId.slice(0, 8)}` : undefined
  const subtitleParts = [taskIdLabel, task?.status].filter((part): part is string => Boolean(part))
  const subtitle = subtitleParts.length > 0 ? subtitleParts.join(' · ') : undefined

  // LogViewer status: pending while the task loads, running on error (so live
  // WS pushes still arrive), otherwise the task's actual status.
  const viewerStatus = task?.status ?? (isLoading ? 'pending' : 'running')
  const showError = !isLoading && (isError || task === undefined)

  return (
    <div className={styles.page}>
      <PageHeader
        title="Logs"
        action={headerActions}
        actionAlign="end"
        {...(subtitle ? { subtitle } : {})}
      />
      {showError && (
        <p className={styles.error} role="alert">
          Could not load task details. Showing logs if available.
        </p>
      )}
      <div className={styles.viewerWrap}>
        <LogViewer taskId={resolvedTaskId} taskStatus={viewerStatus} />
      </div>
    </div>
  )
}
