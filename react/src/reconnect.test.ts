import { afterEach, expect, it, vi } from 'vitest'
import { createReconnect } from './reconnect'

afterEach(() => {
  vi.useRealTimers()
  vi.restoreAllMocks()
})

it('所有恢复路径共用有界退避，稳定后恢复初始速度，卸载不再重连', () => {
  vi.useFakeTimers()
  vi.spyOn(Math, 'random').mockReturnValue(1)
  const connect = vi.fn(),
    notices = vi.fn()
  const reconnect = createReconnect(connect, notices)
  for (const delay of [500, 1000, 2000, 4000, 8000, 16000, 30000, 30000]) {
    reconnect.retry()
    reconnect.retry()
    expect(notices).toHaveBeenLastCalledWith(delay)
    const before = connect.mock.calls.length
    vi.advanceTimersByTime(delay - 1)
    expect(connect).toHaveBeenCalledTimes(before)
    vi.advanceTimersByTime(1)
    expect(connect).toHaveBeenCalledTimes(before + 1)
    reconnect.connected()
    vi.advanceTimersByTime(100)
  }
  vi.advanceTimersByTime(30000)
  reconnect.retry()
  expect(notices).toHaveBeenLastCalledWith(500)
  reconnect.close()
  const before = connect.mock.calls.length
  reconnect.retry()
  vi.advanceTimersByTime(60000)
  expect(connect).toHaveBeenCalledTimes(before)
})
