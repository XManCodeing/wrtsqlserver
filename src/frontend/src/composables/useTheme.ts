import { onMounted, onUnmounted, ref, watch } from 'vue'

export type ThemeMode = 'light' | 'dark' | 'system'
const storageKey = 'port-console-theme'

/** 建立唯一主题入口；仅根组件调用，不重新挂载任何编辑组件。
 * @returns 主题选择与存储失败提示。
 */
export function useTheme() {
  const mode = ref<ThemeMode>('light')
  const warning = ref('')
  const media = window.matchMedia('(prefers-color-scheme: dark)')
  try {
    const saved = localStorage.getItem(storageKey)
    if (saved === 'light' || saved === 'dark' || saved === 'system') mode.value = saved
  } catch { warning.value = '浏览器不允许保存主题，本次选择仍然有效。' }

  /** 同步全局主题标记。
   * @returns 无返回值。
   */
  function apply(): void {
    document.documentElement.classList.toggle('dark', mode.value === 'dark' || (mode.value === 'system' && media.matches))
  }
  watch(mode, () => {
    apply()
    try { localStorage.setItem(storageKey, mode.value) } catch {
      warning.value = '浏览器不允许保存主题，本次选择仍然有效。'
    }
  })
  onMounted(() => { apply(); media.addEventListener('change', apply) })
  onUnmounted(() => media.removeEventListener('change', apply))
  apply()
  return { mode, warning }
}
