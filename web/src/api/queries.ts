// SPDX-License-Identifier: AGPL-3.0-or-later

import { useQuery } from '@tanstack/react-query'
import { apiFetch } from './client'
import type {
  ApiKey,
  ComputeLocation,
  Farm,
  Job,
  JobDetail,
  PresetDetail,
  PresetListItem,
  Principal,
  Product,
  ProductParameter,
  StorageLocation,
  UsagePool,
  ListResponse,
  Queue,
  Task,
  TaskAttemptsResponse,
  TaskLogsResponse,
  User,
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
  farm_id?: string
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
    attempts: (id: string) => ['tasks', 'attempts', id] as const,
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
  computeLocations: {
    all: ['compute-locations'] as const,
    detail: (id: string) => ['compute-locations', 'detail', id] as const,
  },
  products: {
    all: ['products'] as const,
    detail: (name: string) => ['products', 'detail', name] as const,
    parameters: (name: string) => ['products', 'detail', name, 'parameters'] as const,
  },
  presets: {
    all: ['presets'] as const,
    detail: (name: string) => ['presets', name] as const,
  },
  version: {
    all: ['version'] as const,
  },
  auth: {
    me: ['auth', 'me'] as const,
  },
  users: {
    all: ['users'] as const,
    detail: (id: string) => ['users', 'detail', id] as const,
  },
  apiKeys: {
    // `self` is a sibling of `for-user`, not a prefix of it: invalidating the
    // caller's own key list must not refetch every admin per-user list that
    // happens to be cached (TanStack Query matches keys by prefix).
    self: ['api-keys', 'self'] as const,
    forUser: (userId: string) => ['api-keys', 'for-user', userId] as const,
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
      farm_id: params.farm_id,
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

/** Fetch a task's attempt history (oldest first) from `GET /tasks/{id}/attempts`. */
export function fetchTaskAttempts(id: string): Promise<TaskAttemptsResponse> {
  return apiFetch(`/tasks/${encodeURIComponent(id)}/attempts`)
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

/** Fetch all compute locations from `GET /compute-locations` (bare array, no pagination). */
export function fetchListComputeLocations(): Promise<ComputeLocation[]> {
  return apiFetch('/compute-locations')
}

/** Fetch one compute location from `GET /compute-locations/{id}`. */
export function fetchGetComputeLocation(id: string): Promise<ComputeLocation> {
  return apiFetch(`/compute-locations/${encodeURIComponent(id)}`)
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

/** Fetch a product's parsed parameters from GET /products/{name}/parameters. */
export function fetchProductParameters(name: string): Promise<ProductParameter[]> {
  return apiFetch(`/products/${encodeURIComponent(name)}/parameters`)
}

/** Fetch all presets from `GET /presets` (bare array, no pagination). Pass refresh=true to force a remote check. */
export function fetchPresets(refresh = false): Promise<PresetListItem[]> {
  return apiFetch(`/presets${refresh ? '?refresh=true' : ''}`)
}

/** Fetch one preset by name from `GET /presets/{name}`. */
export function fetchPreset(name: string): Promise<PresetDetail> {
  return apiFetch(`/presets/${encodeURIComponent(name)}`)
}

/** Fetch the current authenticated principal from `GET /auth/me`. */
export function fetchAuthMe(): Promise<Principal> {
  return apiFetch('/auth/me')
}

/** Fetch all local user accounts from `GET /users` (server returns them username-sorted). */
export function fetchListUsers(): Promise<User[]> {
  return apiFetch('/users')
}

/** Fetch one local user account from `GET /users/{id}`. */
export function fetchGetUser(id: string): Promise<User> {
  return apiFetch(`/users/${encodeURIComponent(id)}`)
}

/** Fetch all API keys (metadata only, no secret) from `GET /api-keys`. */
export function fetchListApiKeys(): Promise<ApiKey[]> {
  return apiFetch('/api-keys')
}

export function fetchListUserApiKeys(userId: string): Promise<ApiKey[]> {
  return apiFetch(`/users/${encodeURIComponent(userId)}/api-keys`)
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
export function useListJobs(params: ListJobsParams, options?: { enabled?: boolean }) {
  return useQuery({
    queryKey: queryKeys.jobs.list(params),
    queryFn: () => fetchListJobs(params),
    enabled: options?.enabled ?? true,
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

/** Fetch a task's attempt history. Mount only while the attempt row is expanded. */
export function useTaskAttempts(id: string) {
  return useQuery({
    queryKey: queryKeys.tasks.attempts(id),
    queryFn: () => fetchTaskAttempts(id),
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

/** List all compute locations (name-sorted by the server, no pagination). */
export function useListComputeLocations() {
  return useQuery({
    queryKey: queryKeys.computeLocations.all,
    queryFn: fetchListComputeLocations,
  })
}

/** Load a single compute location by id. */
export function useGetComputeLocation(id: string) {
  return useQuery({
    queryKey: queryKeys.computeLocations.detail(id),
    queryFn: () => fetchGetComputeLocation(id),
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

/** Load a product's parsed parameters. Disabled when name is empty. */
export function useProductParameters(name: string) {
  return useQuery({
    queryKey: queryKeys.products.parameters(name),
    queryFn: () => fetchProductParameters(name),
    enabled: name !== '',
  })
}

/** List all presets in the catalog (bare array, no pagination). Pass refresh=true to force a remote check. */
export function usePresets(refresh = false) {
  return useQuery({ queryKey: queryKeys.presets.all, queryFn: () => fetchPresets(refresh) })
}

/** Load a single preset by name. Disabled when name is empty. */
export function usePreset(name: string) {
  return useQuery({
    queryKey: queryKeys.presets.detail(name),
    queryFn: () => fetchPreset(name),
    enabled: !!name,
  })
}

/** Load the running server's build metadata (version, commit, …). */
export function useVersion() {
  return useQuery({
    queryKey: queryKeys.version.all,
    queryFn: fetchVersion,
  })
}

/**
 * Load the current authenticated principal. A 401 here is an expected,
 * non-transient outcome (no session / expired session) rather than a
 * failure worth retrying, so retries are disabled regardless of the global
 * default.
 */
export function useAuthMe() {
  return useQuery({ queryKey: queryKeys.auth.me, queryFn: fetchAuthMe, retry: false })
}

/** List all local user accounts (username-sorted by the server, no pagination). */
export function useListUsers() {
  return useQuery({
    queryKey: queryKeys.users.all,
    queryFn: fetchListUsers,
  })
}

/** Load a single local user account by id. Disabled when id is empty. */
export function useGetUser(id: string) {
  return useQuery({
    queryKey: queryKeys.users.detail(id),
    queryFn: () => fetchGetUser(id),
    enabled: id !== '',
  })
}

/** List all API keys for the current account (metadata only, no secret). */
export function useApiKeys() {
  return useQuery({
    queryKey: queryKeys.apiKeys.self,
    queryFn: fetchListApiKeys,
  })
}

/** List another user's API keys (metadata only). Requires apikeys.admin. */
export function useUserApiKeys(userId: string) {
  return useQuery({
    queryKey: queryKeys.apiKeys.forUser(userId),
    queryFn: () => fetchListUserApiKeys(userId),
    enabled: userId !== '',
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
