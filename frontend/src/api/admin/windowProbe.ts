/**
 * Admin Window Probe API endpoints
 * 窗口猎手 P0：账号×出口节点降智指纹探测与健康状态
 */

import { apiClient } from '../client'

export type WindowProbeResult = 'full_power' | 'degraded' | 'error'

export type WindowProbeHealthState = 'unknown' | 'full_power' | 'degraded' | 'cooldown'

export interface WindowProbeHealth {
  account_id: number
  proxy_id: number
  region: string
  /** 派生态：满血窗口内 / 冷却倒计时中已折算 */
  state: WindowProbeHealthState
  stored_state: string
  window_opened_at?: number
  window_remaining_seconds?: number
  degraded_at?: number
  cooldown_until?: number
  cooldown_remaining_seconds?: number
  probe_count: number
  last_probe_at?: number
  last_probe_answer: string
}

export interface WindowProbeHealthList {
  items: WindowProbeHealth[]
  generated_at: number
  window_duration_seconds: number
}

export interface WindowProbeOutcome {
  result: WindowProbeResult
  answer: string
  latency_ms: number
  question_id: string
  question: string
  message?: string
  state: string
}

export interface WindowProbeQuestion {
  id: string
  text: string
  full_power_keywords: string[]
  degraded_keywords: string[]
}

export interface WindowProbeSettings {
  model: string
  questions: WindowProbeQuestion[]
  cooldown_minutes: number
  min_probe_interval_seconds: number
  window_duration_seconds: number
  probe_timeout_seconds: number
}

/**
 * List window probe health states (panel badge data source)
 * @param accountIds - Optional account id filter
 */
export async function getWindowProbeHealth(accountIds?: number[]): Promise<WindowProbeHealthList> {
  const query = accountIds?.length ? `?account_ids=${accountIds.join(',')}` : ''
  const { data } = await apiClient.get<WindowProbeHealthList>(`/admin/window-probe/health${query}`)
  return data
}

/**
 * Run a single fingerprint probe against an account
 * 探测即污染：一次探测会把该 (账号,出口) 的窗口时钟归零
 */
export async function probeAccountWindow(
  accountId: number,
  options?: { proxy_id?: number; force?: boolean }
): Promise<WindowProbeOutcome> {
  const { data } = await apiClient.post<WindowProbeOutcome>(`/admin/accounts/${accountId}/window-probe`, {
    proxy_id: options?.proxy_id,
    force: options?.force ?? false
  })
  return data
}

/** Get window probe settings (question bank, cooldown, intervals) */
export async function getWindowProbeSettings(): Promise<WindowProbeSettings> {
  const { data } = await apiClient.get<WindowProbeSettings>('/admin/window-probe/settings')
  return data
}

/** Update window probe settings */
export async function updateWindowProbeSettings(settings: WindowProbeSettings): Promise<WindowProbeSettings> {
  const { data } = await apiClient.put<WindowProbeSettings>('/admin/window-probe/settings', settings)
  return data
}
