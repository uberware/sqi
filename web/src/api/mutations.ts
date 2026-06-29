// SPDX-License-Identifier: AGPL-3.0-or-later

import { useMutation, useQueryClient } from '@tanstack/react-query'
import { apiFetch } from './client'
import { queryKeys } from './queries'
import type {
  ComputeLocation,
  Farm,
  Job,
  StorageLocation,
  StorageLocationType,
  UsagePool,
  Queue,
  RetryResponse,
  RetryJobResponse,
  CancelResponse,
  SubmitJobInput,
  SubmitProductJobInput,
  WorkerActionResponse,
  Product,
  TemplateFormat,
} from './types'

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

/** Cancel a job via `POST /jobs/{id}/cancel`. Resolves on the server's 2xx/204. */
export async function fetchCancelJob(id: string): Promise<void> {
  await apiFetch(`/jobs/${encodeURIComponent(id)}/cancel`, { method: 'POST' })
}

/** Retry a job's failed/canceled tasks via `POST /jobs/{id}/retry`. */
export function fetchRetryJob(id: string): Promise<RetryJobResponse> {
  return apiFetch<RetryJobResponse>(`/jobs/${encodeURIComponent(id)}/retry`, {
    method: 'POST',
  })
}

/** Hard-delete a job and all its data via `DELETE /jobs/{id}`. */
export async function fetchDeleteJob(id: string): Promise<void> {
  await apiFetch(`/jobs/${encodeURIComponent(id)}`, { method: 'DELETE' })
}

/** Retry a failed or canceled task via `POST /tasks/{id}/retry`. */
export function fetchRetryTask(id: string): Promise<RetryResponse> {
  return apiFetch<RetryResponse>(`/tasks/${encodeURIComponent(id)}/retry`, {
    method: 'POST',
  })
}

/** Cancel a non-terminal task via `POST /tasks/{id}/cancel`. */
export function fetchCancelTask(id: string): Promise<CancelResponse> {
  return apiFetch<CancelResponse>(`/tasks/${encodeURIComponent(id)}/cancel`, {
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

/** Hard-delete a removable worker via `DELETE /workers/{id}`. */
export async function fetchRemoveWorker(id: string): Promise<void> {
  await apiFetch(`/workers/${encodeURIComponent(id)}`, { method: 'DELETE' })
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

/**
 * Retry a job's failed/canceled tasks. Invalidates all job and task queries on
 * success (job status and task counts both change).
 */
export function useRetryJob() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => fetchRetryJob(id),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.jobs.all })
      void queryClient.invalidateQueries({ queryKey: queryKeys.tasks.all })
    },
  })
}

/** Hard-delete a job and all its data. Invalidates all job queries on success. */
export function useDeleteJob() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => fetchDeleteJob(id),
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

/** Cancel a non-terminal task. Invalidates all task and job queries. */
export function useCancelTask() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => fetchCancelTask(id),
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

/** Hard-delete a removable worker. Invalidates all worker queries. */
export function useRemoveWorker() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => fetchRemoveWorker(id),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.workers.all })
    },
  })
}

// ── Farm input ────────────────────────────────────────────────────────────────

export interface FarmInput {
  name: string
  description: string
  max_concurrent_tasks: number
}

export function fetchCreateFarm(input: FarmInput): Promise<Farm> {
  return apiFetch<Farm>('/farms', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
}

export function fetchUpdateFarm(id: string, input: FarmInput): Promise<Farm> {
  return apiFetch<Farm>(`/farms/${encodeURIComponent(id)}`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
}

export async function fetchDeleteFarm(id: string): Promise<void> {
  await apiFetch(`/farms/${encodeURIComponent(id)}`, { method: 'DELETE' })
}

export function useCreateFarm() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: fetchCreateFarm,
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.farms.all })
    },
  })
}

export function useUpdateFarm() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ id, input }: { id: string; input: FarmInput }) => fetchUpdateFarm(id, input),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.farms.all })
    },
  })
}

export function useDeleteFarm() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => fetchDeleteFarm(id),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.farms.all })
    },
  })
}

// ── Queue input ───────────────────────────────────────────────────────────────

export interface QueueInput {
  farm_id: string
  name: string
  description: string
  priority: number
  max_concurrent_tasks: number
  paused: boolean
}

export function fetchCreateQueue(input: QueueInput): Promise<Queue> {
  return apiFetch<Queue>('/queues', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
}

export function fetchUpdateQueue(id: string, input: QueueInput): Promise<Queue> {
  return apiFetch<Queue>(`/queues/${encodeURIComponent(id)}`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
}

export async function fetchDeleteQueue(id: string): Promise<void> {
  await apiFetch(`/queues/${encodeURIComponent(id)}`, { method: 'DELETE' })
}

export function useCreateQueue() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: fetchCreateQueue,
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.queues.all })
      void queryClient.invalidateQueries({ queryKey: queryKeys.farms.all })
    },
  })
}

export function useUpdateQueue() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ id, input }: { id: string; input: QueueInput }) => fetchUpdateQueue(id, input),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.queues.all })
      void queryClient.invalidateQueries({ queryKey: queryKeys.farms.all })
    },
  })
}

export function useDeleteQueue() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => fetchDeleteQueue(id),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.queues.all })
      void queryClient.invalidateQueries({ queryKey: queryKeys.farms.all })
    },
  })
}

// ── Usage pool input ──────────────────────────────────────────────────────────

export interface UsagePoolInput {
  name: string
  server_hint?: string
  max_concurrent: number
}

export function fetchCreateUsagePool(input: UsagePoolInput): Promise<UsagePool> {
  return apiFetch<UsagePool>('/usage-pools', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
}

export function fetchUpdateUsagePool(id: string, input: UsagePoolInput): Promise<UsagePool> {
  return apiFetch<UsagePool>(`/usage-pools/${encodeURIComponent(id)}`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
}

export async function fetchDeleteUsagePool(id: string): Promise<void> {
  await apiFetch(`/usage-pools/${encodeURIComponent(id)}`, { method: 'DELETE' })
}

export function useCreateUsagePool() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: fetchCreateUsagePool,
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.usagePools.all })
    },
  })
}

export function useUpdateUsagePool() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ id, input }: { id: string; input: UsagePoolInput }) =>
      fetchUpdateUsagePool(id, input),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.usagePools.all })
    },
  })
}

export function useDeleteUsagePool() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => fetchDeleteUsagePool(id),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.usagePools.all })
    },
  })
}

// ── Storage location input ────────────────────────────────────────────────────

export interface StorageLocationInput {
  name: string
  type: StorageLocationType
  description?: string
  roots?: Record<string, string>
}

export function fetchCreateStorageLocation(input: StorageLocationInput): Promise<StorageLocation> {
  return apiFetch<StorageLocation>('/storage-locations', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
}

export function fetchUpdateStorageLocation(
  id: string,
  input: StorageLocationInput,
): Promise<StorageLocation> {
  return apiFetch<StorageLocation>(`/storage-locations/${encodeURIComponent(id)}`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
}

export async function fetchDeleteStorageLocation(id: string): Promise<void> {
  await apiFetch(`/storage-locations/${encodeURIComponent(id)}`, { method: 'DELETE' })
}

export function useCreateStorageLocation() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: fetchCreateStorageLocation,
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.storageLocations.all })
    },
  })
}

export function useUpdateStorageLocation() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ id, input }: { id: string; input: StorageLocationInput }) =>
      fetchUpdateStorageLocation(id, input),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.storageLocations.all })
    },
  })
}

export function useDeleteStorageLocation() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => fetchDeleteStorageLocation(id),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.storageLocations.all })
    },
  })
}

// ── Compute location input ────────────────────────────────────────────────────

export interface ComputeLocationInput {
  name: string
  description?: string
}

export function fetchCreateComputeLocation(input: ComputeLocationInput): Promise<ComputeLocation> {
  return apiFetch<ComputeLocation>('/compute-locations', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
}

export function fetchUpdateComputeLocation(
  id: string,
  input: ComputeLocationInput,
): Promise<ComputeLocation> {
  return apiFetch<ComputeLocation>(`/compute-locations/${encodeURIComponent(id)}`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
}

export async function fetchDeleteComputeLocation(id: string): Promise<void> {
  await apiFetch(`/compute-locations/${encodeURIComponent(id)}`, { method: 'DELETE' })
}

export function useCreateComputeLocation() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: fetchCreateComputeLocation,
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.computeLocations.all })
    },
  })
}

export function useUpdateComputeLocation() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ id, input }: { id: string; input: ComputeLocationInput }) =>
      fetchUpdateComputeLocation(id, input),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.computeLocations.all })
    },
  })
}

export function useDeleteComputeLocation() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => fetchDeleteComputeLocation(id),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.computeLocations.all })
    },
  })
}

// ── Product input ─────────────────────────────────────────────────────────────

export interface ProductInput {
  name: string
  title: string
  description: string
  category: string
  version: string
  template: string
  format: TemplateFormat
}

export function fetchCreateProduct(input: ProductInput): Promise<Product> {
  return apiFetch<Product>('/products', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
}

export function fetchUpdateProduct(name: string, input: ProductInput): Promise<Product> {
  return apiFetch<Product>(`/products/${encodeURIComponent(name)}`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
}

export async function fetchDeleteProduct(name: string): Promise<void> {
  await apiFetch(`/products/${encodeURIComponent(name)}`, { method: 'DELETE' })
}

/** Create a custom product. Invalidates the product list on success. */
export function useCreateProduct() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: fetchCreateProduct,
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.products.all })
    },
  })
}

/** Update a custom/installed product by its (original) name. Invalidates products. */
export function useUpdateProduct() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ name, input }: { name: string; input: ProductInput }) =>
      fetchUpdateProduct(name, input),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.products.all })
    },
  })
}

/** Delete a custom/installed product by name. Invalidates the product list. */
export function useDeleteProduct() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (name: string) => fetchDeleteProduct(name),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.products.all })
    },
  })
}

// ── Product job submission ─────────────────────────────────────────────────────

export function fetchSubmitProductJob(input: SubmitProductJobInput): Promise<Job> {
  const body: Record<string, unknown> = {
    name: input.name,
    farm_id: input.farmId,
    queue_id: input.queueId,
    parameters: input.parameters,
  }
  if (input.owner !== undefined) body.owner = input.owner
  if (input.priority !== undefined) body.priority = input.priority
  if (input.project !== undefined) body.project = input.project
  return apiFetch<Job>(`/products/${encodeURIComponent(input.productName)}/jobs`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
}

/** Submit a job via a product. Invalidates the job list on success. */
export function useSubmitProductJob() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: fetchSubmitProductJob,
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.jobs.all })
    },
  })
}
