// SPDX-License-Identifier: AGPL-3.0-or-later

import { useQuery } from '@tanstack/react-query'
import { apiFetch } from './client'
import type { ServersResponse } from './types'

/** How often the Servers page re-reads the group, in milliseconds. */
export const SERVERS_REFETCH_MS = 10_000

export function fetchServers(): Promise<ServersResponse> {
  return apiFetch<ServersResponse>('/servers')
}

export const serversQueryKey = ['servers'] as const

/** The leader and standby servers of this farm's high-availability group. */
export function useServers() {
  return useQuery({
    queryKey: serversQueryKey,
    queryFn: fetchServers,
    refetchInterval: SERVERS_REFETCH_MS,
  })
}
