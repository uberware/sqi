// SPDX-License-Identifier: AGPL-3.0-or-later

import PageHeader from '@/components/PageHeader'
import DiagnosticsPanel from '@/components/DiagnosticsPanel'
import styles from './ServerLog.module.css'

export default function ServerLog() {
  return (
    <div className={styles.page}>
      <PageHeader title="Server Log" backTo="/admin" />
      <DiagnosticsPanel component="server" title="Server log" showTitle={false} fill />
    </div>
  )
}
