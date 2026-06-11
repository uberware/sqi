// SPDX-License-Identifier: AGPL-3.0-or-later

import { Link, useParams } from 'react-router-dom'
import PageHeader from '@/components/PageHeader'
import LogViewer from '@/components/LogViewer'
import { useGetTask } from '@/api/queries'
import styles from './TaskLogPage.module.css'

export default function TaskLogPage() {
  const { id: jobId, taskId } = useParams<{ id: string; taskId: string }>()
  const resolvedJobId = jobId ?? ''
  const resolvedTaskId = taskId ?? ''

  const { data: task, isLoading, isError } = useGetTask(resolvedTaskId)

  const backLink = (
    <Link to={`/jobs/${resolvedJobId}`} className={styles.backLink}>
      ← Back to job
    </Link>
  )

  if (isLoading) {
    return (
      <div className={styles.page}>
        <PageHeader title="Task Logs" subtitle={resolvedTaskId} action={backLink} />
        <div className={styles.viewerWrap}>
          <LogViewer taskId={resolvedTaskId} taskStatus="pending" />
        </div>
      </div>
    )
  }

  if (isError || task === undefined) {
    return (
      <div className={styles.page}>
        <PageHeader title="Task Logs" subtitle={resolvedTaskId} action={backLink} />
        <p className={styles.error} role="alert">
          Could not load task details. Showing logs if available.
        </p>
        <div className={styles.viewerWrap}>
          {/* Err toward live so WS pushes still arrive if the task is running. */}
          <LogViewer taskId={resolvedTaskId} taskStatus="running" />
        </div>
      </div>
    )
  }

  return (
    <div className={styles.page}>
      <PageHeader
        title={`Logs: ${task.name}`}
        subtitle={`Task ${resolvedTaskId.slice(0, 8)} · ${task.status}`}
        action={backLink}
      />
      <div className={styles.viewerWrap}>
        <LogViewer taskId={resolvedTaskId} taskStatus={task.status} />
      </div>
    </div>
  )
}
