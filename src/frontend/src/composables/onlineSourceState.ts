export interface OnlineUpdateSource { repositoryUrl: string; image: string }

export const defaultOnlineSource: OnlineUpdateSource = {
  repositoryUrl: 'https://github.com/XManCodeing/wrtsqlserver',
  image: 'ghcr.io/xmancodeing/wrtsqlserver',
}

/** 校验公开来源标识，不接受凭据、查询参数或镜像标签。@param source 来源草稿。@returns 错误文案或空字符串。 */
export function validateOnlineSource(source: OnlineUpdateSource): string {
  if (source.repositoryUrl.length > 256 || !/^https:\/\/github\.com\/[A-Za-z0-9][A-Za-z0-9-]*\/[A-Za-z0-9_.-]+$/.test(source.repositoryUrl)
    || /\/\.{1,2}$/.test(source.repositoryUrl)) return '请输入 https://github.com/所有者/仓库，最多 256 字符。'
  if (source.image.length > 256 || !/^ghcr\.io\/[a-z0-9]+(?:[._-][a-z0-9]+)*(?:\/[a-z0-9]+(?:[._-][a-z0-9]+)*)+$/.test(source.image)) return '请输入 GHCR 镜像路径（小写、不含标签或摘要），最多 256 字符。'
  return ''
}

/** 验证服务端来源响应，不将未知或空响应当作保存成功。@param value 接口返回值。@returns 有效来源。 */
export function checkedOnlineSource(value: unknown): OnlineUpdateSource {
  if (!value || typeof value !== 'object' || !('repositoryUrl' in value) || !('image' in value)
    || typeof value.repositoryUrl !== 'string' || typeof value.image !== 'string') throw new Error('更新来源响应无效，请重新读取。')
  const result = { repositoryUrl: value.repositoryUrl, image: value.image }
  if (validateOnlineSource(result)) throw new Error('更新来源响应无效，请重新读取。')
  return result
}
