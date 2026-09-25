<template>
  <BaseDialog
    :show="show"
    :title="t('admin.accounts.windowHunter.title')"
    width="wide"
    @close="handleClose"
  >
    <div class="space-y-5">
      <!-- 猎手状态 -->
      <div class="rounded-lg border border-gray-200 p-4 dark:border-dark-600">
        <div class="flex items-center justify-between">
          <h3 class="text-sm font-semibold text-gray-900 dark:text-gray-100">
            {{ t('admin.accounts.windowHunter.hunterSection') }}
          </h3>
          <button
            type="button"
            class="rounded-lg bg-primary-600 px-3 py-1.5 text-xs font-medium text-white hover:bg-primary-700 disabled:cursor-not-allowed disabled:opacity-60"
            :disabled="status?.running || starting"
            @click="handleRun"
          >
            {{ starting ? t('admin.accounts.windowHunter.starting') : t('admin.accounts.windowHunter.runNow') }}
          </button>
        </div>
        <div class="mt-3 grid grid-cols-1 gap-3 text-xs sm:grid-cols-3">
          <div>
            <p class="text-gray-500 dark:text-gray-400">{{ t('admin.accounts.windowHunter.state') }}</p>
            <p class="mt-1 font-medium" :class="status?.running ? 'text-amber-600 dark:text-amber-400' : 'text-gray-900 dark:text-gray-100'">
              {{ status?.running ? t('admin.accounts.windowHunter.hunting') : t('admin.accounts.windowHunter.idle') }}
            </p>
          </div>
          <div>
            <p class="text-gray-500 dark:text-gray-400">{{ t('admin.accounts.windowHunter.roundsSinceHit') }}</p>
            <p class="mt-1 font-medium text-gray-900 dark:text-gray-100">{{ status?.rounds_since_hit ?? '-' }}</p>
          </div>
          <div>
            <p class="text-gray-500 dark:text-gray-400">{{ t('admin.accounts.windowHunter.targetAccount') }}</p>
            <input
              v-model.number="targetAccountId"
              type="number"
              min="1"
              class="mt-1 w-28 rounded border border-gray-300 bg-white px-2 py-1 text-xs text-gray-900 dark:border-dark-600 dark:bg-dark-700 dark:text-gray-100"
            />
          </div>
        </div>
        <p v-if="status?.settings" class="mt-2 text-[11px] text-gray-400 dark:text-gray-500">
          {{ t('admin.accounts.windowHunter.autoHint', { enabled: status.settings.auto_enabled ? 'ON' : 'OFF', base: status.settings.base_interval_minutes, max: status.settings.max_interval_minutes, cap: status.settings.max_candidates_per_round }) }}
        </p>
      </div>

      <!-- 满血会话池 -->
      <div class="rounded-lg border border-gray-200 p-4 dark:border-dark-600">
        <div class="flex items-center justify-between">
          <h3 class="text-sm font-semibold text-gray-900 dark:text-gray-100">
            {{ t('admin.accounts.windowHunter.poolSection') }}
          </h3>
          <button
            type="button"
            class="rounded-lg bg-emerald-600 px-3 py-1.5 text-xs font-medium text-white hover:bg-emerald-700 disabled:cursor-not-allowed disabled:opacity-60"
            :disabled="!targetAccountId || prewarming"
            :title="t('admin.accounts.windowHunter.prewarmHint')"
            @click="handlePrewarm"
          >
            {{ prewarming ? t('admin.accounts.windowHunter.prewarming') : t('admin.accounts.windowHunter.prewarm') }}
          </button>
        </div>
        <p v-if="!sessions.length" class="mt-3 text-xs text-gray-400 dark:text-gray-500">
          {{ t('admin.accounts.windowHunter.poolEmpty') }}
        </p>
        <div v-else class="mt-3 space-y-2">
          <div
            v-for="session in sessions"
            :key="session.conn_id"
            class="flex items-center justify-between gap-2 rounded border border-gray-100 px-3 py-2 text-xs dark:border-dark-700"
          >
            <div class="min-w-0 flex-1">
              <span class="font-mono text-[11px] text-gray-700 dark:text-gray-300">{{ shortConnId(session.conn_id) }}</span>
              <span class="ml-2 text-gray-400">proxy:{{ session.proxy_id || 'direct' }}</span>
              <span v-if="session.last_sample_answer" class="ml-2 text-gray-500 dark:text-gray-400">
                {{ session.last_sample_answer }}
              </span>
            </div>
            <span :class="['badge text-[10px]', session.is_full_power ? 'badge-success' : session.degraded ? 'badge-danger' : 'badge-gray']">
              {{ session.is_full_power ? t('admin.accounts.windowHunter.sessionFullPower') : session.degraded ? t('admin.accounts.windowProbe.stateDegraded') : t('admin.accounts.windowProbe.stateUnknown') }}
            </span>
            <button
              type="button"
              class="rounded border border-gray-300 px-2 py-0.5 text-[11px] text-gray-600 hover:bg-gray-50 disabled:opacity-50 dark:border-dark-600 dark:text-gray-300 dark:hover:bg-dark-700"
              :disabled="samplingConnId === session.conn_id"
              @click="handleSample(session)"
            >
              {{ samplingConnId === session.conn_id ? t('admin.accounts.windowProbe.probing') : t('admin.accounts.windowHunter.sample') }}
            </button>
          </div>
        </div>
      </div>

      <!-- 运行日志 -->
      <div class="rounded-lg border border-gray-200 p-4 dark:border-dark-600">
        <h3 class="text-sm font-semibold text-gray-900 dark:text-gray-100">{{ t('admin.accounts.windowHunter.runLog') }}</h3>
        <p v-if="!runs.length" class="mt-3 text-xs text-gray-400 dark:text-gray-500">{{ t('admin.accounts.windowHunter.noRuns') }}</p>
        <div v-else class="mt-3 max-h-56 space-y-2 overflow-y-auto">
          <div
            v-for="run in runs"
            :key="run.id"
            class="rounded border border-gray-100 px-3 py-2 text-xs dark:border-dark-700"
          >
            <div class="flex items-center gap-2">
              <span :class="['badge text-[10px]', run.hit ? 'badge-success' : run.running ? 'badge-warning' : run.error ? 'badge-danger' : 'badge-gray']">
                {{ run.hit ? t('admin.accounts.windowHunter.runHit') : run.running ? t('admin.accounts.windowHunter.runRunning') : run.error ? t('admin.accounts.windowHunter.runError') : t('admin.accounts.windowHunter.runMiss') }}
              </span>
              <span class="text-gray-500 dark:text-gray-400">
                #{{ run.round }} · {{ run.trigger }} · {{ run.scanned }}/{{ run.total }}
              </span>
              <span class="ml-auto text-[11px] text-gray-400">{{ formatTime(run.started_at) }}</span>
            </div>
            <p v-if="run.hit" class="mt-1 text-emerald-600 dark:text-emerald-400">
              {{ t('admin.accounts.windowHunter.hitProxy', { proxy: run.hit_proxy_id }) }}
            </p>
            <p v-else-if="run.error" class="mt-1 text-red-500 dark:text-red-400">{{ run.error }}</p>
            <p v-else-if="run.next_run_at" class="mt-1 text-gray-400 dark:text-gray-500">
              {{ t('admin.accounts.windowHunter.nextRun', { time: formatTime(run.next_run_at) }) }}
            </p>
          </div>
        </div>
      </div>

      <p class="text-[11px] leading-4 text-gray-400 dark:text-gray-500">
        {{ t('admin.accounts.windowHunter.pollutionWarning') }}
      </p>
    </div>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { adminAPI } from '@/api/admin'
import type { WindowHunterStatus, WindowHunterRunEntry, WindowSessionSnapshot } from '@/api/admin'

const props = defineProps<{ show: boolean }>()
const emit = defineEmits<{ (e: 'close'): void }>()

const { t } = useI18n()

const status = ref<WindowHunterStatus | null>(null)
const sessions = ref<WindowSessionSnapshot[]>([])
const loading = ref(false)
const starting = ref(false)
const prewarming = ref(false)
const samplingConnId = ref<string | null>(null)
const targetAccountId = ref<number | null>(null)
let pollTimer: ReturnType<typeof setInterval> | null = null

const runs = computed<WindowHunterRunEntry[]>(() => status.value?.runs ?? [])

const refresh = async () => {
  try {
    const [hunterStatus, pool] = await Promise.all([
      adminAPI.windowHunter.getWindowHunterStatus(),
      adminAPI.windowHunter.getWindowSessionPool()
    ])
    status.value = hunterStatus
    sessions.value = pool.sessions
    if (!targetAccountId.value && hunterStatus.settings.target_account_id) {
      targetAccountId.value = hunterStatus.settings.target_account_id
    }
  } catch (error) {
    console.error('Failed to refresh window hunter status:', error)
  }
}

watch(
  () => props.show,
  (visible) => {
    if (visible) {
      loading.value = true
      refresh().finally(() => {
        loading.value = false
      })
      if (pollTimer === null) {
        pollTimer = setInterval(refresh, 4000)
      }
    } else if (pollTimer !== null) {
      clearInterval(pollTimer)
      pollTimer = null
    }
  },
  { immediate: true }
)

const handleRun = async () => {
  starting.value = true
  try {
    await adminAPI.windowHunter.runWindowHunter(targetAccountId.value ?? undefined)
    await refresh()
  } catch (error: any) {
    console.error('Failed to start hunt:', error)
  } finally {
    starting.value = false
  }
}

const handlePrewarm = async () => {
  if (!targetAccountId.value) return
  prewarming.value = true
  try {
    await adminAPI.windowHunter.prewarmWindowSessions(targetAccountId.value, 3)
    await refresh()
  } catch (error: any) {
    console.error('Failed to prewarm sessions:', error)
  } finally {
    prewarming.value = false
  }
}

const handleSample = async (session: WindowSessionSnapshot) => {
  samplingConnId.value = session.conn_id
  try {
    await adminAPI.windowHunter.sampleWindowSession(session.account_id, session.conn_id)
    await refresh()
  } catch (error: any) {
    console.error('Failed to sample session:', error)
  } finally {
    samplingConnId.value = null
  }
}

const shortConnId = (id: string) => id.replace(/^oa_ws_/, '').slice(0, 14)

const formatTime = (value?: string) => {
  if (!value) return '-'
  try {
    return new Date(value).toLocaleTimeString()
  } catch {
    return '-'
  }
}

const handleClose = () => emit('close')
</script>
