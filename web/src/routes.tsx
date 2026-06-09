// SPDX-License-Identifier: AGPL-3.0-only

import { Routes, Route } from 'react-router-dom'
import Dashboard from '@/pages/Dashboard'
import JobList from '@/pages/JobList'
import JobDetail from '@/pages/JobDetail'
import TaskLogPage from '@/pages/TaskLogPage'
import WorkerList from '@/pages/WorkerList'
import WorkerDetail from '@/pages/WorkerDetail'
import Submit from '@/pages/Submit'
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
      <Route path="/submit" element={<Submit />} />
      <Route path="*" element={<NotFound />} />
    </Routes>
  )
}
