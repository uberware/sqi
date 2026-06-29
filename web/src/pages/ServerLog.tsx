// SPDX-License-Identifier: AGPL-3.0-or-later

import { Link } from 'react-router-dom'
import DiagnosticsPanel from '@/components/DiagnosticsPanel'
import styles from './ServerLog.module.css'

export default function ServerLog() {
  return (
    <div className={styles.page}>
      <Link to="/admin" className={styles.back}>
        ← Admin
      </Link>
      <DiagnosticsPanel component="server" title="Server log" fill />
    </div>
  )
}
