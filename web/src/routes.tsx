// SPDX-License-Identifier: AGPL-3.0-or-later

import { Routes, Route } from 'react-router-dom'
import Dashboard from '@/pages/Dashboard'
import JobList from '@/pages/JobList'
import JobDetail from '@/pages/JobDetail'
import TaskLogPage from '@/pages/TaskLogPage'
import WorkerList from '@/pages/WorkerList'
import WorkerDetail from '@/pages/WorkerDetail'
import FarmList from '@/pages/FarmList'
import FarmForm from '@/pages/FarmForm'
import QueueList from '@/pages/QueueList'
import QueueForm from '@/pages/QueueForm'
import UsagePoolList from '@/pages/UsagePoolList'
import UsagePoolForm from '@/pages/UsagePoolForm'
import StorageLocationList from '@/pages/StorageLocationList'
import StorageLocationForm from '@/pages/StorageLocationForm'
import ComputeLocationList from '@/pages/ComputeLocationList'
import ComputeLocationForm from '@/pages/ComputeLocationForm'
import Submit from '@/pages/Submit'
import Admin from '@/pages/Admin'
import NotFound from '@/pages/NotFound'

export default function AppRoutes() {
  return (
    <Routes>
      <Route path="/" element={<Dashboard />} />
      <Route path="/jobs" element={<JobList />} />
      <Route path="/jobs/:id" element={<JobDetail />} />
      <Route path="/jobs/:id/tasks/:taskId/logs" element={<TaskLogPage />} />
      <Route path="/workers" element={<WorkerList />} />
      <Route path="/workers/:id" element={<WorkerDetail />} />
      <Route path="/farms" element={<FarmList />} />
      <Route path="/farms/new" element={<FarmForm mode="create" />} />
      <Route path="/farms/:id/edit" element={<FarmForm mode="edit" />} />
      <Route path="/queues" element={<QueueList />} />
      <Route path="/queues/new" element={<QueueForm mode="create" />} />
      <Route path="/queues/:id/edit" element={<QueueForm mode="edit" />} />
      <Route path="/usage-pools" element={<UsagePoolList />} />
      <Route path="/usage-pools/new" element={<UsagePoolForm mode="create" />} />
      <Route path="/usage-pools/:id/edit" element={<UsagePoolForm mode="edit" />} />
      <Route path="/storage-locations" element={<StorageLocationList />} />
      <Route path="/storage-locations/new" element={<StorageLocationForm mode="create" />} />
      <Route path="/storage-locations/:id/edit" element={<StorageLocationForm mode="edit" />} />
      <Route path="/compute-locations" element={<ComputeLocationList />} />
      <Route path="/compute-locations/new" element={<ComputeLocationForm mode="create" />} />
      <Route path="/compute-locations/:id/edit" element={<ComputeLocationForm mode="edit" />} />
      <Route path="/submit" element={<Submit />} />
      <Route path="/admin" element={<Admin />} />
      <Route path="*" element={<NotFound />} />
    </Routes>
  )
}
