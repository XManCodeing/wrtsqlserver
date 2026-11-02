<script setup lang="ts">
import { computed, ref } from 'vue'
import RedisKeys from './RedisKeys.vue'
import { useRedisStatus } from '../composables/useRedisStatus'
import { redisBytes, redisState, redisUptime } from '../composables/redisState'

const props = defineProps<{ active: boolean }>()
const keyBrowser = ref<InstanceType<typeof RedisKeys>>()
const keySection = ref<HTMLElement>()

/** 定位选中库并滚动到键列表。@param name 库名。@returns 无。 */
function browse(name: string): void {
  keyBrowser.value?.selectDatabase(name)
  keySection.value?.scrollIntoView({ behavior: 'smooth', block: 'start' })
}
const { status, loading, error, refresh } = useRedisStatus(() => props.active)
const metrics = computed(() => status.value?.metrics)
const badge = computed(() => redisState(status.value))
const memoryLimit = computed(() => metrics.value?.maxMemoryBytes === 0 ? '未设置上限' : redisBytes(metrics.value?.maxMemoryBytes))
const memoryRatio = computed(() => {
  const used = metrics.value?.usedMemoryBytes
  const limit = metrics.value?.maxMemoryBytes
  return used != null && limit != null && limit > 0 ? `${(used / limit * 100).toFixed(1)}%` : '—'
})
const rdbStatus = computed(() => {
  const value = metrics.value
  if (value?.rdbBgsaveInProgress) return '正在生成快照'
  if (value?.rdbLastBgsaveStatus === 'ok') return '最近一次成功'
  if (value?.rdbLastBgsaveStatus === 'err') return '最近一次失败'
  return '—'
})

/** 格式化真实时间，不将缺失或无效时间显示为当前时间。@param value ISO时间或Unix秒数。@returns 本地时间或占位符。 */
function formatTime(value: string | number | null | undefined): string {
  if (value == null || value === 0) return '—'
  const date = new Date(typeof value === 'number' ? value * 1000 : value)
  return Number.isFinite(date.getTime()) ? date.toLocaleString('zh-CN', { hour12: false }) : '—'
}
</script>

<template>
  <section>
    <header class="page-heading"><div><h1>Redis 状态</h1><p>运行状态每 5 秒刷新；键值按需读取，可单独清空选中数据库。</p></div>
      <el-button :loading="loading" @click="refresh">刷新状态</el-button>
    </header>
    <el-alert v-if="error" :title="error" type="error" :closable="false" show-icon class="global-error" />
    <el-alert v-else-if="status?.state === 'unconfigured'" title="尚未配置 Redis 状态连接。部署时设置连接地址后即可查看，不代表 Redis 未运行。" type="info" :closable="false" show-icon class="global-error" />
    <el-alert v-else-if="status && status.state !== 'online'" :title="status.error || '暂时无法读取 Redis 状态，请检查服务和访问权限。'" type="warning" :closable="false" show-icon class="global-error" />
    <section v-loading="loading && !status" class="panel overview-section">
      <div class="section-heading"><h2>连接与运行</h2><el-tag :type="badge.type" effect="plain">{{ badge.label }}</el-tag></div>
      <el-descriptions :column="2" border>
        <el-descriptions-item label="连接地址"><span class="endpoint">{{ status?.address || '—' }}</span></el-descriptions-item>
        <el-descriptions-item label="采样时间">{{ formatTime(status?.checkedAt) }}</el-descriptions-item>
        <el-descriptions-item label="Redis 版本">{{ metrics?.version ?? '—' }}</el-descriptions-item>
        <el-descriptions-item label="运行时间">{{ redisUptime(metrics?.uptimeSeconds) }}</el-descriptions-item>
        <el-descriptions-item label="PING 延迟">{{ status?.latencyMs == null ? '—' : `${status.latencyMs} ms` }}</el-descriptions-item>
        <el-descriptions-item label="客户端连接数">{{ metrics?.connectedClients ?? '—' }}</el-descriptions-item>
        <el-descriptions-item label="每秒操作数">{{ metrics?.opsPerSecond ?? '—' }}</el-descriptions-item>
        <el-descriptions-item label="键总数">{{ metrics?.totalKeys ?? '—' }}</el-descriptions-item>
      </el-descriptions>
    </section>
    <section class="panel overview-section">
      <h2>内存与持久化</h2>
      <el-descriptions :column="2" border>
        <el-descriptions-item label="已用内存">{{ redisBytes(metrics?.usedMemoryBytes) }}</el-descriptions-item>
        <el-descriptions-item label="数据内存上限">{{ memoryLimit }}</el-descriptions-item>
        <el-descriptions-item label="内存占用比例">{{ memoryRatio }}</el-descriptions-item>
        <el-descriptions-item label="RDB 快照状态"><span :class="{ 'danger-text': metrics?.rdbLastBgsaveStatus === 'err' }">{{ rdbStatus }}</span></el-descriptions-item>
        <el-descriptions-item label="上次保存时间">{{ formatTime(metrics?.rdbLastSaveTime) }}</el-descriptions-item>
        <el-descriptions-item label="快照后变更数">{{ metrics?.rdbChangesSinceLastSave ?? '—' }}</el-descriptions-item>
        <el-descriptions-item label="AOF">{{ metrics?.aofEnabled == null ? '—' : metrics.aofEnabled ? '已启用' : '未启用' }}</el-descriptions-item>
      </el-descriptions>
      <p class="field-help">数据内存上限不同于容器内存限制。RDB 定期快照可能丢失最近的未保存修改。</p>
    </section>
    <div class="panel table-panel">
      <div class="table-toolbar"><h2 class="redis-table-title">数据库概况</h2><span class="muted toolbar-count">仅显示 Redis 返回的非空数据库</span></div>
      <el-table :data="status?.databases || []" row-key="name">
        <el-table-column prop="name" label="数据库" min-width="150" sortable />
        <el-table-column prop="keys" label="键数量" min-width="130" sortable />
        <el-table-column prop="expires" label="设置过期时间的键" min-width="160" sortable />
        <el-table-column label="操作" width="120"><template #default="{ row }"><el-button link type="primary" :disabled="status?.state !== 'online' || !!error" @click="browse(row.name)">查看键值</el-button></template></el-table-column>
        <template #empty><el-empty :description="status?.state === 'online' && metrics?.totalKeys === 0 ? '当前没有键' : '暂无数据库统计'" :image-size="68" /></template>
      </el-table>
    </div>
    <p class="field-help">连通性表示转发服务能访问 Redis，不等同于 Docker 容器状态；清库只删除选中库的键，不重启或重装 Redis。</p>
    <div ref="keySection"><RedisKeys ref="keyBrowser" :active="active" :status="status" :status-error="error" @changed="refresh" /></div>
  </section>
</template>

<style scoped>
.redis-table-title { margin: 0; }
</style>
