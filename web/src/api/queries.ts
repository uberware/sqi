// SPDX-License-Identifier: AGPL-3.0-only

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

export type ListTasksParams = {
  status?: string
  sort_by?: 'created_at' | 'status' | 'updated_at' | 'name'
  sort_dir?: 'asc' | 'desc'
  limit?: number
  offset?: number
}

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
  },
  queues: {
    all: ['queues'] as const,
    list: (farmId: string) => ['queues', 'list', farmId] as const,
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

export function fetchGetJob(id: string): Promise<JobDetail> {
  return apiFetch(`/jobs/${encodeURIComponent(id)}`)
}

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

export function fetchGetTask(id: string): Promise<Task> {
  return apiFetch(`/tasks/${encodeURIComponent(id)}`)
}

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

export function fetchGetWorker(id: string): Promise<WorkerDetail> {
  return apiFetch(`/workers/${encodeURIComponent(id)}`)
}

export function fetchListFarms(): Promise<Farm[]> {
  return apiFetch('/farms')
}

export function fetchListQueues(farmId: string, limit?: number): Promise<ListResponse<Queue>> {
  return apiFetch(`/queues${buildQS({ farm_id: farmId, limit })}`)
}

// ── Combined farms + queues fetch ─────────────────────────────────────────────

export type FarmWithQueues = {
  farm: Farm
  queues: Queue[]
}

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

/** List queues belonging to a specific farm. */
export function useListQueues(farmId: string) {
  return useQuery({
    queryKey: queryKeys.queues.list(farmId),
    queryFn: () => fetchListQueues(farmId),
  })
}

/** Load all farms along with their queues in a single combined query. */
export function useFarmsWithQueues() {
  return useQuery({
    queryKey: [...queryKeys.farms.all, 'with-queues'] as const,
    queryFn: fetchFarmsWithQueues,
  })
}

export type FetchTaskLogsParams = {
  taskId: string
  afterNatsSeq?: number
  limit?: number
}

export function fetchTaskLogs(params: FetchTaskLogsParams): Promise<TaskLogsResponse> {
  return apiFetch(
    `/tasks/${encodeURIComponent(params.taskId)}/logs${buildQS({
      after_nats_seq: params.afterNatsSeq,
      limit: params.limit,
    })}`,
  )
}
