/** 宿主统一的连接恢复调度；只重新建连，不重放应用输入。 */
export function createReconnect(connect: () => void, notify?: (delayMS: number) => void) {
  let attempts = 0
  let timer: ReturnType<typeof setTimeout> | undefined
  let stable: ReturnType<typeof setTimeout> | undefined
  let closed = false
  const cancel = () => {
    clearTimeout(timer)
    clearTimeout(stable)
    timer = undefined
    stable = undefined
  }
  return {
    /** 一轮失败只安排一次恢复，抖动连接不会重置指数退避。 */
    retry() {
      if (closed || timer !== undefined) return
      clearTimeout(stable)
      stable = undefined
      const delay = Math.round(Math.min(30000, 500 * 2 ** Math.min(attempts++, 6)) * (0.8 + Math.random() * 0.2))
      notify?.(delay)
      timer = setTimeout(() => {
        timer = undefined
        if (!closed) connect()
      }, delay)
    },
    /** 业务连接稳定三十秒后恢复初始退避，不能因瞬间握手成功清零。 */
    connected() {
      if (closed || stable !== undefined) return
      clearTimeout(timer)
      timer = undefined
      stable = setTimeout(() => {
        attempts = 0
        stable = undefined
      }, 30000)
    },
    /** 上层连接变化时取消当前安排，保留失败次数。 */
    cancel,
    pending: () => timer !== undefined,
    /** 释放所有计时器；关闭后不再建连。 */
    close() {
      closed = true
      cancel()
    },
  }
}
