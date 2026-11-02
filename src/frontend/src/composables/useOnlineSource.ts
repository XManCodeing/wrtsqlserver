import { computed, onUnmounted, ref, watch } from 'vue'
import { api, errorMessage } from '../api'
import { checkedOnlineSource, validateOnlineSource, type OnlineUpdateSource } from './onlineSourceState'

/** 管理来源读取和独立草稿，不监听主题或状态轮询覆盖输入。@param active 页面激活。@param blocked 更新操作禁写条件。@param changed 保存完成回调。@returns 来源编辑模型。 */
export function useOnlineSource(active: () => boolean, blocked: () => boolean, changed: () => Promise<void>) {
  const source = ref<OnlineUpdateSource | null>(null)
  const draft = ref<OnlineUpdateSource>({ repositoryUrl: '', image: '' })
  const loading = ref(false)
  const saving = ref(false)
  const visible = ref(false)
  const error = ref('')
  const formError = ref('')
  const ready = ref(false)
  let disposed = false
  const disabled = computed(() => blocked() || loading.value || saving.value || !ready.value)

  /** 显式读取失败后禁写，重试不会替换已打开的草稿。@returns 读取结束。 */
  async function load(): Promise<void> {
    if (disposed || loading.value || saving.value || !active()) return
    loading.value = true
    ready.value = false
    try {
      const result = checkedOnlineSource(await api.onlineUpdateSource())
      if (!disposed) { source.value = result; ready.value = true; error.value = '' }
    } catch (failure) {
      if (!disposed) error.value = errorMessage(failure)
    } finally { if (!disposed) loading.value = false }
  }

  /** 只从已读成功的宿主配置创建草稿，绝不提交网页默认值。@returns 无。 */
  function open(): void {
    if (disabled.value || !source.value) return
    draft.value = { ...source.value }
    formError.value = ''
    visible.value = true
  }

  /** 保存一次，成功后失效版本缓存；连接失败要求先重新读取。@returns 保存结束。 */
  async function save(): Promise<void> {
    if (disabled.value || !visible.value) return
    formError.value = validateOnlineSource(draft.value)
    if (formError.value) return
    saving.value = true
    try {
      const result = checkedOnlineSource(await api.saveOnlineUpdateSource({ ...draft.value }))
      if (disposed) return
      source.value = result
      visible.value = false
      error.value = ''
      await changed()
    } catch (failure) {
      if (!disposed) { ready.value = false; error.value = errorMessage(failure); formError.value = error.value }
    } finally { if (!disposed) saving.value = false }
  }

  watch(active, value => { if (value && !source.value) void load() }, { immediate: true })
  onUnmounted(() => { disposed = true })
  return { source, draft, loading, saving, visible, error, formError, ready, disabled, load, open, save }
}
