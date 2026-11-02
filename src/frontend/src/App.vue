<script setup lang="ts">
import { nextTick, onMounted, onUnmounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Connection, DataLine, Monitor, Upload, Coin } from '@element-plus/icons-vue'
import { api, errorMessage, setCsrfToken } from './api'
import { useTheme } from './composables/useTheme'
import { useDashboard } from './composables/useDashboard'
import LoginPanel from './components/LoginPanel.vue'
import MappingsPage from './components/MappingsPage.vue'
import ProxiesPage from './components/ProxiesPage.vue'
import OverviewPage from './components/OverviewPage.vue'
import UpdatesPage from './components/UpdatesPage.vue'
import RedisPage from './components/RedisPage.vue'

const { mode, warning } = useTheme()
const dashboard = useDashboard()
const { proxies, mappings, status, loading, error, updatedAt } = dashboard
const authenticated = ref(false)
const checking = ref(true)
const sessionError = ref('')
const loggingOut = ref(false)
const page = ref<'overview' | 'mappings' | 'proxies' | 'updates' | 'redis'>('overview')
const proxyRevision = ref(0)
const proxiesPage = ref<InstanceType<typeof ProxiesPage>>()

/** 代理写入后失效近期探测（包括密码变化）。@returns 无。 */
function proxiesChanged(): void {
  proxyRevision.value += 1
  void dashboard.refresh()
}

/** 登录成功后建立实时只读快照。
 * @returns 无返回值。
 */
function onAuthenticated(): void {
  authenticated.value = true
  sessionError.value = ''
  dashboard.start()
}

/** 恢复现有 cookie 会话，失败保留显式重试入口。
 * @returns 检查完成后的 Promise。
 */
async function checkSession(): Promise<void> {
  checking.value = true
  sessionError.value = ''
  try {
    const session = await api.session()
    if (session.authenticated && session.csrfToken) {
      setCsrfToken(session.csrfToken)
      onAuthenticated()
    }
  } catch (failure) { sessionError.value = errorMessage(failure) }
  finally { checking.value = false }
}

/** 会话过期时停止请求并清除敏感编辑状态。
 * @returns 无返回值。
 */
function expireSession(): void {
  if (!authenticated.value) return
  authenticated.value = false
  setCsrfToken('')
  dashboard.stop()
  ElMessageBox.close()
  ElMessage.warning('登录已过期，请重新登录。')
}

/** 确认后注销，避免意外丢弃未保存草稿。
 * @returns 注销完成后的 Promise。
 */
async function logout(): Promise<void> {
  if (loggingOut.value) return
  loggingOut.value = true
  try {
    try { await ElMessageBox.confirm('退出将清除未保存的编辑内容。', '退出登录', { confirmButtonText: '退出登录', cancelButtonText: '取消' }) }
    catch { return }
    await api.logout()
    authenticated.value = false
    setCsrfToken('')
    dashboard.stop()
  } catch (failure) { ElMessage.error(errorMessage(failure)) }
  finally { loggingOut.value = false }
}

/** 打开代理创建引导，切换页面不销毁其他页面草稿。
 * @returns 弹窗打开后的 Promise。
 */
async function createProxy(): Promise<void> {
  page.value = 'proxies'
  await nextTick()
  proxiesPage.value?.create()
}

onMounted(() => { window.addEventListener('session-expired', expireSession); void checkSession() })
onUnmounted(() => window.removeEventListener('session-expired', expireSession))
</script>

<template>
  <div class="console-root">
    <header class="topbar">
      <div class="brand"><el-icon :size="21"><Connection /></el-icon><span>端口转发<span class="brand-secondary">控制台</span></span></div>
      <div class="topbar-actions">
        <span v-if="authenticated" class="refresh-label">{{ updatedAt ? `更新于 ${updatedAt}` : '等待服务状态' }}</span>
        <el-select v-model="mode" class="theme-select" aria-label="选择外观主题">
          <el-option label="浅色外观" value="light" /><el-option label="深色外观" value="dark" /><el-option label="跟随系统" value="system" />
        </el-select>
        <el-button v-if="authenticated" text :loading="loggingOut" @click="logout">退出登录</el-button>
      </div>
    </header>
    <el-alert v-if="warning" :title="warning" type="warning" :closable="false" />
    <main v-if="checking" class="session-loading" v-loading="true" aria-label="正在验证会话" />
    <main v-else-if="sessionError" class="login-shell"><section class="panel login-panel"><h1>暂时无法连接服务</h1><el-alert :title="sessionError" type="error" :closable="false" /><el-button type="primary" @click="checkSession">重试连接</el-button></section></main>
    <LoginPanel v-else-if="!authenticated" @authenticated="onAuthenticated" />
    <div v-else class="workspace">
      <aside class="sidebar" aria-label="主导航">
        <div class="nav-caption">工作空间</div>
        <el-menu :default-active="page" @select="page = $event">
          <el-menu-item index="overview"><el-icon><DataLine /></el-icon><span>运行概览</span></el-menu-item>
          <el-menu-item index="mappings"><el-icon><Connection /></el-icon><span>端口映射</span></el-menu-item>
          <el-menu-item index="proxies"><el-icon><Monitor /></el-icon><span>SOCKS5 代理</span></el-menu-item>
          <el-menu-item index="redis"><el-icon><Coin /></el-icon><span>Redis 状态</span></el-menu-item>
          <el-menu-item index="updates"><el-icon><Upload /></el-icon><span>程序更新</span></el-menu-item>
        </el-menu>
        <div class="sidebar-note">TCP 转发管理<br />配置按映射独立生效</div>
      </aside>
      <main class="main-content">
        <el-alert v-if="error" type="error" :closable="false" show-icon class="global-error">
          <template #title>{{ error }}</template><span>当前展示最近一次成功快照。</span><el-button link type="primary" :disabled="loading" @click="dashboard.refresh(true)">重新加载</el-button>
        </el-alert>
        <OverviewPage v-show="page === 'overview'" :status="status" :mappings="mappings" :proxy-count="proxies.length" :loading="loading" @navigate="page = $event" />
        <MappingsPage v-show="page === 'mappings'" :active="page === 'mappings'" :proxy-revision="proxyRevision" :mappings="mappings" :proxies="proxies" :loading="loading" @changed="dashboard.refresh()" @create-proxy="createProxy" />
        <ProxiesPage v-show="page === 'proxies'" ref="proxiesPage" :mappings="mappings" :proxies="proxies" :loading="loading" @changed="proxiesChanged" />
        <RedisPage v-show="page === 'redis'" :active="page === 'redis'" />
        <UpdatesPage v-show="page === 'updates'" :active="page === 'updates'" />
      </main>
    </div>
  </div>
</template>
