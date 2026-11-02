<script setup lang="ts">
import { ref } from 'vue'
import { api, errorMessage, setCsrfToken } from '../api'

const emit = defineEmits<{ authenticated: [] }>()
const password = ref('')
const loading = ref(false)
const error = ref('')

/** 验证密码并开启会话；密码仅保留于当前表单。
 * @returns 登录完成后的 Promise。
 */
async function login(): Promise<void> {
  if (loading.value || !password.value) return
  loading.value = true
  error.value = ''
  try {
    const result = await api.login(password.value)
    setCsrfToken(result.csrfToken)
    password.value = ''
    emit('authenticated')
  } catch (failure) { error.value = errorMessage(failure) }
  finally { loading.value = false }
}
</script>

<template>
  <main class="login-shell">
    <section class="login-panel panel" aria-labelledby="login-title">
      <div class="eyebrow">PORT FORWARDING</div>
      <h1 id="login-title">端口转发控制台</h1>
      <p class="muted">管理 SOCKS5 代理与 TCP 端口映射。</p>
      <el-alert v-if="error" :title="error" type="error" :closable="false" show-icon />
      <el-form label-position="top" @submit.prevent="login">
        <el-form-item label="管理密码">
          <el-input v-model="password" type="password" autocomplete="current-password" placeholder="输入管理密码" :disabled="loading" />
        </el-form-item>
        <el-button type="primary" native-type="submit" :loading="loading" :disabled="!password" class="login-submit">登录</el-button>
      </el-form>
    </section>
  </main>
</template>
