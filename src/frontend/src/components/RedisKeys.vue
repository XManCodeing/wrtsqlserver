<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { api, errorMessage } from '../api'
import { useRedisKeys } from '../composables/useRedisKeys'
import type { RedisStatus, RedisValue } from '../types'

const props = defineProps<{ active: boolean; status: RedisStatus | null; statusError: string }>()
const emit = defineEmits<{ changed: [] }>()
const database = ref('db0')
const clearing = ref(false)
const confirmOpen = ref(false)
const confirmation = ref('')
const clearError = ref('')
const clearTarget = ref({ database: '', address: '' })
const detail = ref<RedisValue | null>(null)
const detailOpen = ref(false)
const enabled = computed(() => props.active && props.status?.state === 'online' && !props.statusError && !clearing.value)
const databases = computed(() => [...new Set(['db0', ...(props.status?.databases.map(item => item.name) || []), database.value])])
const { keys, rows, values, failures, pattern, page, nextCursor, scanned, loading, previewLoading, error, scan, reset, loadPreviews } = useRedisKeys(
  () => `${database.value}`, () => enabled.value,
)
const canConfirm = computed(() => confirmation.value === `清空 ${clearTarget.value.database}`
  && enabled.value && clearTarget.value.address === props.status?.address && clearTarget.value.database === database.value)

/** 显示TTL采样值。@param value 键预览。@returns 可读剩余时间。 */
function ttl(value?: RedisValue): string {
  if (!value) return '—'
  if (value.ttlSeconds === -2 || value.type === 'none') return '已不存在'
  return value.ttlSeconds === -1 ? '不过期' : `${value.ttlSeconds} 秒`
}

/** 展示已有预览，不再次请求或把值作为HTML渲染。@param value 当前值。@returns 无。 */
function showDetail(value?: RedisValue): void {
  if (!value) return
  detail.value = value; detailOpen.value = true
}

/** 从数据库概况定位键列表。@param name 已知库名。@returns 无。 */
function selectDatabase(name: string): void {
  if (!/^db(?:0|[1-9]\d{0,4})$/.test(name) || clearing.value) return
  database.value = name
}

/** 固定确认目标，避免确认期间状态切换导致误删。@returns 无。 */
function prepareClear(): void {
  if (!enabled.value) return
  clearTarget.value = { database: database.value, address: props.status?.address || '' }
  confirmation.value = ''; clearError.value = ''; confirmOpen.value = true
}

/** 仅发送一次清库请求，失败不得自动重试。@returns 清库请求完成。 */
async function clearDatabase(): Promise<void> {
  if (!canConfirm.value || clearing.value) return
  clearing.value = true; clearError.value = ''; reset()
  try {
    await api.clearRedis(clearTarget.value.database, confirmation.value, clearTarget.value.address)
    confirmOpen.value = false
    ElMessage.success('当前库已清空，后台内存释放可能仍在进行。')
    emit('changed')
  } catch (failure) {
    clearError.value = `${errorMessage(failure)} 请先刷新核实数据，不要直接重复清空。`
    confirmation.value = ''
  } finally { clearing.value = false }
}

watch(database, () => { detailOpen.value = false; detail.value = null; confirmOpen.value = false })
watch(() => props.status?.address, (address, previous) => {
  if (previous && address !== previous) { reset(); detailOpen.value = false; confirmOpen.value = false; if (enabled.value) void scan() }
})
watch(() => props.active, active => { if (!active) { detailOpen.value = false; detail.value = null; if (!clearing.value) confirmOpen.value = false } })
defineExpose({ selectDatabase })
</script>

<template>
  <section class="redis-keys-section">
    <div class="section-heading"><h2>键值列表</h2></div>
    <el-alert v-if="error" :title="error" type="error" :closable="false" show-icon class="global-error" />
    <div class="panel table-panel">
      <div class="table-toolbar redis-key-toolbar">
        <el-select v-model="database" class="status-filter" aria-label="选择 Redis 数据库" :disabled="clearing">
          <el-option v-for="name in databases" :key="name" :label="name" :value="name" />
        </el-select>
        <el-input v-model="pattern" class="search-input" placeholder="键名模式，例如 user:*" aria-label="键名筛选" clearable :disabled="!enabled" @keyup.enter="scan()" />
        <el-button type="primary" :loading="loading" :disabled="!enabled" @click="scan()">查询</el-button>
        <el-button :disabled="!enabled || loading || previewLoading || !rows.length" @click="loadPreviews">重试失败预览</el-button>
        <span class="muted toolbar-count">本批 {{ keys.length }} 个键</span>
        <el-button type="danger" plain :disabled="!enabled || loading" :loading="clearing" @click="prepareClear">清空当前库</el-button>
      </div>
      <el-table v-loading="loading" :data="rows" row-key="keyToken">
        <el-table-column prop="key" label="键" min-width="180" show-overflow-tooltip />
        <el-table-column label="类型" width="90"><template #default="{ row }">{{ values[row.keyToken]?.type || '—' }}</template></el-table-column>
        <el-table-column label="TTL" width="110"><template #default="{ row }">{{ ttl(values[row.keyToken]) }}</template></el-table-column>
        <el-table-column label="值预览" min-width="240"><template #default="{ row }">
          <span v-if="failures[row.keyToken]" class="danger-text">{{ failures[row.keyToken] }}</span>
          <span v-else-if="!values[row.keyToken]" class="muted">读取中…</span>
          <span v-else-if="values[row.keyToken]?.type === 'none'" class="muted">键已过期或删除</span>
          <template v-else><span class="value-inline">{{ values[row.keyToken]?.value || '（空值）' }}</span>
            <span v-if="values[row.keyToken]?.truncated" class="muted">（部分内容）</span>
            <span v-if="values[row.keyToken]?.encoding === 'base64'" class="muted">（Base64）</span>
          </template>
        </template></el-table-column>
        <el-table-column label="操作" width="80" fixed="right"><template #default="{ row }">
          <el-button link type="primary" :disabled="!values[row.keyToken]" @click="showDetail(values[row.keyToken])">查看</el-button>
        </template></el-table-column>
        <template #empty><el-empty :description="!enabled ? 'Redis 不可用，暂时无法读取' : !scanned ? '查询后显示键值' : nextCursor !== '0' ? '本批无匹配键，可继续扫描' : '未找到匹配键'" :image-size="68" /></template>
      </el-table>
      <div class="pagination redis-key-pagination">
        <span class="muted">{{ scanned && nextCursor === '0' ? '本轮扫描结束' : '分批扫描，不提供全库总页数' }}</span>
        <el-pagination v-model:current-page="page" :page-size="10" :total="keys.length" layout="prev, pager, next" :disabled="loading || !enabled" />
        <el-button :disabled="!enabled || loading || !scanned || nextCursor === '0'" @click="scan(false)">下一批</el-button>
      </div>
    </div>
    <p class="field-help">按需读取当前页的值，长值和集合只显示有限预览。扫描期间发生写入时，键可能重复或消失；TTL 为读取时的剩余秒数。</p>
    <el-dialog v-model="detailOpen" title="键值预览" width="720px">
      <template v-if="detail">
        <el-descriptions :column="2" border>
          <el-descriptions-item label="数据库">{{ detail.database }}</el-descriptions-item>
          <el-descriptions-item label="类型">{{ detail.type }}</el-descriptions-item>
          <el-descriptions-item label="TTL">{{ ttl(detail) }}</el-descriptions-item>
          <el-descriptions-item label="编码">{{ detail.encoding }}</el-descriptions-item>
          <el-descriptions-item label="键" :span="2"><span class="endpoint">{{ detail.key }}</span></el-descriptions-item>
        </el-descriptions>
        <p v-if="detail.truncated" class="field-help">仅显示部分内容，不是完整值。</p>
        <pre class="redis-value-preview">{{ detail.type === 'none' ? '键已过期或删除' : detail.value || '（空值）' }}</pre>
      </template>
      <template #footer><el-button @click="detailOpen = false">关闭</el-button></template>
    </el-dialog>
    <el-dialog v-model="confirmOpen" title="清空当前数据库" width="520px" :close-on-click-modal="false" :close-on-press-escape="!clearing" :show-close="!clearing">
      <el-alert title="此操作不可撤销，将删除选中库的全部键，而不是仅删除筛选结果。其他数据库不受影响。" type="warning" :closable="false" show-icon />
      <p class="clear-target">连接：<span class="endpoint">{{ clearTarget.address }}</span><br>数据库：<strong>{{ clearTarget.database }}</strong></p>
      <p class="field-help">正在运行的业务仍可能写入新键。输入“清空 {{ clearTarget.database }}”后确认。</p>
      <el-input v-model="confirmation" :disabled="clearing" :placeholder="`清空 ${clearTarget.database}`" aria-label="清空确认文字" class="clear-confirmation" />
      <el-alert v-if="clearError" :title="clearError" type="error" :closable="false" show-icon />
      <template #footer><el-button :disabled="clearing" @click="confirmOpen = false">取消</el-button><el-button type="danger" :disabled="!canConfirm" :loading="clearing" @click="clearDatabase">确认清空</el-button></template>
    </el-dialog>
  </section>
</template>

<style scoped>
.redis-keys-section { margin-top: 24px; }
.redis-keys-section > .section-heading { align-items: center; margin-bottom: 16px; }
.redis-keys-section > .section-heading h2 { margin-bottom: 0; }
.redis-key-toolbar { flex-wrap: wrap; }
.redis-key-pagination { gap: 16px; align-items: center; }
.redis-key-pagination > .muted { margin-right: auto; font-size: 12px; }
.value-inline { display: block; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 12px; }
.redis-value-preview { white-space: pre-wrap; overflow-wrap: anywhere; max-height: 360px; overflow: auto; padding: 16px; background: var(--app-bg); color: var(--app-text); border: 1px solid var(--app-border); border-radius: var(--app-radius); }
.clear-target { margin: 18px 0 8px; }
.clear-confirmation { margin: 12px 0 16px; }
</style>
