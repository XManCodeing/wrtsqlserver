<script setup lang="ts">
import { computed, ref } from 'vue'
import { safeMessage } from '../api'
import type { ConnectivitySample, TestResult } from '../types'
import { connectivityBars, HISTORY_MINUTES } from '../composables/connectivityState'

const props = defineProps<{ samples: ConnectivitySample[]; now: number; enabled: boolean; loading: boolean; probing: boolean }>()
const bars = computed(() => connectivityBars(props.samples, props.now))
const selected = ref(HISTORY_MINUTES - 1)
const hovered = ref<'proxy' | 'target' | null>(null)
const tracks = [{ key: 'proxy', label: 'S5' }, { key: 'target', label: '目标' }] as const
const activeBar = computed(() => bars.value[selected.value]!)
const measured = computed(() => bars.value.filter(bar => bar.sample))
const successes = computed(() => measured.value.filter(bar => bar.state === 'success').length)
const summary = computed(() => {
  if (props.loading) return '加载中'
  if (props.probing) return '检测中'
  if (!measured.value.length) return props.enabled ? '等待采样' : '未启用'
  return `目标成功 ${successes.value}/${measured.value.length}`
})
const description = computed(() => {
  const bar = activeBar.value
  const time = new Date(bar.sample?.checkedAt ?? bar.time).toLocaleTimeString('zh-CN', { hour12: false })
  const result = hovered.value === 'proxy' ? bar.sample?.proxy : bar.sample
  const label = hovered.value === 'proxy' ? 'S5 建连与认证' : '经 S5 连接目标（总耗时）'
  if (!result) return `${time} · ${label} · 无探测记录`
  return `${time} · ${label} · ${result.ok ? '通畅' : '失败'} · ${result.latencyMs} ms${result.error ? ` · ${safeMessage(result.error)}` : ''}`
})

/** 缺失S5分段指标显示灰色，不从目标连接结果推断。@param result 实际阶段结果。@returns 语义样式。 */
function resultState(result?: TestResult): string {
  return result ? result.ok ? 'success' : 'failure' : 'unknown'
}

/** 根据指针位置选择分钟格；每条时间线仅使用一个浮层。
 * @param event 时间条上的指针事件。
 * @returns 无。
 */
function selectMinute(event: MouseEvent): void {
  const rect = (event.currentTarget as HTMLElement).getBoundingClientRect()
  selected.value = Math.max(0, Math.min(HISTORY_MINUTES - 1, Math.floor((event.clientX - rect.left) / rect.width * HISTORY_MINUTES)))
}

/** 使用左右方向键查看相邻分钟的状态。@param event 键盘事件。@returns 无。 */
function selectWithKeyboard(event: KeyboardEvent): void {
  if (!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) return
  event.preventDefault()
  if (event.key === 'Home') selected.value = 0
  else if (event.key === 'End') selected.value = HISTORY_MINUTES - 1
  else selected.value = Math.max(0, Math.min(HISTORY_MINUTES - 1, selected.value + (event.key === 'ArrowLeft' ? -1 : 1)))
}
</script>

<template>
  <div class="connectivity-history">
    <div v-for="track in tracks" :key="track.key" class="connectivity-track">
      <span class="track-label">{{ track.label }}</span>
      <el-tooltip :content="description" :visible="hovered === track.key" placement="top" :show-after="0">
        <div class="connectivity-bars" tabindex="0" role="group"
          :aria-label="`${track.label}最近一小时，每格一分钟。${summary}。左右方向键查看采样。${description}`"
          @mousemove="selectMinute" @mouseenter="hovered = track.key" @mouseleave="hovered = null"
          @focus="hovered = track.key" @blur="hovered = null" @keydown="selectWithKeyboard">
          <span v-for="(bar, index) in bars" :key="bar.time" class="connectivity-bar"
            :class="[track.key === 'proxy' ? resultState(bar.sample?.proxy) : bar.state,
              { selected: hovered === track.key && index === selected }]" aria-hidden="true" />
        </div>
      </el-tooltip>
    </div>
    <div class="connectivity-axis"><span>1 小时前</span><span>{{ summary }}</span><span>现在</span></div>
  </div>
</template>

<style scoped>
.connectivity-history { width: 100%; padding: 6px 0; }
.connectivity-track { display: grid; grid-template-columns: 26px minmax(0, 1fr); gap: 7px; align-items: center; }
.connectivity-track + .connectivity-track { margin-top: 5px; }
.track-label { color: var(--app-muted); font-size: 11px; line-height: 16px; }
.connectivity-bars { display: grid; grid-template-columns: repeat(60, minmax(0, 1fr)); gap: 2px; height: 16px; align-items: end; outline-offset: 3px; }
.connectivity-bars:focus-visible { outline: 1px solid var(--app-primary); }
.connectivity-bar { height: 100%; border-radius: 1px; background: var(--app-border); }
.connectivity-bar.success { background: var(--app-success); }
.connectivity-bar.failure { background: var(--app-danger); height: 65%; }
.connectivity-bar.unknown { height: 35%; }
.connectivity-bar.selected { outline: 1px solid var(--app-text); outline-offset: 1px; }
.connectivity-axis { display: flex; justify-content: space-between; gap: 8px; margin: 3px 0 0 33px; color: var(--app-muted); font-size: 10px; line-height: 16px; font-variant-numeric: tabular-nums; }
</style>
