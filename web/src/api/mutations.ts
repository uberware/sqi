// SPDX-License-Identifier: AGPL-3.0-only

import { useMutation, useQueryClient } from '@tanstack/react-query'
import { apiFetch } from './client'
import { queryKeys } from './queries'
import type { Job, RetryResponse, SubmitJobInput, WorkerActionResponse } from './types'

// ── Raw mutation fetch functions ──────────────────────────────────────────────

/**
 * Submit a raw OpenJD template via `POST /jobs`. The body content type is set
 * from `input.format` (`application/yaml` or `application/json`); `farm_id`,
 * `queue_id`, `owner`, `submitter`, `priority`, and `project` travel as query
 * parameters.
 */
export function fetchSubmitJob(input: SubmitJobInput): Promise<Job> {
  const contentType = input.format === 'yaml' ? 'application/yaml' : 'application/json'

  const qs = new URLSearchParams({ farm_id: input.farmId, queue_id: input.queueId })
  if (input.owner !== undefined) qs.set('owner', input.owner)
  if (input.submitter !== undefined) qs.set('submitter', input.submitter)
  if (input.priority !== undefined) qs.set('priority', String(input.priority))
  if (input.project !== undefined) qs.set('project', input.project)

  return apiFetch<Job>(`/jobs?${qs.toString()}`, {
    method: 'POST',
    headers: { 'Content-Type': contentType },
    body: input.template,
  })
}

/** Cancel a job via `DELETE /jobs/{id}`. Resolves on the server's 2xx/204. */
export async function fetchCancelJob(id: string): Promise<void> {
  await apiFetch(`/jobs/${encodeURIComponent(id)}`, { method: 'DELETE' })
}

/** Retry a failed or canceled task via `POST /tasks/{id}/retry`. */
export function fetchRetryTask(id: string): Promise<RetryResponse> {
  return apiFetch<RetryResponse>(`/tasks/${encodeURIComponent(id)}/retry`, {
    method: 'POST',
  })
}

/** Administratively disable a worker via `POST /workers/{id}/disable`. */
export function fetchDisableWorker(id: string): Promise<WorkerActionResponse> {
  return apiFetch<WorkerActionResponse>(`/workers/${encodeURIComponent(id)}/disable`, {
    method: 'POST',
  })
}

/** Re-enable a disabled worker via `POST /workers/{id}/enable`. */
export function fetchEnableWorker(id: string): Promise<WorkerActionResponse> {
  return apiFetch<WorkerActionResponse>(`/workers/${encodeURIComponent(id)}/enable`, {
    method: 'POST',
  })
}

// ── Mutation hooks ────────────────────────────────────────────────────────────

/** Submit a new OpenJD job. Invalidates the job list on success. */
export function useSubmitJob() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: fetchSubmitJob,
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.jobs.all })
    },
  })
}

/**
 * Cancel a job by ID. Invalidates all job queries on success.
 * TanStack Query prefix-matches queryKeys.jobs.all against all job queries
 * (list and detail), so no separate detail invalidation is needed.
 */
export function useCancelJob() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => fetchCancelJob(id),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.jobs.all })
    },
  })
}

/** Retry a failed or canceled task. Invalidates all task and job queries. */
export function useRetryTask() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => fetchRetryTask(id),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.tasks.all })
      void queryClient.invalidateQueries({ queryKey: queryKeys.jobs.all })
    },
  })
}

/** Administratively disable a worker. Invalidates all worker queries. */
export function useDisableWorker() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => fetchDisableWorker(id),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.workers.all })
    },
  })
}

/** Re-enable a disabled worker. Invalidates all worker queries. */
export function useEnableWorker() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => fetchEnableWorker(id),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.workers.all })
    },
  })
}
