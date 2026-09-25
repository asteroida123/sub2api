<template>
  <button
    type="button"
    :class="['badge text-xs cursor-pointer whitespace-nowrap', badgeClass, probing && 'opacity-60']"
    :title="tooltipText"
    :disabled="probing"
    @click.stop="$emit('probe')"
  >
    <span v-if="probing">{{ t('admin.accounts.windowProbe.probing') }}</span>
    <template v-else>
      <span>{{ label }}</span>
      <span v-if="countdownText" class="ml-1 font-normal opacity-80">{{ countdownText }}</span>
    </template>
  </button>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import type { WindowProbeHealth } from '@/api/admin'

const props = defineProps<{
  /** null = 该账号从未探测过 */
  health: WindowProbeHealth | null
  probing?: boolean
  /** 健康数据抓取时刻（ms），用于本地走秒修正剩余时间 */
  generatedAtMs?: number
}>()

defineEmits<{
  (e: 'probe'): void
}>()

const { t } = useI18n()

// 倒计时只在有活动窗口/冷却时走秒
const nowTick = ref(Date.now())
let timer: ReturnType<typeof setInterval> | null = null

const hasCountdown = computed(
  () => props.health?.state === 'cooldown' || props.health?.state === 'full_power'
)

watch(
  hasCountdown,
  (active) => {
    if (active && timer === null) {
      timer = setInterval(() => {
        nowTick.value = Date.now()
      }, 1000)
    } else if (!active && timer !== null) {
      clearInterval(timer)
      timer = null
    }
  },
  { immediate: true }
)

onBeforeUnmount(() => {
  if (timer !== null) clearInterval(timer)
})

const badgeClass = computed(() => {
  switch (props.health?.state) {
    case 'full_power':
      return 'badge-success'
    case 'cooldown':
      return 'badge-warning'
    case 'degraded':
      return 'badge-danger'
    default:
      return 'bg-gray-100 text-gray-500 dark:bg-dark-600 dark:text-gray-400'
  }
})

const label = computed(() => {
  switch (props.health?.state) {
    case 'full_power':
      return t('admin.accounts.windowProbe.stateFullPower')
    case 'cooldown':
      return t('admin.accounts.windowProbe.stateCooldown')
    case 'degraded':
      return t('admin.accounts.windowProbe.stateDegraded')
    default:
      return t('admin.accounts.windowProbe.stateUnknown')
  }
})

function formatDuration(totalSeconds: number): string {
  const seconds = Math.max(0, Math.floor(totalSeconds))
  const h = Math.floor(seconds / 3600)
  const m = Math.floor((seconds % 3600) / 60)
  const s = seconds % 60
  if (h > 0) return `${h}h${String(m).padStart(2, '0')}m`
  if (m > 0) return `${m}m${String(s).padStart(2, '0')}s`
  return `${s}s`
}

const countdownText = computed(() => {
  const health = props.health
  if (!health) return ''
  if (health.state === 'cooldown' && health.cooldown_remaining_seconds != null) {
    return formatDuration(health.cooldown_remaining_seconds)
  }
  if (health.state === 'full_power' && health.window_remaining_seconds != null) {
    const serverRemain = health.window_remaining_seconds
    const elapsed = Math.floor((nowTick.value - (props.generatedAtMs ?? nowTick.value)) / 1000)
    return formatDuration(serverRemain - elapsed)
  }
  return ''
})

const tooltipText = computed(() => {
  const health = props.health
  if (!health) return t('admin.accounts.windowProbe.probeNowHint')
  const parts: string[] = [t('admin.accounts.windowProbe.probeNowHint')]
  if (health.last_probe_at) {
    parts.push(
      t('admin.accounts.windowProbe.lastProbe', {
        time: new Date(health.last_probe_at * 1000).toLocaleString(),
        count: health.probe_count
      })
    )
  }
  if (health.last_probe_answer) {
    parts.push(t('admin.accounts.windowProbe.lastAnswer', { answer: health.last_probe_answer }))
  }
  return parts.join('\n')
})
</script>
