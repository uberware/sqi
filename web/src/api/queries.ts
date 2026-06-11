// SPDX-License-Identifier: AGPL-3.0-or-later

import { useQuery } from '@tanstack/react-query'
import { apiFetch } from './client'
import type {
  Farm,
  Job,
  JobDetail,
  ListResponse,
  Queue,
  Task,
  TaskLogsResponse,
  Worker,
  WorkerDetail,
} from './types'

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
  })
}

/** Fetch a single worker by ID, including current task and capabilities. */
export function useGetWorker(id: string) {
  return useQuery({
    queryKey: queryKeys.workers.detail(id),
    queryFn: () => fetchGetWorker(id),
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
