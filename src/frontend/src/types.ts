/** SOCKS5 代理的只读摘要；服务端不会返回密码。 */
export interface ProxyRecord {
  id: string
  name: string
  host: string
  port: number
  username: string
  passwordSet: boolean
}

/** 代理提交字段；不提交 password 表示保留已存密码。 */
export interface ProxyInput {
  name: string
  host: string
  port: number
  username: string
  password?: string
  clearPassword?: boolean
}

/** 映射配置；与运行状态分离以避免轮询覆盖编辑草稿。 */
export interface MappingInput {
  name: string
  listenHost: string
  listenPort: number
  targetHost: string
  targetPort: number
  proxyId: string
  enabled: boolean
  maxConnections: number
  allowedCIDRs: string[]
}

export interface MappingRecord extends MappingInput {
  id: string
  status: 'running' | 'stopped' | 'error'
  activeConnections: number
  lastError: string
}

export interface ServiceStatus {
  uptimeSeconds: number
  activeConnections: number
  runningMappings: number
  version: string
}

export interface TestResult {
  ok: boolean
  latencyMs: number
  error?: string
}

export interface ConnectivitySample {
  checkedAt: number
  ok: boolean
  latencyMs: number
  error?: string
  /** 代理 TCP 建连及 SOCKS5 协商/认证阶段，不包含目标连接。 */
  proxy?: TestResult
}

/** 后端时钟和两小时探测历史；缺测分钟不补造成功记录。 */
export interface ConnectivityHistory {
  serverTime: number
  intervalSeconds: number
  windowSeconds: number
  histories: Record<string, ConnectivitySample[]>
}

/** 已验证的更新包摘要，不包含签名私钥或部署权限。 */
export interface UpdatePackage {
  version: string
  platform: string
  imageTag: string
  sha256: string
  size: number
  uploadedAt: string
}

export interface UpdateStatus {
  currentVersion: string
  uploadEnabled: boolean
  applyEnabled: boolean
  maxUploadBytes: number
  reason: string
  package: UpdatePackage | null
}

export interface RedisMetrics {
  version: string | null
  uptimeSeconds: number | null
  usedMemoryBytes: number | null
  maxMemoryBytes: number | null
  connectedClients: number | null
  opsPerSecond: number | null
  totalKeys: number | null
  rdbLastSaveTime: number | null
  rdbLastBgsaveStatus: string | null
  rdbBgsaveInProgress: boolean | null
  rdbChangesSinceLastSave: number | null
  aofEnabled: boolean | null
}

/** 只读 Redis 状态，未知数值为 null，不等同于零。 */
export interface RedisStatus {
  configured: boolean
  address: string
  state: 'unconfigured' | 'online' | 'unreachable' | 'error'
  checkedAt: string | null
  latencyMs: number | null
  error: string
  metrics: RedisMetrics | null
  databases: { name: string; keys: number; expires: number }[]
}

export interface RedisKey { key: string; keyToken: string }
export interface RedisKeys { database: string; keys: RedisKey[]; nextCursor: string }
export interface RedisValue extends RedisKey {
  database: string
  type: string
  ttlSeconds: number
  value: string
  truncated: boolean
  encoding: 'utf8' | 'base64' | 'json'
}

export interface Session {
  authenticated: boolean
  csrfToken?: string
}
