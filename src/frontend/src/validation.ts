import type { MappingInput, MappingRecord, ProxyInput } from './types'

/** 校验端口。
 * @param value 待检验数值。
 * @returns 是否为有效 TCP 端口。
 */
function isPort(value: number): boolean {
  return Number.isInteger(value) && value >= 1 && value <= 65535
}

/** 校验主机名/IP，不允许协议、凭据或路径。
 * @param value 待检验主机字符串。
 * @returns 是否有效。
 */
export function isHost(value: string): boolean {
  if (!value || value.length > 253 || /[\s/@?#\\]/.test(value)) return false
  if (value.includes(':')) {
    try { return new URL(`http://[${value}]/`).hostname.length > 0 } catch { return false }
  }
  if (/^[\d.]+$/.test(value)) return isIPv4(value)
  return value.split('.').every(label => /^[a-z\d](?:[a-z\d-]{0,61}[a-z\d])?$/i.test(label))
}

/** 严格校验 IPv4 地址。
 * @param value 点分十进制地址。
 * @returns 是否有效。
 */
function isIPv4(value: string): boolean {
  const parts = value.split('.')
  return parts.length === 4 && parts.every(part => /^(0|[1-9]\d{0,2})$/.test(part) && Number(part) <= 255)
}

/** 计算 UTF-8 字节数，与后端字符串长度约束一致。
 * @param value 用户输入文本。
 * @returns UTF-8 编码字节数。
 */
export function byteLength(value: string): number { return new TextEncoder().encode(value).length }

/** 校验名称内容和字节上限。
 * @param value 显示名称。
 * @returns 是否满足后端合同。
 */
function isName(value: string): boolean {
  return !!value.trim() && byteLength(value) <= 100 && !/[\x00\r\n\t]/.test(value)
}

/** 校验允许绑定的 IPv4 范围。
 * @param value 监听地址。
 * @returns 是否为私网、回环或所有接口地址。
 */
export function isListenHost(value: string): boolean {
  if (!isIPv4(value)) return false
  const [first, second] = value.split('.').map(Number)
  return value === '0.0.0.0' || first === 127 || first === 10 ||
    (first === 172 && second >= 16 && second <= 31) || (first === 192 && second === 168)
}

/** 检验 IPv4 CIDR，禁止开放所有来源。
 * @param value CIDR 文本。
 * @returns 是否有效。
 */
export function isCIDR(value: string): boolean {
  const parts = value.split('/')
  if (parts.length !== 2 || !/^\d{1,2}$/.test(parts[1])) return false
  const prefix = Number(parts[1])
  return isIPv4(parts[0]) && prefix >= 1 && prefix <= 32
}

/** 校验代理提交边界。
 * @param input 代理草稿。
 * @returns 首条错误，成功为空。
 */
export function validateProxy(input: ProxyInput): string {
  if (!isName(input.name)) return '代理名称不能为空、不能含控制字符，且不得超过 100 UTF-8 字节。'
  if (!isHost(input.host)) return '请输入有效代理主机名或 IP，不包含协议和端口。'
  if (!isPort(input.port)) return '代理端口必须为 1–65535 的整数。'
  if (byteLength(input.username) > 255 || byteLength(input.password ?? '') > 255) return '用户名和密码分别不能超过 255 UTF-8 字节。'
  return ''
}

/** 校验映射提交边界。
 * @param input 映射草稿。
 * @returns 首条错误，成功为空。
 */
export function validateMapping(input: MappingInput): string {
  if (!isName(input.name)) return '映射名称不能为空、不能含控制字符，且不得超过 100 UTF-8 字节。'
  if (!isListenHost(input.listenHost)) return '监听地址仅支持私网 IPv4、回环 IPv4 或 0.0.0.0。'
  if (!isHost(input.targetHost)) return '请输入有效目标主机名或 IP，不包含协议和端口。'
  if (!isPort(input.listenPort) || !isPort(input.targetPort)) return '端口必须为 1–65535 的整数。'
  if (!input.proxyId) return '请选择 SOCKS5 代理。'
  if (!Number.isInteger(input.maxConnections) || input.maxConnections < 1 || input.maxConnections > 4096) return '连接上限必须为 1–4096 的整数。'
  if (!input.allowedCIDRs.length || input.allowedCIDRs.length > 64 || input.allowedCIDRs.some(cidr => !isCIDR(cidr))) return '请填写 1–64 条 IPv4 CIDR，每行一条；不支持 IPv6 或 /0。'
  return ''
}

/** 提取可写配置，防止将运行态字段传回服务端。
 * @param record 当前映射快照。
 * @returns 独立的配置副本。
 */
export function mappingInput(record: MappingRecord): MappingInput {
  const { name, listenHost, listenPort, targetHost, targetPort, proxyId, enabled, maxConnections, allowedCIDRs } = record
  return { name, listenHost, listenPort, targetHost, targetPort, proxyId, enabled, maxConnections, allowedCIDRs: [...allowedCIDRs] }
}

/** 为副本保留完整码点的名称前缀，追加固定后缀且不超过后端字节上限。
 * @param name 原映射名称。
 * @returns 最多 100 UTF-8 字节的副本名称。
 */
export function mappingCopyName(name: string): string {
  const suffix = ' copy'
  const budget = 100 - byteLength(suffix)
  let prefix = ''
  let used = 0
  for (const character of name) {
    const size = byteLength(character)
    if (used + size > budget) break
    prefix += character
    used += size
  }
  return prefix + suffix
}

/** 格式化主机端口，兼容 IPv6。
 * @param host 主机地址。
 * @param port 端口。
 * @returns 可读端点。
 */
export function endpoint(host: string, port: number): string {
  return `${host.includes(':') ? `[${host}]` : host}:${port}`
}
