/**
 * Admin Window Hunter API endpoints
 * 窗口猎手 P1/P2：狩猎编排器 + 满血会话池
 */

import { apiClient } from '../client'
import type { WindowProbeHealth, WindowProbeResult } from './windowProbe'

// ---------------------------------------------------------------------------
// P1 猎手
// ---------------------------------------------------------------------------

export interface WindowHunterCandidateResult {
  proxy_id: number
  proxy_name: string
  region?: string
  result: WindowProbeResult
  answer?: string
  latency_ms: number
  message?: string
}

export interface WindowHunterRunEntry {
  id: string
  account_id: number
  trigger: 'manual' | 'auto'
  started_at: string
  finished_at: string
  running: boolean
  hit: boolean
  hit_proxy_id?: number
  scanned: number
  total: number
  round: number
  next_run_at?: string
  candidates?: WindowHunterCandidateResult[]
  error?: string
}

export interface WindowHunterSettings {
  auto_enabled: boolean
  target_account_id: number
  proxy_ids?: number[]
  base_interval_minutes: number
  max_interval_minutes: number
  max_candidates_per_round: number
}

export interface WindowHunterStatus {
  running: boolean
  rounds_since_hit: number
  next_run_at?: string
  settings: WindowHunterSettings
  runs: WindowHunterRunEntry[]
}

export async function getWindowHunterStatus(): Promise<WindowHunterStatus> {
  const { data } = await apiClient.get<WindowHunterStatus>('/admin/window-hunter/status')
  return data
}

export async function runWindowHunter(accountId?: number): Promise<{ run_id: string }> {
  const { data } = await apiClient.post<{ run_id: string }>('/admin/window-hunter/run', {
    account_id: accountId
  })
  return data
}

export async function getWindowHunterSettings(): Promise<WindowHunterSettings> {
  const { data } = await apiClient.get<WindowHunterSettings>('/admin/window-hunter/settings')
  return data
}

export async function updateWindowHunterSettings(
  settings: WindowHunterSettings
): Promise<WindowHunterSettings> {
  const { data } = await apiClient.put<WindowHunterSettings>('/admin/window-hunter/settings', settings)
  return data
}

// ---------------------------------------------------------------------------
// P2 满血会话池
// ---------------------------------------------------------------------------

export interface WindowSessionSnapshot {
  account_id: number
  conn_id: string
  proxy_id: number
  established_at: string
  full_power_until: string
  is_full_power: boolean
  last_sample_at?: string
  last_sample_answer: string
  degraded: boolean
  leased: boolean
  age_seconds: number
}

export interface WindowSessionSampleEvent {
  id: string
  account_id: number
  conn_id: string
  proxy_id: number
  at: string
  result: WindowProbeResult
  answer?: string
  message?: string
  retired: boolean
}

export interface WindowSessionPoolSnapshot {
  sessions: WindowSessionSnapshot[]
  recent_samples: WindowSessionSampleEvent[]
}

export interface WindowSessionPoolSettings {
  sampling_enabled: boolean
  sample_interval_seconds: number
  session_lifetime_minutes: number
  prewarm_count: number
  sample_fail_threshold: number
  gate_models: string[]
  reject_gated_when_no_full_power: boolean
}

export async function getWindowSessionPool(): Promise<WindowSessionPoolSnapshot> {
  const { data } = await apiClient.get<WindowSessionPoolSnapshot>('/admin/window-session-pool')
  return data
}

export async function prewarmWindowSessions(
  accountId: number,
  count?: number
): Promise<{ conn_ids: string[]; count: number }> {
  const { data } = await apiClient.post<{ conn_ids: string[]; count: number }>(
    '/admin/window-session-pool/prewarm',
    { account_id: accountId, count }
  )
  return data
}

export async function sampleWindowSession(
  accountId: number,
  connId: string
): Promise<WindowSessionSampleEvent> {
  const { data } = await apiClient.post<WindowSessionSampleEvent>('/admin/window-session-pool/sample', {
    account_id: accountId,
    conn_id: connId
  })
  return data
}

export async function getWindowSessionPoolSettings(): Promise<WindowSessionPoolSettings> {
  const { data } = await apiClient.get<WindowSessionPoolSettings>('/admin/window-session-pool/settings')
  return data
}

export async function updateWindowSessionPoolSettings(
  settings: WindowSessionPoolSettings
): Promise<WindowSessionPoolSettings> {
  const { data } = await apiClient.put<WindowSessionPoolSettings>(
    '/admin/window-session-pool/settings',
    settings
  )
  return data
}

// 代理拨号设置（P1：代理跳 TLS 校验豁免）
export interface ProxyDialSettings {
  insecure_skip_verify: boolean
}

export async function getProxyDialSettings(): Promise<ProxyDialSettings> {
  const { data } = await apiClient.get<ProxyDialSettings>('/admin/settings/proxy-dial')
  return data
}

export async function updateProxyDialSettings(
  settings: ProxyDialSettings
): Promise<ProxyDialSettings> {
  const { data } = await apiClient.put<ProxyDialSettings>('/admin/settings/proxy-dial', settings)
  return data
}

export type { WindowProbeHealth }
