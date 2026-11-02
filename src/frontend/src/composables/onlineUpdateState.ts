export type OnlineUpdateState = 'idle' | 'checking' | 'available' | 'downloading' | 'applying' | 'succeeded' | 'rolled_back' | 'failed' | 'manual_recovery'

export interface OnlineUpdateStatus {
  enabled: boolean
  currentVersion: string
  latest: { version: string; image: string; digest: string; platform: string } | null
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
  applying: '正在切换并检查健康状态', succeeded: '更新成功', rolled_back: '已回退',
  failed: '更新失败', manual_recovery: '需要人工恢复',
}

/** 生成状态标签，仅服务端终态表示成功或失败。@param status 持久快照。@returns 展示文字。 */
export function onlinePhaseLabel(status: OnlineUpdateStatus | null): string {
  if (!status) return '正在读取'
  if (!status.enabled) return '未启用'
  return stateLabels[status.state] ?? '未知状态'
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
