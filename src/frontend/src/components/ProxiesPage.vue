<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api, errorMessage } from '../api'
import { endpoint } from '../validation'
import type { MappingRecord, ProxyRecord } from '../types'
import ProxyDialog from './ProxyDialog.vue'

const props = defineProps<{ proxies: ProxyRecord[]; mappings: MappingRecord[]; loading: boolean }>()
const emit = defineEmits<{ changed: [] }>()
const editor = ref<InstanceType<typeof ProxyDialog>>()
const query = ref('')
const page = ref(1)
const busy = ref<Record<string, boolean>>({})
const filtered = computed(() => props.proxies.filter(proxy => `${proxy.name} ${proxy.host}`.toLowerCase().includes(query.value.toLowerCase())))
const rows = computed(() => filtered.value.slice((page.value - 1) * 10, page.value * 10))
// 保持删除后的页码处于有效范围。
watch(() => filtered.value.length, total => { page.value = Math.min(page.value, Math.max(1, Math.ceil(total / 10))) })

/** 统计代理引用，阻止误删仍在使用的代理。
 * @param id 代理标识。
 * @returns 引用映射数量。
 */
function referenceCount(id: string): number { return props.mappings.filter(record => record.proxyId === id).length }

/** 确认后删除无引用代理，服务端仍进行最终校验。
 * @param proxy 目标代理。
 * @returns 删除完成后的 Promise。
 */
async function remove(proxy: ProxyRecord): Promise<void> {
  if (busy.value[proxy.id] || referenceCount(proxy.id)) return
  busy.value[proxy.id] = true
  try {
    try { await ElMessageBox.confirm(`确定删除代理“${proxy.name}”？此操作无法恢复。`, '删除代理', { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消' }) }
    catch { return }
    await api.deleteProxy(proxy.id)
    ElMessage.success('代理已删除')
    emit('changed')
  } catch (failure) { ElMessage.error(errorMessage(failure)) }
  finally { delete busy.value[proxy.id] }
}

/** 从无代理引导中打开新增弹窗。
 * @returns 无返回值。
 */
function create(): void { editor.value?.open() }
defineExpose({ create })
</script>

<template>
  <section>
    <header class="page-heading"><div><h1>SOCKS5 代理</h1><p>配置转发出口。密码只写入，不回显。</p></div><el-button type="primary" :disabled="loading" @click="create">新增代理</el-button></header>
    <div class="panel table-panel">
      <div class="table-toolbar"><el-input v-model="query" class="search-input" placeholder="搜索代理名称或主机" clearable aria-label="搜索代理" @input="page = 1" /><span class="muted toolbar-count">{{ filtered.length }} 个代理</span></div>
      <el-table v-loading="loading" :data="rows" row-key="id">
        <el-table-column prop="name" label="名称" min-width="160" sortable show-overflow-tooltip />
        <el-table-column label="SOCKS5 地址" min-width="220"><template #default="{ row }"><span class="endpoint">{{ endpoint(row.host, row.port) }}</span></template></el-table-column>
        <el-table-column prop="username" label="用户名" min-width="140" show-overflow-tooltip><template #default="{ row }">{{ row.username || '未设置' }}</template></el-table-column>
        <el-table-column label="密码" width="110"><template #default="{ row }"><el-tag type="info" effect="plain">{{ row.passwordSet ? '已设置' : '未设置' }}</el-tag></template></el-table-column>
        <el-table-column label="引用映射" width="110"><template #default="{ row }">{{ referenceCount(row.id) }}</template></el-table-column>
        <el-table-column label="操作" width="140" fixed="right"><template #default="{ row }"><div class="row-actions">
          <el-button link type="primary" :disabled="busy[row.id]" @click="editor?.open(row)">编辑</el-button>
          <el-tooltip content="先移除或更换引用此代理的映射" :disabled="!referenceCount(row.id)"><span><el-button link type="danger" :disabled="!!referenceCount(row.id) || busy[row.id]" @click="remove(row)">删除</el-button></span></el-tooltip>
        </div></template></el-table-column>
        <template #empty><el-empty :description="query ? '没有符合条件的代理' : '暂无代理，添加 SOCKS5 出口后即可创建映射'" :image-size="68" /></template>
      </el-table>
      <div class="pagination"><el-pagination v-model:current-page="page" :page-size="10" :total="filtered.length" layout="total, prev, pager, next" /></div>
    </div>
    <p class="field-help">测试入口位于端口映射列表，用于检查完整转发链路。</p>
    <ProxyDialog ref="editor" @saved="emit('changed')" />
  </section>
</template>
