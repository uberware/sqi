// SPDX-License-Identifier: AGPL-3.0-or-later

import { useQuery } from '@tanstack/react-query'
import { apiFetch } from './client'
import type {
  Farm,
  Job,
  JobDetail,
  Product,
  StorageLocation,
  UsagePool,
  ListResponse,
  Queue,
  Task,
  TaskLogsResponse,
  VersionInfo,
  Worker,
  WorkerDetail,
} from './types'

/**
 * How often (ms) the usage-pool list refetches so utilization (in_use /
 * available) stays current while a page is open. Polling keeps the data path
 * simple; a generic WebSocket limit-usage channel can replace this later.
 */
const USAGE_POOL_REFETCH_MS = 5000

/**
 * How often (ms) the worker-detail query refetches while the page is open. The
 * worker's `current_tasks` are derived server-side from its assigned/running
 * tasks, but no WebSocket event fires when those assignments change (the
 * `workers` subject only carries online/offline transitions). Polling keeps the
 * Active Tasks list current — picking up new tasks and, crucially, clearing
 * finished ones so their live "Elapsed" stops ticking.
 */
const WORKER_DETAIL_REFETCH_MS = 5000

/**
 * How often (ms) the worker-list query refetches while the page is open. The
 * `workers` WebSocket subject only carries online/offline status transitions —
 * no event fires for routine heartbeats — so an idle online worker would never
 * refresh its "Last Heartbeat" cell (and the page's "Updated …" timestamp would
 * climb forever) without polling. Aligned with the worker's default 15 s
 * heartbeat interval: polling faster gains no freshness since no new heartbeat
 * has arrived.
 */
export const WORKER_LIST_REFETCH_MS = 15_000

// ── Query key factory ─────────────────────────────────────────────────────────
// Keys are structured arrays enabling prefix-based invalidation.
// Use queryKeys.jobs.all to invalidate every job query (list + detail).

/** Query parameters accepted by {@link fetchListJobs} / {@link useListJobs}. */
export type ListJobsParams = {
  status?: string
  search?: string
  owner?: string
  queue_id?: string
  project?: string
  sort_by?: 'created_at' | 'priority' | 'status' | 'updated_at' | 'name'
  sort_dir?: 'asc' | 'desc'
  limit?: number
  offset?: number
}

/** Query parameters accepted by {@link fetchListTasks} / {@link useListTasks}. */
export type ListTasksParams = {
  status?: string
  sort_by?: 'created_at' | 'status' | 'updated_at' | 'name'
  sort_dir?: 'asc' | 'desc'
  limit?: number
  offset?: number
}

/** Query parameters accepted by {@link fetchListWorkers} / {@link useListWorkers}. */
export type ListWorkersParams = {
  farm_id?: string
  queue_id?: string
  compute_location?: string
  status?: string
  search?: string
  sort_by?: 'hostname' | 'status' | 'registered_at' | 'last_heartbeat_at'
  sort_dir?: 'asc' | 'desc'
  limit?: number
  offset?: number
}

/**
 * Structured query keys for TanStack Query. Keys are arrays so related queries
 * share a prefix; e.g. invalidating `queryKeys.jobs.all` matches every job
 * query (both list and detail) by prefix.
 */
export const queryKeys = {
  jobs: {
    all: ['jobs'] as const,
    list: (params: ListJobsParams) => ['jobs', 'list', params] as const,
    detail: (id: string) => ['jobs', 'detail', id] as const,
  },
  tasks: {
    all: ['tasks'] as const,
    list: (jobId: string, params?: ListTasksParams) => ['tasks', 'list', jobId, params] as const,
    detail: (id: string) => ['tasks', 'detail', id] as const,
    logs: (id: string) => ['tasks', 'logs', id] as const,
  },
  workers: {
    all: ['workers'] as const,
    list: (params?: ListWorkersParams) => ['workers', 'list', params] as const,
    detail: (id: string) => ['workers', 'detail', id] as const,
  },
  farms: {
    all: ['farms'] as const,
    detail: (id: string) => ['farms', 'detail', id] as const,
  },
  queues: {
    all: ['queues'] as const,
    list: (farmId: string) => ['queues', 'list', farmId] as const,
    detail: (id: string) => ['queues', 'detail', id] as const,
  },
  usagePools: {
    all: ['usage-pools'] as const,
    detail: (id: string) => ['usage-pools', 'detail', id] as const,
  },
  storageLocations: {
    all: ['storage-locations'] as const,
    detail: (id: string) => ['storage-locations', 'detail', id] as const,
  },
  products: {
    all: ['products'] as const,
    detail: (name: string) => ['products', name] as const,
  },
  version: {
    all: ['version'] as const,
  },
} as const

// ── Raw fetch functions ───────────────────────────────────────────────────────

function buildQS(params: Record<string, string | number | boolean | undefined>): string {
  const qs = new URLSearchParams()
  for (const [key, value] of Object.entries(params)) {
    if (value !== undefined) {
      qs.set(key, String(value))
    }
  }
  const s = qs.toString()
  return s ? `?${s}` : ''
}

/** Fetch a page of jobs from `GET /jobs`. Prefer {@link useListJobs} in components. */
export function fetchListJobs(params: ListJobsParams): Promise<ListResponse<Job>> {
  return apiFetch(
    `/jobs${buildQS({
      status: params.status,
      search: params.search,
      owner: params.owner,
      queue_id: params.queue_id,
      project: params.project,
      sort_by: params.sort_by,
      sort_dir: params.sort_dir,
      limit: params.limit,
      offset: params.offset,
    })}`,
  )
}

/** Fetch one job (with steps and task counts) from `GET /jobs/{id}`. */
export function fetchGetJob(id: string): Promise<JobDetail> {
  return apiFetch(`/jobs/${encodeURIComponent(id)}`)
}

/** Fetch a page of tasks for a job from `GET /jobs/{jobId}/tasks`. */
export function fetchListTasks(
  jobId: string,
  params?: ListTasksParams,
): Promise<ListResponse<Task>> {
  return apiFetch(
    `/jobs/${encodeURIComponent(jobId)}/tasks${buildQS({
      status: params?.status,
      sort_by: params?.sort_by,
      sort_dir: params?.sort_dir,
      limit: params?.limit,
      offset: params?.offset,
    })}`,
  )
}

/** Fetch one task from `GET /tasks/{id}`. */
export function fetchGetTask(id: string): Promise<Task> {
  return apiFetch(`/tasks/${encodeURIComponent(id)}`)
}

/** Fetch a page of workers from `GET /workers`. */
export function fetchListWorkers(params?: ListWorkersParams): Promise<ListResponse<Worker>> {
  return apiFetch(
    `/workers${buildQS({
      farm_id: params?.farm_id,
      queue_id: params?.queue_id,
      compute_location: params?.compute_location,
      status: params?.status,
      search: params?.search,
      sort_by: params?.sort_by,
      sort_dir: params?.sort_dir,
      limit: params?.limit,
      offset: params?.offset,
    })}`,
  )
}

/** Fetch one worker (with current task and capabilities) from `GET /workers/{id}`. */
export function fetchGetWorker(id: string): Promise<WorkerDetail> {
  return apiFetch(`/workers/${encodeURIComponent(id)}`)
}

/** Fetch all farms from `GET /farms` (unordered, no pagination). */
export function fetchListFarms(): Promise<Farm[]> {
  return apiFetch('/farms')
}

/** Fetch one farm from `GET /farms/{id}`. */
export function fetchGetFarm(id: string): Promise<Farm> {
  return apiFetch(`/farms/${encodeURIComponent(id)}`)
}

/** Fetch a page of queues belonging to a farm from `GET /queues?farm_id=…`. */
export function fetchListQueues(farmId: string, limit?: number): Promise<ListResponse<Queue>> {
  return apiFetch(`/queues${buildQS({ farm_id: farmId, limit })}`)
}

/** Fetch one queue from `GET /queues/{id}`. */
export function fetchGetQueue(id: string): Promise<Queue> {
  return apiFetch(`/queues/${encodeURIComponent(id)}`)
}

/** Fetch all usage pools from `GET /usage-pools` (server returns them name-sorted, no pagination). */
export function fetchListUsagePools(): Promise<UsagePool[]> {
  return apiFetch('/usage-pools')
}

/** Fetch one usage pool from `GET /usage-pools/{id}`. */
export function fetchGetUsagePool(id: string): Promise<UsagePool> {
  return apiFetch(`/usage-pools/${encodeURIComponent(id)}`)
}

/** Fetch all storage locations from `GET /storage-locations` (bare array, no pagination). */
export function fetchListStorageLocations(): Promise<StorageLocation[]> {
  return apiFetch('/storage-locations')
}

/** Fetch one storage location from `GET /storage-locations/{id}`. */
export function fetchGetStorageLocation(id: string): Promise<StorageLocation> {
  return apiFetch(`/storage-locations/${encodeURIComponent(id)}`)
}

/** Fetch the server build metadata from `GET /version`. */
export function fetchVersion(): Promise<VersionInfo> {
  return apiFetch('/version')
}

/** Fetch all products from `GET /products` (bare array, no pagination). */
export function fetchProducts(): Promise<Product[]> {
  return apiFetch('/products')
}

/** Fetch one product by name from `GET /products/{name}`. */
export function fetchProduct(name: string): Promise<Product> {
  return apiFetch(`/products/${encodeURIComponent(name)}`)
}

// ── Combined farms + queues fetch ─────────────────────────────────────────────

/** A farm paired with its queues, as produced by {@link fetchFarmsWithQueues}. */
export type FarmWithQueues = {
  farm: Farm
  queues: Queue[]
}

/**
 * Fetch every farm together with its queues, for the submission-form target
 * selector. Queues are requested with a high limit so the selector is never
 * silently truncated.
 */
export async function fetchFarmsWithQueues(): Promise<FarmWithQueues[]> {
  const farms = await fetchListFarms()
  // Request a high limit so the selector is never silently truncated — a
  // partial queue list would prevent submitting to queues beyond the server's
  // default page size.
  return Promise.all(
    farms.map((farm) =>
      fetchListQueues(farm.id, 1000).then((res) => ({ farm, queues: res.items })),
    ),
  )
}

// ── Query hooks ───────────────────────────────────────────────────────────────

/** List jobs with optional filters, pagination, and sort. */
export function useListJobs(params: ListJobsParams) {
  return useQuery({
    queryKey: queryKeys.jobs.list(params),
    queryFn: () => fetchListJobs(params),
  })
}

/** Fetch a single job by ID, including steps and task counts. */
export function useGetJob(id: string) {
  return useQuery({
    queryKey: queryKeys.jobs.detail(id),
    queryFn: () => fetchGetJob(id),
  })
}

/** List tasks for a specific job. */
export function useListTasks(jobId: string, params?: ListTasksParams) {
  return useQuery({
    queryKey: queryKeys.tasks.list(jobId, params),
    queryFn: () => fetchListTasks(jobId, params),
  })
}

/** Fetch a single task by ID. */
export function useGetTask(id: string) {
  return useQuery({
    queryKey: queryKeys.tasks.detail(id),
    queryFn: () => fetchGetTask(id),
  })
}

/** List workers with optional filters, pagination, and sort. */
export function useListWorkers(params?: ListWorkersParams) {
  return useQuery({
    queryKey: queryKeys.workers.list(params),
    queryFn: () => fetchListWorkers(params),
    refetchInterval: WORKER_LIST_REFETCH_MS,
  })
}

/** Fetch a single worker by ID, including current task and capabilities. */
export function useGetWorker(id: string) {
  return useQuery({
    queryKey: queryKeys.workers.detail(id),
    queryFn: () => fetchGetWorker(id),
    refetchInterval: WORKER_DETAIL_REFETCH_MS,
  })
}

/** List all farms (unordered, no pagination). */
export function useListFarms() {
  return useQuery({
    queryKey: queryKeys.farms.all,
    queryFn: fetchListFarms,
  })
}

/** Load a single farm by id. */
export function useGetFarm(id: string) {
  return useQuery({
    queryKey: queryKeys.farms.detail(id),
    queryFn: () => fetchGetFarm(id),
    enabled: id !== '',
  })
}

/** List queues belonging to a specific farm. */
export function useListQueues(farmId: string) {
  return useQuery({
    queryKey: queryKeys.queues.list(farmId),
    queryFn: () => fetchListQueues(farmId),
  })
}

/** Load a single queue by id. */
export function useGetQueue(id: string) {
  return useQuery({
    queryKey: queryKeys.queues.detail(id),
    queryFn: () => fetchGetQueue(id),
    enabled: id !== '',
  })
}

/**
 * List all usage pools, including live utilization. Refetches on an interval
 * so in_use / available stay current while the page is open.
 */
export function useListUsagePools() {
  return useQuery({
    queryKey: queryKeys.usagePools.all,
    queryFn: fetchListUsagePools,
    refetchInterval: USAGE_POOL_REFETCH_MS,
  })
}

/** Load a single usage pool by id. */
export function useGetUsagePool(id: string) {
  return useQuery({
    queryKey: queryKeys.usagePools.detail(id),
    queryFn: () => fetchGetUsagePool(id),
    enabled: id !== '',
  })
}

/** List all storage locations (name-sorted by the server, no pagination). */
export function useListStorageLocations() {
  return useQuery({
    queryKey: queryKeys.storageLocations.all,
    queryFn: fetchListStorageLocations,
  })
}

/** Load a single storage location by id. */
export function useGetStorageLocation(id: string) {
  return useQuery({
    queryKey: queryKeys.storageLocations.detail(id),
    queryFn: () => fetchGetStorageLocation(id),
    enabled: id !== '',
  })
}

/** List all products in the catalog (bare array, no pagination). */
export function useProducts() {
  return useQuery({
    queryKey: queryKeys.products.all,
    queryFn: fetchProducts,
  })
}

/** Load a single product by name. Disabled when name is empty. */
export function useProduct(name: string) {
  return useQuery({
    queryKey: queryKeys.products.detail(name),
    queryFn: () => fetchProduct(name),
    enabled: name !== '',
  })
}

/** Load the running server's build metadata (version, commit, …). */
export function useVersion() {
  return useQuery({
    queryKey: queryKeys.version.all,
    queryFn: fetchVersion,
  })
}

/** Load all farms along with their queues in a single combined query. */
export function useFarmsWithQueues() {
  return useQuery({
    queryKey: [...queryKeys.farms.all, 'with-queues'] as const,
    queryFn: fetchFarmsWithQueues,
  })
}

/** Parameters for {@link fetchTaskLogs}. */
export type FetchTaskLogsParams = {
  taskId: string
  /** Return only chunks after this NATS stream sequence (offset-based paging). */
  afterNatsSeq?: number
  limit?: number
}

/**
 * Fetch a page of stored log chunks for a task from
 * `GET /tasks/{id}/logs`, using offset-based pagination on the NATS sequence.
 */
export function fetchTaskLogs(params: FetchTaskLogsParams): Promise<TaskLogsResponse> {
  return apiFetch(
    `/tasks/${encodeURIComponent(params.taskId)}/logs${buildQS({
      after_nats_seq: params.afterNatsSeq,
      limit: params.limit,
    })}`,
  )
}

/**
 * Fetch the first page of a task's stored log chunks. Used by the task-log page
 * to detect whether a task produced any output (so a failed task with none can
 * fall back to worker diagnostics); a small limit is enough for that decision.
 */
export function useTaskLogs(taskId: string) {
  return useQuery({
    queryKey: queryKeys.tasks.logs(taskId),
    queryFn: () => fetchTaskLogs({ taskId, limit: 1 }),
    enabled: taskId !== '',
  })
}
