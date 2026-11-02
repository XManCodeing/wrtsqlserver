import type { PortsApplyInput, PortsStatus } from './composables/portsState'
import type { OnlineUpdateSource } from './composables/onlineSourceState'
import type { OnlineUpdateStatus } from './composables/onlineUpdateState'
import type { ConnectivityHistory, MappingInput, MappingRecord, ProxyInput, ProxyRecord, ServiceStatus, Session, TestResult, UpdatePackage, UpdateStatus, RedisStatus, RedisKeys, RedisValue } from './types'

let csrfToken = ''

/** 设置内存中的 CSRF token。
 * @param token 当前会话 token，空字符串用于注销。
 * @returns 无返回值。
 */
export function setCsrfToken(token: string): void {
  csrfToken = token
}

/** 过滤接口诊断中可能存在的凭据。
 * @param message 服务端或本地错误文本。
 * @param secrets 本次提交的敏感值。
 * @returns 可安全显示的简短诊断。
 */
export function safeMessage(message: string, secrets: string[] = []): string {
  let sanitized = message.replace(/([a-z][a-z0-9+.-]*:\/\/)[^\s/@]+@/gi, '$1[已隐藏]@')
    .replace(/((?:password|token|secret|authorization)\s*[=:]\s*)[^\s,;]+/gi, '$1[已隐藏]')
  for (const secret of secrets) {
    if (secret) sanitized = sanitized.split(secret).join('[已隐藏]')
  }
  return sanitized.slice(0, 400)
}

/** 执行同源 API 请求，统一超时、会话过期和错误处理。
 * @param path API 相对路径。
 * @param method HTTP 方法。
 * @param body 可选 JSON 请求体或原始更新包。
 * @param timeoutMs 请求超时时间，上传包使用独立上限。
 * @returns 类型化响应；无内容响应返回 undefined。
 */
async function request<Result>(path: string, method = 'GET', body?: unknown, timeoutMs = 15000): Promise<Result> {
  const controller = new AbortController()
  const timeout = window.setTimeout(() => controller.abort(), timeoutMs)
  const headers: Record<string, string> = { Accept: 'application/json' }
  const binary = body instanceof Blob
  if (body !== undefined) headers['Content-Type'] = binary ? 'application/octet-stream' : 'application/json'
  if (method !== 'GET' && path !== '/login') headers['X-CSRF-Token'] = csrfToken
  const secrets = body && typeof body === 'object' && 'password' in body
    ? [String(body.password ?? '')] : []
  try {
    const response = await fetch(`/api${path}`, {
      method, headers, credentials: 'same-origin', signal: controller.signal,
      body: binary ? body as Blob : body === undefined ? undefined : JSON.stringify(body),
    })
    if (response.status === 401 && path !== '/login') {
      csrfToken = ''
      window.dispatchEvent(new Event('session-expired'))
      throw new Error('登录已过期，请重新登录。')
    }
    const text = await response.text()
    let payload: unknown
    try { payload = text ? JSON.parse(text) : undefined } catch {
      throw new Error(`服务返回了无效响应（HTTP ${response.status}），请检查后端服务。`)
    }
    if (!response.ok) {
      // 关键写入失败只记录方法和状态码，不记录请求体、地址或凭据。
      if (method !== 'GET') console.warn('[api] 写入失败', { method, status: response.status })
      const detail = payload && typeof payload === 'object' && 'error' in payload
        ? String(payload.error) : `请求失败（HTTP ${response.status}）`
      throw new Error(safeMessage(detail, secrets))
    }
    if (method !== 'GET') console.info('[api] 写入完成', { method, status: response.status })
    return payload as Result
  } catch (error) {
    const portsMutation = method === 'POST' && path === '/ports/apply'
    if (portsMutation && (error instanceof TypeError || (error instanceof DOMException && error.name === 'AbortError'))) {
      throw new Error('端口应用请求连接中断或超时，执行结果尚未确认。请等待服务恢复后刷新状态，不要重复提交；可能需要重新登录。')
    }
    const onlineMutation = method === 'POST' && path.startsWith('/online-updates/')
    if (onlineMutation && (error instanceof TypeError || (error instanceof DOMException && error.name === 'AbortError'))) {
      throw new Error('更新请求连接中断或超时，执行结果尚未确认。请等待服务恢复后刷新状态，不要重复提交；可能需要重新登录。')
    }
    const destructiveRedisRequest = method === 'POST' && /^\/redis\/databases\/db\d+\/clear$/.test(path)
    if (error instanceof DOMException && error.name === 'AbortError') {
      throw new Error(destructiveRedisRequest
        ? '清空请求超时，执行结果不确定。请先刷新核实数据，不要直接重复清空。'
        : '请求超时，请检查服务状态后重试。')
    }
    if (error instanceof TypeError) throw new Error(destructiveRedisRequest
      ? '清空请求连接中断，执行结果不确定。请先刷新核实数据，不要直接重复清空。'
      : '无法连接服务，请检查网络或后端服务。')
    throw error
  } finally { window.clearTimeout(timeout) }
}

/** 获取可展示错误。
 * @param error 捕获到的异常。
 * @returns 脱敏后的诊断。
 */
export function errorMessage(error: unknown): string {
  return safeMessage(error instanceof Error ? error.message : '操作失败，请重试。')
}

export const api = {
  /** 查询宿主期望、发布与 LAN 例外状态。@returns 端口状态快照。 */
  ports: () => request<PortsStatus>('/ports'),
  /** 明确确认后请求宿主应用其计算的端口集合。@param input 版本与确认信息。@returns 后台接受结果。 */
  applyPorts: (input: PortsApplyInput) => request<unknown>('/ports/apply', 'POST', {
    revision: input.revision, configRevision: input.configRevision,
    confirmation: input.confirmation, finalizePrevious: input.finalizePrevious,
  }),
  /** 读取宿主保存的更新来源，不以网页默认值覆盖。@returns 当前来源。 */
  onlineUpdateSource: () => request<OnlineUpdateSource>('/online-updates/source'),
  /** 保存公开来源标识，凭据继续由宿主配置。@param source 已校验来源。@returns 持久化来源。 */
  saveOnlineUpdateSource: (source: OnlineUpdateSource) => request<OnlineUpdateSource>('/online-updates/source', 'POST', source),
  /** 查询宿主更新器持久状态。@returns 在线更新状态。 */
  onlineUpdates: () => request<OnlineUpdateStatus>('/online-updates'),
  /** 仅检测最新正式版本，不下载或切换。@returns 更新器当前状态。 */
  checkOnlineUpdate: () => request<OnlineUpdateStatus>('/online-updates/check', 'POST'),
  /** 显式确认后下载并更新固定版本。@param version 已检测版本。@param digest 镜像摘要。@param confirmation 确认文字。@returns 异步操作状态。 */
  applyOnlineUpdate: (version: string, digest: string, confirmation: string) => request<OnlineUpdateStatus>(
    '/online-updates/apply', 'POST', { version, digest, confirmation }),
  /** 显式确认后回退转发程序。@param confirmation 确认文字。@returns 异步操作状态。 */
  rollbackOnlineUpdate: (confirmation: string) => request<OnlineUpdateStatus>('/online-updates/rollback', 'POST', { confirmation }),
  /** 游标浏览键，不执行KEYS。@param database 库名。@param cursor 扫描游标。@param pattern 匹配模式。@returns 本批键。 */
  redisKeys: (database: string, cursor: string, pattern: string) => request<RedisKeys>(
    `/redis/databases/${encodeURIComponent(database)}/keys?${new URLSearchParams({ cursor, pattern, count: '50' })}`),
  /** 按需读取有界值预览。@param database 库名。@param keyToken 不透明键令牌。@returns 值预览。 */
  redisValue: (database: string, keyToken: string) => request<RedisValue>(
    `/redis/databases/${encodeURIComponent(database)}/value?${new URLSearchParams({ keyToken })}`),
  /** 仅清空选中库。@param database 库名。@param confirmation 确认文字。@param address 确认地址。@returns 接受结果。 */
  clearRedis: (database: string, confirmation: string, address: string) => request<{ database: string; accepted: boolean }>(
    `/redis/databases/${encodeURIComponent(database)}/clear`, 'POST', { confirmation, address }),
  /** 查询固定 Redis 的只读状态，不接受任意地址或命令。@returns 状态快照。 */
  redisStatus: () => request<RedisStatus>('/redis/status'),
  /** 查询更新能力及已校验包。@returns 服务端更新状态。 */
  updates: () => request<UpdateStatus>('/updates'),
  /** 上传签名包，不执行部署。@param file 用户选定的包。@returns 校验后的摘要。 */
  uploadUpdate: (file: File) => request<UpdatePackage>('/updates/package', 'POST', file, 300000),
  /** 删除暂存包，不触碰运行程序或Redis。@returns 完成结果。 */
  deleteUpdate: () => request<void>('/updates/package', 'DELETE'),
  /** 查询会话。@returns 当前认证状态和 CSRF token。 */
  session: () => request<Session>('/session'),
  /** 登录。@param password 管理密码。@returns CSRF token。 */
  login: (password: string) => request<{ csrfToken: string }>('/login', 'POST', { password }),
  /** 注销当前会话。@returns 完成结果。 */
  logout: () => request<void>('/logout', 'POST'),
  /** 查询代理。@returns 不含密码的代理列表。 */
  proxies: () => request<ProxyRecord[]>('/proxies'),
  /** 查询映射。@returns 配置和运行态快照。 */
  mappings: () => request<MappingRecord[]>('/mappings'),
  /** 查询两小时探测历史。@returns 后端时钟与各映射分钟采样。 */
  connectivity: () => request<ConnectivityHistory>('/connectivity'),
  /** 查询运行概览。@returns 服务状态。 */
  status: () => request<ServiceStatus>('/status'),
  /** 保存代理。@param input 可写配置。@param id 编辑目标，缺省为新增。@returns 代理摘要。 */
  saveProxy: (input: ProxyInput, id?: string) => request<ProxyRecord>(
    id ? `/proxies/${encodeURIComponent(id)}` : '/proxies', id ? 'PUT' : 'POST', input),
  /** 删除代理。@param id 代理标识。@returns 完成结果。 */
  deleteProxy: (id: string) => request<void>(`/proxies/${encodeURIComponent(id)}`, 'DELETE'),
  /** 保存映射。@param input 可写配置。@param id 编辑目标，缺省为新增。@returns 映射快照。 */
  saveMapping: (input: MappingInput, id?: string) => request<MappingRecord>(
    id ? `/mappings/${encodeURIComponent(id)}` : '/mappings', id ? 'PUT' : 'POST', input),
  /** 删除映射。@param id 映射标识。@returns 完成结果。 */
  deleteMapping: (id: string) => request<void>(`/mappings/${encodeURIComponent(id)}`, 'DELETE'),
  /** 手动测试映射。@param id 映射标识。@returns TCP 连通性结果。 */
  testMapping: (id: string) => request<TestResult>(`/mappings/${encodeURIComponent(id)}/test`, 'POST'),
}
