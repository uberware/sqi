// SPDX-License-Identifier: AGPL-3.0-only

import { useParams } from 'react-router-dom'
import PageHeader from '@/components/PageHeader'

export default function JobDetail() {
  const { id } = useParams<{ id: string }>()
  return (
    <div>
      <PageHeader title={`Job ${id ?? ''}`} />
    </div>
  )
}
