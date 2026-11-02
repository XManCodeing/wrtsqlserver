export type OnlineUpdateState = 'idle' | 'checking' | 'available' | 'downloading' | 'downloaded' | 'applying' | 'succeeded' | 'rolled_back' | 'failed' | 'manual_recovery'

export interface OnlineRelease {
  version: string
  image: string
  digest: string
  platform: string
}

export interface OnlineUpdateStatus {
  enabled: boolean
  currentVersion: string
  latest: OnlineRelease | null
  stagedUpdates?: boolean
  prepared?: OnlineRelease | null
  downloadPercent?: number | null
  downloadedBytes?: number
  totalBytes?: number
  state: OnlineUpdateState
  phase: string
  message: string
  operationId: string
  updatedAt: string
  canApply: boolean
  canRollback: boolean
}

const stateLabels: Record<OnlineUpdateState, string> = {
  idle: '待检测', checking: '正在检测', available: '发现新版本', downloading: '正在下载镜像',
  applying: '正在重启并检查健康状态', downloaded: '已下载，待重启', succeeded: '更新成功', rolled_back: '已回退',
  failed: '更新失败', manual_recovery: '需要人工恢复',
}

/** 生成状态标签，仅服务端终态表示成功或失败。@param status 持久快照。@returns 展示文字。 */
export function onlinePhaseLabel(status: OnlineUpdateStatus | null): string {
  if (!status) return '正在读取'
  if (!status.enabled) return '未启用'
  return stateLabels[status.state] ?? '未知状态'
}

/** 仅显示宿主真实下载百分比；未知长度不制造进度。@param status 当前快照。@returns 百分比或 null。 */
export function onlineDownloadPercent(status: OnlineUpdateStatus | null): number | null {
  const value = status?.downloadPercent
  return typeof value === 'number' && Number.isFinite(value) && value >= 0 && value <= 100
    ? Math.floor(value) : null
}

/** 格式化真实传输字节；缺少统计时不宣称下载大小。@param bytes 字节计数。@returns 简短大小文本。 */
export function onlineTransferSize(bytes: number | undefined): string {
  if (typeof bytes !== 'number' || !Number.isFinite(bytes) || bytes < 0) return ''
  if (bytes < 1024) return `${Math.floor(bytes)} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  return `${(bytes / 1024 / 1024).toFixed(1)} MB`
}

/** 用户打开弹窗时才检测，已有任务或待重启包不被自动检测覆盖。@param status 当前快照。@returns 是否可自动检测。 */
export function canCheckOnOpen(status: OnlineUpdateStatus | null): boolean {
  return !!status?.enabled && !onlineOperationActive(status) && !status.prepared && status.state !== 'manual_recovery'
}

/** 判断更新器是否忙碌。@param status 当前快照。@returns 是否正在执行操作。 */
export function onlineOperationActive(status: OnlineUpdateStatus | null): boolean {
  return !!status && ['checking', 'downloading', 'applying'].includes(status.state)
}

/** 轮询仅限活动且可见页面，不自动执行检测或更新。@param active 当前导航激活。@param visibility 文档可见性。@returns 是否轮询。 */
export function shouldPollOnline(active: boolean, visibility: string): boolean {
  return active && visibility === 'visible'
}

/** 保留最后持久快照，断联绝不伪造失败终态。@param status 最后快照。@returns 面向用户的连接提示。 */
export function onlineDisconnectedMessage(status: OnlineUpdateStatus | null): string {
  return onlineOperationActive(status)
    ? '暂时无法读取状态，切换版本期间连接可能中断。请等待恢复或重新登录，最终结果以恢复后的状态为准。'
    : '暂时无法读取更新状态，操作结果尚未确认。请恢复连接后刷新，不要重复提交。'
}
