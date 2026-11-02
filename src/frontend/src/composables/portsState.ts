export interface PortsStatus {
  supported: boolean
  schemaVersion: 1
  revision: string
  configRevision: string
  desiredPorts: number[]
  appliedPorts: number[]
  actualPorts: number[]
  rulesReady: boolean
  state: 'idle' | 'applying' | 'failed' | 'manual_recovery'
  operationId: string
  message: string
  canApply: boolean
  needsFinalize: boolean
}

/** 仅提交版本与明确授权，端口集合始终由宿主计算。 */
export interface PortsApplyInput {
  revision: string
  configRevision: string
  confirmation: '应用端口变更'
  finalizePrevious: boolean
}

/** 校验接口边界的完整状态，拒绝未知协议或无效端口。
 * @param value 服务端响应。
 * @returns 已校验的状态。
 */
export function validatePortsStatus(value: unknown): PortsStatus {
  if (!value || typeof value !== 'object') throw new Error('端口状态响应无效，请刷新后重试。')
  const status = value as Record<string, unknown>
  const booleans = ['supported', 'rulesReady', 'canApply', 'needsFinalize']
  const strings = ['revision', 'configRevision', 'operationId', 'message']
  if (status.schemaVersion !== 1 || booleans.some(key => typeof status[key] !== 'boolean') ||
    strings.some(key => typeof status[key] !== 'string') ||
    !['idle', 'applying', 'failed', 'manual_recovery'].includes(String(status.state))) {
    throw new Error('端口状态协议不兼容，请检查宿主版本。')
  }
  for (const key of ['desiredPorts', 'appliedPorts', 'actualPorts']) {
    const ports = status[key]
    if (!Array.isArray(ports) || ports.some(port => !Number.isInteger(port) || port < 1 || port > 65535)) {
      throw new Error('端口状态包含无效端口，请检查宿主状态。')
    }
  }
  return value as PortsStatus
}

/** 根据真实发布集合计算变更，绝不在前端添加固定迁移端口。
 * @param status 宿主状态快照。
 * @returns 排序去重后的新增与移除集合。
 */
export function portsDiff(status: PortsStatus | null): { added: number[]; removed: number[] } {
  if (!status) return { added: [], removed: [] }
  const desired = new Set(status.desiredPorts)
  const actual = new Set(status.actualPorts)
  return {
    added: [...desired].filter(port => !actual.has(port)).sort((left, right) => left - right),
    removed: [...actual].filter(port => !desired.has(port)).sort((left, right) => left - right),
  }
}

/** 判断状态是否允许进入应用确认。
 * @param status 最近成功快照。
 * @returns 宿主支持且明确允许应用时为真。
 */
export function portsCanApply(status: PortsStatus | null): boolean {
  return !!status?.supported && !!status.canApply && !!status.revision && !!status.configRevision &&
    status.state !== 'applying' && status.state !== 'manual_recovery'
}

/** 格式化只读端口集合。
 * @param ports 服务端端口集合。
 * @returns 排序去重的端口文本，空集合明确显示。
 */
export function formatPorts(ports: number[]): string {
  return [...new Set(ports)].sort((left, right) => left - right).join('、') || '无'
}
