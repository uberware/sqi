// SPDX-License-Identifier: AGPL-3.0-or-later

import { Link } from 'react-router-dom'
import styles from './NotFound.module.css'

export default function NotFound() {
  return (
    <div className={styles.container}>
      <h1 className={styles.heading}>Page not found</h1>
      <p className={styles.message}>The page you&rsquo;re looking for doesn&rsquo;t exist.</p>
      <Link to="/" className={styles.link}>
        Back to dashboard
      </Link>
    </div>
  )
}
