// SPDX-License-Identifier: AGPL-3.0-or-later

import { Routes, Route } from 'react-router-dom'
import RequireRole from '@/components/RequireRole'
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
import UserList from '@/pages/UserList'
import UserForm from '@/pages/UserForm'
import ApiKeyList from '@/pages/ApiKeyList'
import Account from '@/pages/Account'
import UserApiKeys from '@/pages/UserApiKeys'
import PresetLibrary from '@/pages/PresetLibrary'
import PresetDetail from '@/pages/PresetDetail'
import ProductList from '@/pages/ProductList'
import ProductDetail from '@/pages/ProductDetail'
import ProductForm from '@/pages/ProductForm'
import ProductPicker from '@/pages/ProductPicker'
import ProductSubmit from '@/pages/ProductSubmit'
import Submit from '@/pages/Submit'
import Admin from '@/pages/Admin'
import ServerLog from '@/pages/ServerLog'
import NotFound from '@/pages/NotFound'

export default function AppRoutes() {
  return (
    <Routes>
      <Route path="/" element={<Dashboard />} />
      {/* Ungated: every authenticated role may manage its own account. */}
      <Route path="/account" element={<Account />} />
      <Route path="/jobs" element={<JobList />} />
      <Route path="/jobs/:id" element={<JobDetail />} />
      <Route path="/jobs/:id/tasks/:taskId/logs" element={<TaskLogPage />} />
      <Route path="/workers" element={<WorkerList />} />
      <Route path="/workers/:id" element={<WorkerDetail />} />
      <Route path="/farms" element={<FarmList />} />
      <Route
        path="/farms/new"
        element={
          <RequireRole permission="infra.manage">
            <FarmForm mode="create" />
          </RequireRole>
        }
      />
      <Route
        path="/farms/:id/edit"
        element={
          <RequireRole permission="infra.manage">
            <FarmForm mode="edit" />
          </RequireRole>
        }
      />
      <Route path="/queues" element={<QueueList />} />
      <Route
        path="/queues/new"
        element={
          <RequireRole permission="infra.manage">
            <QueueForm mode="create" />
          </RequireRole>
        }
      />
      <Route
        path="/queues/:id/edit"
        element={
          <RequireRole permission="infra.manage">
            <QueueForm mode="edit" />
          </RequireRole>
        }
      />
      <Route path="/usage-pools" element={<UsagePoolList />} />
      <Route
        path="/usage-pools/new"
        element={
          <RequireRole permission="infra.manage">
            <UsagePoolForm mode="create" />
          </RequireRole>
        }
      />
      <Route
        path="/usage-pools/:id/edit"
        element={
          <RequireRole permission="infra.manage">
            <UsagePoolForm mode="edit" />
          </RequireRole>
        }
      />
      <Route path="/storage-locations" element={<StorageLocationList />} />
      <Route
        path="/storage-locations/new"
        element={
          <RequireRole permission="infra.manage">
            <StorageLocationForm mode="create" />
          </RequireRole>
        }
      />
      <Route
        path="/storage-locations/:id/edit"
        element={
          <RequireRole permission="infra.manage">
            <StorageLocationForm mode="edit" />
          </RequireRole>
        }
      />
      <Route path="/compute-locations" element={<ComputeLocationList />} />
      <Route
        path="/compute-locations/new"
        element={
          <RequireRole permission="infra.manage">
            <ComputeLocationForm mode="create" />
          </RequireRole>
        }
      />
      <Route
        path="/compute-locations/:id/edit"
        element={
          <RequireRole permission="infra.manage">
            <ComputeLocationForm mode="edit" />
          </RequireRole>
        }
      />
      <Route
        path="/users"
        element={
          <RequireRole permission="users.read">
            <UserList />
          </RequireRole>
        }
      />
      <Route
        path="/users/new"
        element={
          <RequireRole permission="users.manage">
            <UserForm mode="create" />
          </RequireRole>
        }
      />
      <Route
        path="/users/:id/edit"
        element={
          <RequireRole permission="users.manage">
            <UserForm mode="edit" />
          </RequireRole>
        }
      />
      <Route
        path="/users/:id/api-keys"
        element={
          <RequireRole permission="apikeys.admin">
            <UserApiKeys />
          </RequireRole>
        }
      />
      <Route path="/api-keys" element={<ApiKeyList />} />
      <Route
        path="/submit"
        element={
          <RequireRole permission="jobs.write">
            <ProductPicker />
          </RequireRole>
        }
      />
      <Route
        path="/submit/raw"
        element={
          <RequireRole permission="jobs.write">
            <Submit />
          </RequireRole>
        }
      />
      <Route
        path="/submit/product/:name"
        element={
          <RequireRole permission="jobs.write">
            <ProductSubmit />
          </RequireRole>
        }
      />
      <Route path="/admin" element={<Admin />} />
      <Route path="/presets" element={<PresetLibrary />} />
      <Route path="/presets/:name" element={<PresetDetail />} />
      <Route path="/products" element={<ProductList />} />
      <Route
        path="/products/new"
        element={
          <RequireRole permission="products.manage">
            <ProductForm mode="create" />
          </RequireRole>
        }
      />
      <Route
        path="/products/:name/edit"
        element={
          <RequireRole permission="products.manage">
            <ProductForm mode="edit" />
          </RequireRole>
        }
      />
      <Route path="/products/:name" element={<ProductDetail />} />
      <Route
        path="/server-log"
        element={
          <RequireRole permission="diagnostics.read">
            <ServerLog />
          </RequireRole>
        }
      />
      <Route path="*" element={<NotFound />} />
    </Routes>
  )
}
