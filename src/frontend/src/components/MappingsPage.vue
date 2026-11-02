<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api, errorMessage, safeMessage } from '../api'
import { endpoint, mappingInput } from '../validation'
import type { MappingRecord, ProxyRecord } from '../types'
import { useConnectivity } from '../composables/useConnectivity'
import MappingDialog from './MappingDialog.vue'
import ConnectivityHistory from './ConnectivityHistory.vue'

const props = defineProps<{ mappings: MappingRecord[]; proxies: ProxyRecord[]; loading: boolean; active: boolean; proxyRevision: number }>()
const emit = defineEmits<{ changed: []; createProxy: [] }>()
const editor = ref<InstanceType<typeof MappingDialog>>()
const query = ref('')
const filter = ref('all')
const page = ref(1)
const busy = ref<Record<string, string>>({})
const testResults = ref<Record<string, string>>({})
const operationError = ref('')
const filtered = computed(() => props.mappings.filter(record =>
  `${record.name} ${record.listenHost} ${record.targetHost}`.toLowerCase().includes(query.value.toLowerCase()) &&
  (filter.value === 'all' || record.status === filter.value)))
const rows = computed(() => filtered.value.slice((page.value - 1) * 10, page.value * 10))
const { histories, now, error: historyError, loading: historyLoading, probing, probe, refresh: refreshHistory } = useConnectivity({
  active: () => props.active && !props.loading,
  revision: () => JSON.stringify([props.proxyRevision, props.proxies,
    props.mappings.map(({ activeConnections, lastError, status, ...config }) => config)]),
})
const statusLabels = { running: '运行中', stopped: '已停止', error: '异常' }
// 删除或轮询导致总数缩小时，避免分页停在不存在的页。
watch(() => filtered.value.length, total => { page.value = Math.min(page.value, Math.max(1, Math.ceil(total / 10))) })

/** 查找当前代理显示名称。
 * @param id 代理标识。
 * @returns 名称或不可用提示。
 */
function proxyName(id: string): string { return props.proxies.find(proxy => proxy.id === id)?.name ?? '代理不可用' }

/** 执行明确授权的单映射操作，不重启全局服务。
 * @param record 目标映射。
 * @param action 行内操作类型。
 * @returns 操作完成后的 Promise。
 */
async function act(record: MappingRecord, action: 'toggle' | 'delete' | 'test'): Promise<void> {
  if (busy.value[record.id]) return
  busy.value[record.id] = action
  try {
    const title = action === 'test' ? '测试连通性' : action === 'delete' ? '删除映射' : record.enabled ? '停用映射' : '启用映射'
    const message = action === 'test'
      ? '将通过所选 SOCKS5 代理建立一次目标 TCP 连接，不发送数据库查询。确认测试？'
      : action === 'delete' ? '删除后无法恢复，该映射已有连接将受影响。其他映射和容器不重启。'
        : '此操作仅重新应用该映射，已有连接可能中断；其他映射和容器不重启。'
    try { await ElMessageBox.confirm(message, title, { confirmButtonText: title, cancelButtonText: '取消', type: action === 'test' ? 'info' : 'warning' }) }
    catch { return }
    operationError.value = ''
    if (action === 'test') {
      const result = await probe(record)
      if (!result) return
      testResults.value[record.id] = result.ok ? `连接成功 · ${result.latencyMs} ms` : `测试失败：${safeMessage(result.error ?? '目标连接未成功')}`
      if (result.ok) ElMessage.success(testResults.value[record.id])
      else ElMessage.error(testResults.value[record.id])
    } else {
      if (action === 'delete') await api.deleteMapping(record.id)
      else {
        const saved = await api.saveMapping({ ...mappingInput(record), enabled: !record.enabled }, record.id)
        if (saved.status === 'error') {
          operationError.value = `${record.name}：配置已保存，但监听启动失败。${safeMessage(saved.lastError || '请检查监听地址和端口占用。')}`
          emit('changed')
          return
        }
      }
      ElMessage.success(action === 'delete' ? '映射已删除' : record.enabled ? '映射已停用' : '映射已启用')
      emit('changed')
    }
  } catch (failure) {
    const message = errorMessage(failure)
    if (action === 'test') testResults.value[record.id] = message
    operationError.value = `${record.name}：${message}`
    ElMessage.error(message)
  } finally { delete busy.value[record.id] }
}
</script>

<template>
  <section>
    <header class="page-heading"><div><h1>端口映射</h1><p>将本地 TCP 入口通过 SOCKS5 转发至指定目标。</p></div>
      <el-button type="primary" :disabled="!proxies.length || loading" @click="editor?.open()">新增映射</el-button>
    </header>
    <el-alert v-if="!proxies.length && !loading" type="info" :closable="false" show-icon class="setup-alert">
      <template #title>先配置 SOCKS5 代理，再创建端口映射。</template>
      <el-button link type="primary" @click="emit('createProxy')">创建代理</el-button>
    </el-alert>
    <el-alert v-if="operationError" :title="operationError" type="error" show-icon class="global-error" @close="operationError = ''" />
    <el-alert v-if="historyError" type="warning" :closable="false" show-icon class="global-error">
      <template #title>历史刷新失败：{{ historyError }} <el-button link type="primary" @click="refreshHistory">重试</el-button></template>
    </el-alert>
    <div class="panel table-panel">
      <div class="table-toolbar">
        <el-input v-model="query" placeholder="搜索名称、入口或目标" clearable class="search-input" aria-label="搜索映射" @input="page = 1" />
        <el-select v-model="filter" class="status-filter" aria-label="筛选映射状态" @change="page = 1">
          <el-option label="全部状态" value="all" /><el-option label="运行中" value="running" /><el-option label="已停止" value="stopped" /><el-option label="异常" value="error" />
        </el-select>
        <el-tooltip content="后台每分钟检测已启用映射，关闭网页仍记录；只建立 TCP 连接，不执行数据库查询。">
          <span class="muted history-cadence">后台每分钟探测</span>
        </el-tooltip>
        <span class="muted toolbar-count">{{ filtered.length }} 条映射</span>
      </div>
      <el-table v-loading="loading" :data="rows" row-key="id" table-layout="fixed">
        <el-table-column prop="name" label="名称" min-width="115" sortable show-overflow-tooltip />
        <el-table-column label="本地入口 → 目标" min-width="170"><template #default="{ row }"><div class="endpoint">{{ endpoint(row.listenHost, row.listenPort) }}</div><div class="endpoint muted">→ {{ endpoint(row.targetHost, row.targetPort) }}</div></template></el-table-column>
        <el-table-column label="代理" width="64" class-name="compact-column" show-overflow-tooltip><template #default="{ row }">{{ proxyName(row.proxyId) }}</template></el-table-column>
        <el-table-column label="状态" width="76" class-name="compact-column">
          <template #default="{ row }"><el-tooltip :content="safeMessage(row.lastError || '无错误')" :disabled="!row.lastError"><el-tag :type="row.status === 'running' ? 'success' : row.status === 'error' ? 'danger' : 'info'" effect="plain">{{ statusLabels[row.status as keyof typeof statusLabels] || '未知' }}</el-tag></el-tooltip></template>
        </el-table-column>
        <el-table-column label="连通性 · 最近 1 小时" width="520" class-name="history-column">
          <template #default="{ row }">
            <ConnectivityHistory :samples="histories[row.id] || []" :now="now" :enabled="row.enabled"
              :loading="historyLoading" :probing="probing === row.id" />
          </template>
        </el-table-column>
        <el-table-column label="连接" width="74"><template #default="{ row }">{{ row.activeConnections }} / {{ row.maxConnections }}</template></el-table-column>
        <el-table-column label="操作" width="190" fixed="right">
          <template #default="{ row }"><div class="row-actions">
            <el-button link type="primary" :disabled="!!busy[row.id]" @click="editor?.open(row)">编辑</el-button>
            <el-tooltip :content="testResults[row.id] || '手动测试目标连接'"><el-button link type="primary" :loading="busy[row.id] === 'test' || probing === row.id" :disabled="!!busy[row.id] || !!probing" @click="act(row, 'test')">测试</el-button></el-tooltip>
            <el-button link type="primary" :loading="busy[row.id] === 'toggle'" :disabled="!!busy[row.id]" @click="act(row, 'toggle')">{{ row.enabled ? '停用' : '启用' }}</el-button>
            <el-button link type="danger" :disabled="!!busy[row.id]" @click="act(row, 'delete')">删除</el-button>
          </div></template>
        </el-table-column>
        <template #empty><el-empty :description="query || filter !== 'all' ? '没有符合条件的映射' : '暂无映射，创建后可手动启用'" :image-size="68" /></template>
      </el-table>
      <div class="pagination"><el-pagination v-model:current-page="page" :page-size="10" :total="filtered.length" layout="total, prev, pager, next" /></div>
    </div>
    <p class="field-help connectivity-legend">
      <span><i class="legend-success" />通畅</span><span><i class="legend-failure" />阻塞</span><span><i class="legend-unknown" />无记录</span>
      每格 1 分钟；S5 为代理建连及认证，目标为经代理连接目标端口。悬停查看耗时。停用暂停采样，配置修改或重启后重新积累。
    </p>
    <MappingDialog ref="editor" :proxies="proxies" @saved="emit('changed')" />
  </section>
</template>
