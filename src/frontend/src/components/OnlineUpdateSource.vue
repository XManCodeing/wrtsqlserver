<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { defaultOnlineSource } from '../composables/onlineSourceState'
import { useOnlineSource } from '../composables/useOnlineSource'

const props = defineProps<{ active: boolean; blocked: boolean; onSaved: () => Promise<void> }>()
const emit = defineEmits<{ saving: [value: boolean] }>()
const { source, draft, loading, saving, visible, error, formError, disabled, load, open, save } = useOnlineSource(
  () => props.active, () => props.blocked, props.onSaved,
)
const expanded = ref<string[]>([])
// 默认值仅作展示；未成功读取宿主配置前，仍禁止编辑和保存。
const displayedSource = computed(() => source.value ?? defaultOnlineSource)
watch(saving, value => emit('saving', value), { flush: 'sync' })
</script>

<template>
  <el-collapse v-model="expanded">
    <el-collapse-item title="更新来源设置" name="source">
  <section v-loading="loading" class="source-settings">
    <el-button link type="primary" :disabled="disabled" @click="open">编辑来源</el-button>
    <el-alert v-if="error" :title="error" type="warning" :closable="false" show-icon />
    <p v-if="!source" class="field-help">以下为默认来源，尚未确认实际配置；读取成功后可编辑。</p>
    <el-descriptions :column="1" size="small">
      <el-descriptions-item label="GitHub 仓库"><span class="endpoint">{{ displayedSource.repositoryUrl }}</span></el-descriptions-item>
      <el-descriptions-item label="GHCR 镜像"><span class="endpoint">{{ displayedSource.image }}</span></el-descriptions-item>
    </el-descriptions>
    <p class="field-help">仅支持 GitHub 与 GHCR；修改后需重新检测。仓库访问凭据由宿主配置，发布包须兼容本程序。<el-button v-if="error" link type="primary" :disabled="loading || saving" @click="load">重新读取</el-button></p>
  </section>
    </el-collapse-item>
  </el-collapse>
  <el-dialog v-model="visible" title="编辑更新来源" width="560px" :close-on-click-modal="false" :close-on-press-escape="!saving" :show-close="!saving">
    <el-alert v-if="formError" :title="formError" type="error" :closable="false" show-icon />
    <el-form label-position="top" :disabled="saving" @submit.prevent="save">
      <el-form-item label="GitHub 仓库地址" required><el-input v-model="draft.repositoryUrl" :maxlength="256" :placeholder="defaultOnlineSource.repositoryUrl" autocomplete="off" /></el-form-item>
      <el-form-item label="GHCR 镜像路径" required><el-input v-model="draft.image" :maxlength="256" :placeholder="defaultOnlineSource.image" autocomplete="off" /></el-form-item>
      <p class="field-help">不填写 token 或密码。保存只修改来源，不检测、下载或更新；私有仓库仍需宿主具有读取权限。</p>
    </el-form>
    <template #footer><el-button :disabled="saving" @click="visible = false">取消</el-button><el-button type="primary" :loading="saving" :disabled="disabled" @click="save">保存来源</el-button></template>
  </el-dialog>
</template>

<style scoped>
.source-settings .endpoint { white-space: normal; overflow-wrap: anywhere; }
</style>
