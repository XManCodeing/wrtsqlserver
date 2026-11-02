<script setup lang="ts">
import { computed } from 'vue'
import { safeMessage } from '../api'
import type { MappingRecord, ServiceStatus } from '../types'

const props = defineProps<{ status: ServiceStatus | null; mappings: MappingRecord[]; proxyCount: number; loading: boolean }>()
const emit = defineEmits<{ navigate: [page: 'mappings' | 'proxies'] }>()
const failures = computed(() => props.mappings.filter(record => record.status === 'error'))
const uptime = computed(() => {
  if (!props.status) return '—'
  const seconds = Math.max(0, Math.floor(props.status.uptimeSeconds))
  return `${Math.floor(seconds / 86400)} 天 ${Math.floor(seconds % 86400 / 3600)} 小时 ${Math.floor(seconds % 3600 / 60)} 分钟`
})
</script>

<template>
  <section>
    <header class="page-heading"><div><h1>运行概览</h1><p>服务运行状态与连接摘要，每 5 秒更新。</p></div><el-button type="primary" @click="emit('navigate', 'mappings')">管理映射</el-button></header>
    <div v-loading="loading" class="metrics">
      <article class="metric panel"><span>运行中的映射</span><strong>{{ status?.runningMappings ?? '—' }}<small>/ {{ mappings.length }}</small></strong></article>
      <article class="metric panel"><span>活动连接</span><strong>{{ status?.activeConnections ?? '—' }}</strong></article>
      <article class="metric panel"><span>SOCKS5 代理</span><strong>{{ proxyCount }}</strong></article>
      <article class="metric panel"><span>异常映射</span><strong :class="{ 'danger-text': failures.length }">{{ failures.length }}</strong></article>
    </div>
    <section class="panel overview-section">
      <h2>服务信息</h2>
      <el-descriptions :column="2" border>
        <el-descriptions-item label="运行时间">{{ uptime }}</el-descriptions-item>
        <el-descriptions-item label="服务版本">{{ status?.version || '—' }}</el-descriptions-item>
        <el-descriptions-item label="转发协议">TCP → SOCKS5</el-descriptions-item>
        <el-descriptions-item label="配置生效方式">按映射应用，不重启容器</el-descriptions-item>
      </el-descriptions>
    </section>
    <section class="panel overview-section">
      <div class="section-heading"><h2>{{ failures.length ? '需要关注' : '配置与状态' }}</h2><el-button link type="primary" @click="emit('navigate', proxyCount ? 'mappings' : 'proxies')">{{ proxyCount ? '查看映射' : '配置代理' }}</el-button></div>
      <el-table v-if="failures.length" :data="failures" row-key="id"><el-table-column prop="name" label="映射" width="200" /><el-table-column label="错误信息"><template #default="{ row }">{{ safeMessage(row.lastError || '运行异常，请检查配置') }}</template></el-table-column></el-table>
      <p v-else class="muted">{{ !status ? '正在等待服务状态。' : !proxyCount ? '从创建 SOCKS5 代理开始，然后添加端口映射。' : !mappings.length ? '代理已就绪，创建映射并手动启用即可开始转发。' : '当前没有异常映射。已启用映射每分钟检查连通性，历史见端口映射。' }}</p>
    </section>
  </section>
</template>
