import { computed, onUnmounted, ref, watch } from 'vue'
import { api, errorMessage } from '../api'
import { portsCanApply, validatePortsStatus, type PortsApplyInput, type PortsStatus } from './portsState'

/** 管理端口快照与一次性提交；断线仅通过 GET 核实，不自动重发 POST。
 * @param active 当前映射页是否激活。
 * @param revision 映射配置变更标记。
 * @returns 状态、忙标记与显式刷新/应用操作。
 */
export function usePorts(active: () => boolean, revision: () => number) {
  const status = ref<PortsStatus | null>(null)
  const loading = ref(false)
  const busy = ref(false)
  const error = ref('')
  const uncertain = ref(false)
  let disposed = false
  let generation = 0
  let timer: ReturnType<typeof setInterval> | undefined
  const blocked = computed(() => loading.value || busy.value || uncertain.value || !!error.value || !portsCanApply(status.value))

  /** 页面隐藏或未激活时不发起任何请求。@returns 是否允许读取状态。 */
  function visible(): boolean { return !disposed && active() && document.visibilityState === 'visible' }

  /** 查询真实状态，丢弃映射变更前的在途响应。@returns 本次查询结束。 */
  async function refresh(): Promise<void> {
    if (!visible() || loading.value || busy.value) return
    loading.value = true
    const requestedGeneration = generation
    try {
      const result = validatePortsStatus(await api.ports())
      if (!disposed && requestedGeneration === generation) {
        status.value = result
        error.value = ''
        uncertain.value = false
      }
    } catch (failure) {
      if (!disposed && requestedGeneration === generation) error.value = `${errorMessage(failure)} 连接恢复后仅刷新状态核实最终结果，不要重复提交。`
    } finally {
      if (!disposed) {
        loading.value = false
        if (requestedGeneration !== generation) void refresh()
      }
    }
  }

  /** 提交冻结版本的一次性确认；结果无论成功失败都由后续 GET 核实。
   * @param input 弹窗冻结的版本和用户授权，不接受端口集合。
   * @returns 提交与可见状态查询结束。
   */
  async function apply(input: PortsApplyInput): Promise<void> {
    if (!visible() || blocked.value || input.confirmation !== '应用端口变更' ||
      input.revision !== status.value?.revision || input.configRevision !== status.value?.configRevision ||
      (status.value.needsFinalize && !input.finalizePrevious)) return
    busy.value = true
    uncertain.value = true
    try {
      await api.applyPorts(input)
      if (!disposed && status.value) {
        status.value = { ...status.value, state: 'applying', canApply: false }
        error.value = ''
      }
    } catch (failure) {
      if (!disposed) error.value = `${errorMessage(failure)} 执行结果尚未确认，请刷新状态，不要重复提交。`
    } finally {
      if (!disposed) { busy.value = false; void refresh() }
    }
  }

  /** 按激活与文档可见性启停轮询。@returns 无。 */
  function synchronize(): void {
    if (timer) clearInterval(timer)
    timer = undefined
    if (visible()) { void refresh(); timer = setInterval(() => { void refresh() }, 5000) }
  }
  watch(revision, () => {
    generation++
    uncertain.value = true
    void refresh()
  })
  document.addEventListener('visibilitychange', synchronize)
  watch(active, synchronize, { immediate: true })
  onUnmounted(() => {
    disposed = true
    if (timer) clearInterval(timer)
    document.removeEventListener('visibilitychange', synchronize)
  })
  return { status, loading, busy, error, blocked, refresh, apply }
}
