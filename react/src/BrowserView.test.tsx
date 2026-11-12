import { act, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  activateBrowserTab,
  historyBrowserTab,
  navigateBrowserTab,
  browserDevtoolsStreamURL,
  browserStateStreamURL,
  browserStreamURL,
  closeBrowser,
  closeBrowserTab,
  fetchBrowserAssets,
  fetchBrowserDevtoolsState,
  fetchBrowserState,
  fetchCachedBrowserFrame,
  setBrowserTabViewport,
  takeBrowserControl,
  startBrowserSequenceRecording,
  finishBrowserSequenceRecording,
  type BrowserTabView,
  type BrowserViewState,
} from './client'
import { BrowserView } from './BrowserView'
import { httpBrowserSource } from './source'
function BrowserLiveWindow({
  sessionID,
  devtoolsEnabled,
  liveViewEnabled,
  agentOperating,
}: {
  sessionID?: string
  devtoolsEnabled?: boolean
  liveViewEnabled?: boolean
  agentOperating?: boolean
  [key: string]: unknown
}) {
  return (
    <BrowserView
      endpoint={sessionID || '/browser'}
      devtools={devtoolsEnabled}
      liveView={liveViewEnabled}
      busy={agentOperating}
    />
  )
}

vi.mock('./client', () => ({
  decodeFrame: vi.fn(async (blob: Blob) => ({ blob, pageID: 'page-a', generation: 1 })),
  newBrowserTab: vi.fn(),
  navigateBrowserTab: vi.fn(),
  historyBrowserTab: vi.fn(),
  reloadBrowserTab: vi.fn(),
  activateBrowserTab: vi.fn(),
  browserDevtoolsStreamURL: vi.fn(() => 'ws://localhost/browser/devtools/ws'),
  browserStateStreamURL: vi.fn(() => 'ws://localhost/browser/state/ws'),
  browserStreamURL: vi.fn(() => 'ws://localhost/browser/ws'),
  closeBrowser: vi.fn(),
  closeBrowserTab: vi.fn(),
  fetchBrowserAssets: vi.fn(),
  fetchBrowserDevtoolsState: vi.fn(),
  startBrowserDevtools: vi.fn(),
  stopBrowserDevtools: vi.fn(),
  fetchBrowserState: vi.fn(),
  fetchCachedBrowserFrame: vi.fn(),
  setBrowserTabViewport: vi.fn(),
  takeBrowserControl: vi.fn(),
  startBrowserSequenceRecording: vi.fn(),
  finishBrowserSequenceRecording: vi.fn(),
}))

const mockedActivateBrowserTab = vi.mocked(activateBrowserTab)
const mockedHistoryBrowserTab = vi.mocked(historyBrowserTab)
const mockedFetchBrowserAssets = vi.mocked(fetchBrowserAssets)
const mockedFetchBrowserDevtoolsState = vi.mocked(fetchBrowserDevtoolsState)
const mockedFetchBrowserState = vi.mocked(fetchBrowserState)
const mockedFetchCachedBrowserFrame = vi.mocked(fetchCachedBrowserFrame)
const mockedBrowserStreamURL = vi.mocked(browserStreamURL)
const mockedBrowserDevtoolsStreamURL = vi.mocked(browserDevtoolsStreamURL)
const mockedBrowserStateStreamURL = vi.mocked(browserStateStreamURL)
const mockedCloseBrowser = vi.mocked(closeBrowser)
const mockedCloseBrowserTab = vi.mocked(closeBrowserTab)
const mockedSetBrowserTabViewport = vi.mocked(setBrowserTabViewport)
const mockedTakeBrowserControl = vi.mocked(takeBrowserControl)
const mockedStartRecording = vi.mocked(startBrowserSequenceRecording)
const mockedFinishRecording = vi.mocked(finishBrowserSequenceRecording)

const defaultViewport = { width: 1_920, height: 1_080 }

const singleTabState = (
  tab: Partial<BrowserTabView> = {},
  state: Omit<Partial<BrowserViewState>, 'active_page_id' | 'tabs'> = {},
): BrowserViewState => {
  const pageID = tab.page_id ?? 'page-a'
  return {
    available: true,
    closed: false,
    tabs_revision: 0,
    url: tab.url ?? 'https://example.com',
    viewport: tab.viewport ?? defaultViewport,
    ...state,
    active_page_id: pageID,
    tabs: [
      {
        page_id: pageID,
        url: 'https://example.com',
        viewport: defaultViewport,
        status: 'ready',
        active: true,
        ...tab,
      },
    ],
  }
}

const unavailableState = (closed = false): BrowserViewState => ({
  available: false,
  closed,
  tabs_revision: 0,
  tabs: closed ? [] : undefined,
})

describe('BrowserLiveWindow', () => {
  let socketMessages: string[]
  let sockets: Array<{
    bufferedAmount: number
    onmessage: ((event: MessageEvent) => void) | null
    onclose: (() => void) | null
  }>
  let drawImage: ReturnType<typeof vi.fn>
  let createBitmap: ReturnType<typeof vi.fn>

  const receiveFrameWithControl = async (control: {
    controller: boolean
    can_takeover: boolean
    retry_after_ms?: number
    revision: number
  }) => {
    await act(async () => {
      sockets
        .at(-1)
        ?.onmessage?.(new MessageEvent('message', { data: JSON.stringify({ type: 'browser_control', ...control }) }))
      sockets.at(-1)?.onmessage?.(new MessageEvent('message', { data: new Blob(['jpeg']) }))
      await Promise.resolve()
      await Promise.resolve()
    })
  }

  const receiveFrame = () => receiveFrameWithControl({ controller: true, can_takeover: false, revision: 1 })

  const receiveState = async (state: BrowserViewState) => {
    await act(async () => {
      sockets[0]?.onmessage?.(new MessageEvent('message', { data: JSON.stringify(state) }))
      await Promise.resolve()
    })
  }

  const sentInputs = () =>
    socketMessages.map((message) => {
      const { page_id: _, ...input } = JSON.parse(message)
      return input
    })

  beforeEach(() => {
    vi.useFakeTimers()
    mockedFetchBrowserState.mockReset()
    mockedFetchBrowserAssets.mockReset()
    mockedFetchBrowserAssets.mockResolvedValue({ page_id: 'page-a', assets: [], truncated: false })
    mockedFetchBrowserDevtoolsState.mockReset()
    mockedFetchBrowserDevtoolsState.mockResolvedValue({
      available: true,
      page_id: 'page-a',
      status: 'idle',
      generation: 0,
      revision: 0,
      entries: [],
      next_sequence: 0,
      pending_requests: 0,
      dropped_before: 0,
      truncated: false,
    })
    mockedFetchCachedBrowserFrame.mockReset()
    mockedFetchCachedBrowserFrame.mockRejectedValue(new Error('没有缓存帧'))
    mockedActivateBrowserTab.mockReset()
    mockedActivateBrowserTab.mockResolvedValue()
    mockedHistoryBrowserTab.mockReset()
    mockedHistoryBrowserTab.mockResolvedValue()
    mockedBrowserStreamURL.mockReturnValue('ws://localhost/browser/ws')
    mockedBrowserDevtoolsStreamURL.mockReturnValue('ws://localhost/browser/devtools/ws')
    mockedBrowserStateStreamURL.mockReturnValue('ws://localhost/browser/state/ws')
    mockedCloseBrowser.mockReset()
    mockedCloseBrowser.mockResolvedValue()
    mockedCloseBrowserTab.mockReset()
    mockedCloseBrowserTab.mockResolvedValue()
    mockedSetBrowserTabViewport.mockReset()
    mockedSetBrowserTabViewport.mockResolvedValue()
    mockedTakeBrowserControl.mockReset()
    mockedTakeBrowserControl.mockResolvedValue({
      type: 'browser_control',
      controller: true,
      can_takeover: false,
      retry_after_ms: 30_000,
      revision: 2,
    })
    mockedStartRecording.mockReset()
    mockedStartRecording.mockResolvedValue()
    mockedFinishRecording.mockReset()
    mockedFinishRecording.mockResolvedValue({ session_id: 'session-a', path: '/tmp/sequence.json', sequence: {} })
    socketMessages = []
    sockets = []
    drawImage = vi.fn()
    createBitmap = vi.fn(async () => ({ width: 1_920, height: 1_080, close: vi.fn() }))
    class TestWebSocket {
      binaryType = 'blob'
      readyState = 1
      bufferedAmount = 0
      onmessage: ((event: MessageEvent) => void) | null = null
      onclose: (() => void) | null = null
      onerror: (() => void) | null = null
      constructor() {
        sockets.push(this)
      }
      send(message: string) {
        socketMessages.push(message)
      }
      close() {}
    }
    vi.stubGlobal('WebSocket', TestWebSocket)
    vi.stubGlobal('createImageBitmap', createBitmap)
    vi.spyOn(HTMLCanvasElement.prototype, 'getContext').mockReturnValue({
      drawImage,
    } as unknown as CanvasRenderingContext2D)
    const host = document.createElement('div')
    host.className = 'browser-live-panel-host'
    document.body.appendChild(host)
    Object.defineProperty(URL, 'createObjectURL', { configurable: true, value: vi.fn(() => 'blob:browser-frame') })
    Object.defineProperty(URL, 'revokeObjectURL', { configurable: true, value: vi.fn() })
  })

  afterEach(() => {
    document.querySelector('.browser-live-panel-host')?.remove()
    vi.restoreAllMocks()
    vi.unstubAllGlobals()
    vi.useRealTimers()
  })

  const pointerEvent = (type: string, buttons = 1) => {
    const event = new Event(type, { bubbles: true, cancelable: true })
    Object.defineProperties(event, {
      button: { value: 0 },
      buttons: { value: buttons },
      pointerId: { value: 7 },
      pointerType: { value: 'mouse' },
      clientX: { value: 100 },
      clientY: { value: 100 },
    })
    return event
  }

  const configuredFrame = async () => {
    await act(async () => {})
    await receiveFrame()
    const frame = screen.getByRole('application', { name: '可交互浏览器页面' })
    vi.spyOn(frame, 'getBoundingClientRect').mockReturnValue({
      left: 0,
      top: 0,
      width: 1920,
      height: 1080,
      right: 1920,
      bottom: 1080,
      x: 0,
      y: 0,
      toJSON: () => ({}),
    })
    return frame
  }

  it('reconnects the state stream even when the HTTP refresh succeeds', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    render(<BrowserLiveWindow sessionID="/browser" />)
    await act(async () => {})
    const stateSocket = sockets[0]
    const previousSockets = new Set(sockets)
    mockedFetchBrowserState.mockResolvedValue(singleTabState({ url: 'https://example.com/snapshot' }))

    await act(async () => {
      stateSocket.onclose?.()
    })
    expect(screen.getByDisplayValue('https://example.com/snapshot')).toBeInTheDocument()
    await act(async () => {
      await vi.advanceTimersByTimeAsync(500)
    })

    const reconnected = sockets.find((socket) => !previousSockets.has(socket))
    expect(reconnected).toBeDefined()
    await act(async () => {
      reconnected!.onmessage?.(
        new MessageEvent('message', {
          data: JSON.stringify(singleTabState({ page_id: 'page-b', url: 'https://example.com/live' })),
        }),
      )
    })
    expect(screen.getByDisplayValue('https://example.com/live')).toBeInTheDocument()
  })

  it('keeps scrolling in select mode and freezes remote input only after selection', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    const source = {
      ...httpBrowserSource,
      inspectPoint: vi.fn(async () => ({
        ref: 'r_1',
        revision: 1,
        role: 'button',
        name: '提交',
        url: 'https://example.com',
        bounds: { x: 90, y: 90, width: 80, height: 30 },
      })),
    }
    render(<BrowserView endpoint="/browser" source={source} onElementSelect={vi.fn()} />)
    const frame = await configuredFrame()
    fireEvent.click(screen.getByRole('button', { name: '选择页面元素' }))
    const overlay = document.querySelector('.browser-element-inspector')!
    fireEvent.wheel(overlay, { clientX: 100, clientY: 100, deltaY: 20 })
    act(() => {
      vi.advanceTimersByTime(60)
    })
    expect(sentInputs()).toEqual([expect.objectContaining({ kind: 'mouse', action: 'wheel' })])
    fireEvent(overlay, pointerEvent('pointerdown'))
    fireEvent(overlay, pointerEvent('pointerup', 0))
    await act(async () => {
      await Promise.resolve()
    })
    expect(screen.getByRole('dialog', { name: '选中页面内容' })).toBeInTheDocument()
    fireEvent.wheel(overlay, { clientX: 100, clientY: 100, deltaY: 20 })
    fireEvent.keyDown(frame, { key: 'A', code: 'KeyA' })
    expect(sentInputs()).toHaveLength(1)
    fireEvent.click(screen.getByRole('button', { name: '取消' }))
    expect(screen.getByRole('button', { name: '选择页面元素' })).toHaveAttribute('aria-pressed', 'true')
    fireEvent.wheel(overlay, { clientX: 100, clientY: 100, deltaY: 20 })
    act(() => {
      vi.advanceTimersByTime(60)
    })
    expect(sentInputs()).toHaveLength(2)
  })

  it('samples continuous hover at 10Hz with an immediate first check and the latest position', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    const source = {
      ...httpBrowserSource,
      inspectPoint: vi.fn(async () => ({
        ref: 'r_1',
        revision: 1,
        role: 'button',
        url: 'https://example.com',
        bounds: { x: 90, y: 90, width: 80, height: 30 },
      })),
    }
    const { unmount } = render(<BrowserView endpoint="/browser" source={source} onElementSelect={vi.fn()} />)
    await configuredFrame()
    vi.useFakeTimers()
    fireEvent.click(screen.getByRole('button', { name: '选择页面元素' }))
    const overlay = document.querySelector('.browser-element-inspector')!
    const move = (x: number) => {
      const event = new Event('pointermove', { bubbles: true, cancelable: true })
      Object.defineProperties(event, {
        clientX: { value: x },
        clientY: { value: 100 },
        pointerId: { value: 7 },
        pointerType: { value: 'mouse' },
        buttons: { value: 0 },
      })
      fireEvent(overlay, event)
    }
    move(100)
    expect(source.inspectPoint).toHaveBeenCalledExactlyOnceWith('/browser', 'page-a', 100, 100, expect.any(String))
    for (const x of [200, 300, 400, 500]) {
      await act(async () => {
        await vi.advanceTimersByTimeAsync(20)
      })
      move(x)
    }
    expect(source.inspectPoint).toHaveBeenCalledTimes(1)
    await act(async () => {
      await vi.advanceTimersByTimeAsync(19)
    })
    expect(source.inspectPoint).toHaveBeenCalledTimes(1)
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1)
    })
    expect(source.inspectPoint).toHaveBeenLastCalledWith(
      '/browser',
      'page-a',
      expect.closeTo(500, 5),
      100,
      expect.any(String),
    )
    expect(source.inspectPoint).toHaveBeenCalledTimes(2)
    expect(document.querySelector('.browser-element-inspector-outline')).toBeInTheDocument()
    move(600)
    fireEvent(overlay, pointerEvent('pointerdown'))
    await act(async () => {
      await vi.advanceTimersByTimeAsync(100)
    })
    expect(source.inspectPoint).toHaveBeenCalledTimes(2)
    fireEvent.keyDown(overlay, { key: 'Escape' })
    fireEvent.click(screen.getByRole('button', { name: '选择页面元素' }))
    const freshOverlay = document.querySelector('.browser-element-inspector')!
    fireEvent(freshOverlay, pointerEvent('pointermove', 0))
    expect(source.inspectPoint).toHaveBeenCalledTimes(3)
    fireEvent(freshOverlay, pointerEvent('pointermove', 0))
    fireEvent.keyDown(freshOverlay, { key: 'Escape' })
    await act(async () => {
      await vi.advanceTimersByTimeAsync(100)
    })
    expect(source.inspectPoint).toHaveBeenCalledTimes(3)
    fireEvent.click(screen.getByRole('button', { name: '选择页面元素' }))
    fireEvent(document.querySelector('.browser-element-inspector')!, pointerEvent('pointermove', 0))
    fireEvent(document.querySelector('.browser-element-inspector')!, pointerEvent('pointermove', 0))
    unmount()
    await act(async () => {
      await vi.advanceTimersByTimeAsync(100)
    })
    expect(source.inspectPoint).toHaveBeenCalledTimes(4)
  })

  it('shows a slow hover reply during movement and samples only the latest waiting point', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    const element = {
      ref: 'r_1',
      revision: 1,
      role: 'button',
      url: 'https://example.com',
      bounds: { x: 90, y: 90, width: 80, height: 30 },
    }
    let finish!: (value: typeof element) => void
    const source = {
      ...httpBrowserSource,
      inspectPoint: vi.fn(
        () =>
          new Promise<typeof element>((resolve) => {
            finish = resolve
          }),
      ),
    }
    render(<BrowserView endpoint="/browser" source={source} onElementSelect={vi.fn()} />)
    await configuredFrame()
    vi.useFakeTimers()
    fireEvent.click(screen.getByRole('button', { name: '选择页面元素' }))
    const overlay = document.querySelector('.browser-element-inspector')!
    fireEvent(overlay, pointerEvent('pointermove', 0))
    for (let i = 0; i < 5; i++) {
      await act(async () => {
        await vi.advanceTimersByTimeAsync(30)
      })
      fireEvent(overlay, pointerEvent('pointermove', 0))
    }
    expect(source.inspectPoint).toHaveBeenCalledTimes(1)
    await act(async () => {
      finish(element)
    })
    expect(document.querySelector('.browser-element-inspector-outline')).toBeInTheDocument()
    await act(async () => {
      await vi.advanceTimersByTimeAsync(50)
    })
    expect(source.inspectPoint).toHaveBeenCalledTimes(2)
    await act(async () => {
      finish(element)
    })
  })

  it('keeps a clicked selection while its inspect reply is pending instead of issuing hover calls', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    let resolve!: (element: {
      ref: string
      revision: number
      role: string
      url: string
      bounds: { x: number; y: number; width: number; height: number }
    }) => void
    const source = {
      ...httpBrowserSource,
      inspectPoint: vi.fn(
        () =>
          new Promise<Parameters<typeof resolve>[0]>((done) => {
            resolve = done
          }),
      ),
    }
    render(<BrowserView endpoint="/browser" source={source} onElementSelect={vi.fn()} />)
    await configuredFrame()
    fireEvent.click(screen.getByRole('button', { name: '选择页面元素' }))
    const overlay = document.querySelector('.browser-element-inspector')!
    fireEvent(overlay, pointerEvent('pointerdown'))
    fireEvent(overlay, pointerEvent('pointerup', 0))
    fireEvent(overlay, pointerEvent('pointermove'))
    expect(source.inspectPoint).toHaveBeenCalledTimes(1)
    fireEvent.wheel(overlay, { clientX: 100, clientY: 100, deltaY: 20 })
    expect(sentInputs()).toHaveLength(0)
    await act(async () =>
      resolve({
        ref: 'r_1',
        revision: 1,
        role: 'button',
        url: 'https://example.com',
        bounds: { x: 90, y: 90, width: 80, height: 30 },
      }),
    )
    expect(screen.getByRole('dialog', { name: '选中页面内容' })).toBeInTheDocument()
  })

  it('captures a dragged page region with viewport coordinates and a PNG', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    vi.spyOn(HTMLCanvasElement.prototype, 'toBlob').mockImplementation((callback) =>
      callback(new Blob(['png'], { type: 'image/png' })),
    )
    const selected = vi.fn()
    render(<BrowserView endpoint="/browser" onElementSelect={vi.fn()} onAreaSelect={selected} />)
    await configuredFrame()
    fireEvent.click(screen.getByRole('button', { name: '选择页面元素' }))
    const overlay = document.querySelector('.browser-element-inspector')!
    const drag = (type: string, x: number, y: number) => {
      const event = new Event(type, { bubbles: true, cancelable: true })
      Object.defineProperties(event, {
        clientX: { value: x },
        clientY: { value: y },
        pointerId: { value: 7 },
        pointerType: { value: 'mouse' },
        button: { value: 0 },
        buttons: { value: 1 },
      })
      return event
    }
    fireEvent(overlay, drag('pointerdown', 100, 120))
    fireEvent(overlay, drag('pointermove', 220, 200))
    fireEvent(overlay, drag('pointerup', 220, 200))
    expect(document.querySelector('.browser-element-inspector-area')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '确认' }))
    expect(selected).toHaveBeenCalledWith(
      expect.objectContaining({
        pageID: 'page-a',
        bounds: { x: 100, y: 120, width: 120, height: 80 },
        viewport: defaultViewport,
        image: expect.any(Blob),
      }),
    )
    expect(screen.getByRole('button', { name: '选择页面元素' })).toHaveAttribute('aria-pressed', 'true')
  })

  it('discards a pending region image after selection is cancelled', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    let finishImage: BlobCallback | undefined
    vi.spyOn(HTMLCanvasElement.prototype, 'toBlob').mockImplementation((callback) => {
      finishImage = callback
    })
    render(<BrowserView endpoint="/browser" onAreaSelect={vi.fn()} />)
    await configuredFrame()
    fireEvent.click(screen.getByRole('button', { name: '选择页面元素' }))
    const overlay = document.querySelector('.browser-element-inspector')!
    const drag = (type: string, x: number, y: number) => {
      const event = new Event(type, { bubbles: true, cancelable: true })
      Object.defineProperties(event, {
        clientX: { value: x },
        clientY: { value: y },
        pointerId: { value: 7 },
        pointerType: { value: 'mouse' },
        button: { value: 0 },
        buttons: { value: 1 },
      })
      return event
    }
    fireEvent(overlay, drag('pointerdown', 100, 120))
    fireEvent(overlay, drag('pointerup', 220, 200))
    expect(finishImage).toBeDefined()
    fireEvent.click(screen.getByRole('button', { name: '选择页面元素' }))
    await act(async () => {
      finishImage?.(new Blob(['png'], { type: 'image/png' }))
    })
    expect(screen.queryByRole('dialog', { name: '选中页面内容' })).toBeNull()
    expect(document.querySelector('.browser-element-inspector')).toBeNull()
  })

  it('hides all controls while retaining the live canvas and caller styles', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    render(<BrowserView endpoint="/browser" controls={false} className="embedded" style={{ height: 320 }} />)
    const frame = await configuredFrame()
    expect(document.querySelector('[data-part="toolbar"]')).toBeNull()
    expect(screen.queryByRole('tablist')).toBeNull()
    expect(screen.queryByRole('button')).toBeNull()
    expect(screen.getByRole('region')).toHaveClass('embedded')
    expect(screen.getByRole('region')).toHaveStyle({ height: '320px' })
    fireEvent(frame, pointerEvent('pointerdown'))
    expect(sentInputs()).toEqual([expect.objectContaining({ kind: 'mouse', action: 'down' })])
  })

  it('configures individual actions and collapses an empty toolbar without reconnecting', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    const { rerender } = render(
      <BrowserView
        endpoint="/browser"
        devtools
        controls={{
          addressBar: false,
          newTab: false,
          closeTab: false,
          closeBrowser: false,
          devtools: false,
          devtoolsFloating: false,
          keyboard: false,
        }}
      />,
    )
    await configuredFrame()
    const count = sockets.length
    expect(screen.getByRole('tab')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '刷新当前页面' })).toBeInTheDocument()
    expect(screen.queryByRole('textbox', { name: '浏览器页面地址' })).toBeNull()
    expect(screen.queryByRole('button', { name: '新建浏览器 Tab' })).toBeNull()
    expect(screen.queryByRole('button', { name: '前往页面' })).toBeNull()
    expect(screen.queryByRole('button', { name: '关闭 浏览器 Tab' })).toBeNull()
    rerender(
      <BrowserView
        endpoint="/browser"
        devtools
        controls={{
          tabs: false,
          addressBar: false,
          back: false,
          forward: false,
          reload: false,
          closeBrowser: false,
          devtools: false,
          devtoolsFloating: false,
          recording: false,
          keyboard: false,
        }}
      />,
    )
    expect(document.querySelector('[data-part="toolbar"]')).toBeNull()
    expect(sockets).toHaveLength(count)
    rerender(<BrowserView endpoint="/browser" devtools />)
    expect(screen.getByRole('textbox', { name: '浏览器页面地址' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '新建浏览器 Tab' })).toBeInTheDocument()
    expect(sockets).toHaveLength(count)
  })

  it('starts manual recording and saves it through the stop dialog', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    render(<BrowserView endpoint="/browser" />)
    await configuredFrame()
    fireEvent.click(screen.getByRole('button', { name: '开始操作录制' }))
    await act(async () => {
      await Promise.resolve()
    })
    expect(mockedStartRecording).toHaveBeenCalledWith('/browser', 'page-a', expect.any(String))
    await receiveState(singleTabState({}, { recording: true, recorded_steps: 3 }))
    fireEvent.click(screen.getByRole('button', { name: '停止并保存操作录制' }))
    expect(screen.getByRole('dialog', { name: '保存操作录制' })).toBeInTheDocument()
    fireEvent.click(screen.getByLabelText('保存到指定文件'))
    fireEvent.change(screen.getByLabelText('操作序列保存路径'), { target: { value: '/tmp/manual.json' } })
    fireEvent.click(screen.getByRole('button', { name: '停止并保存' }))
    await act(async () => {
      await Promise.resolve()
    })
    expect(mockedFinishRecording).toHaveBeenCalledWith('/browser', '/tmp/manual.json', expect.any(String))
    expect(screen.queryByRole('dialog', { name: '保存操作录制' })).toBeNull()
    expect(screen.getByRole('alert')).toHaveTextContent('/tmp/sequence.json')
  })

  it('uses toolbar buttons and only offers navigation after the address changes', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    render(<BrowserView endpoint="/browser" onElementSelect={vi.fn()} />)
    await configuredFrame()
    const input = screen.getByRole('textbox', { name: '浏览器页面地址' }) as HTMLInputElement
    expect(input).toHaveClass('browser-address-input')
    expect(document.querySelector('.browser-live-address-row i')).toBeNull()
    fireEvent.focus(input)
    expect(input.selectionStart).toBe(0)
    expect(input.selectionEnd).toBe(input.value.length)
    expect(screen.getByRole('button', { name: '刷新当前页面' })).toHaveClass('browser-toolbar-button')
    expect(screen.getByRole('button', { name: '选择页面元素' })).toHaveClass('browser-toolbar-button')
    expect(screen.queryByRole('button', { name: '前往页面' })).toBeNull()
    fireEvent.change(input, { target: { value: 'https://example.com/next' } })
    expect(screen.getByRole('button', { name: '前往页面' })).toHaveClass('browser-toolbar-button')
    fireEvent.change(input, { target: { value: 'https://example.com' } })
    expect(screen.queryByRole('button', { name: '前往页面' })).toBeNull()
  })

  it('requires the explicit keyboard button in manual activation mode', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    const { rerender } = render(<BrowserView endpoint="/browser" input={{ keyboardActivation: 'manual' }} />)
    const frame = await configuredFrame()
    fireEvent(frame, pointerEvent('pointerdown'))
    expect(frame).toHaveFocus()
    const keyboard = screen.getByLabelText('浏览器直播键盘输入')
    expect(keyboard).toHaveAttribute('inputmode', 'none')
    fireEvent.click(screen.getByLabelText('开启页面输入'))
    expect(keyboard).toHaveFocus()
    expect(keyboard).toHaveAttribute('inputmode', 'text')
    fireEvent.compositionEnd(keyboard, { data: '中文输入' })
    expect(sentInputs()).toContainEqual(expect.objectContaining({ kind: 'text', text: '中文输入' }))
    rerender(<BrowserView endpoint="/browser" input={{ keyboard: false }} />)
    expect(screen.queryByLabelText('浏览器直播键盘输入')).toBeNull()
    expect(screen.queryByRole('button', { name: '关闭页面输入' })).toBeNull()
    socketMessages.length = 0
    fireEvent.keyDown(frame, { key: 'a', code: 'KeyA' })
    fireEvent.compositionEnd(frame, { data: '不会发送' })
    fireEvent.paste(frame, { clipboardData: { getData: () => '不会发送' } })
    expect(socketMessages).toEqual([])
  })

  it('disables every page input independently of the toolbar and host scrolling', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    render(<BrowserView endpoint="/browser" input={false} />)
    const frame = await configuredFrame()
    fireEvent(frame, pointerEvent('pointerdown'))
    fireEvent(frame, pointerEvent('pointermove'))
    fireEvent(frame, pointerEvent('pointerup', 0))
    fireEvent.keyDown(frame, { key: 'Enter', code: 'Enter' })
    const wheel = new WheelEvent('wheel', { bubbles: true, cancelable: true, clientX: 100, clientY: 100, deltaY: 10 })
    fireEvent(frame, wheel)
    expect(wheel.defaultPrevented).toBe(false)
    const touch = new Event('touchstart', { bubbles: true, cancelable: true })
    fireEvent(frame, touch)
    expect(touch.defaultPrevented).toBe(false)
    expect(frame.style.touchAction).toBe('auto')
    expect(socketMessages).toEqual([])
    expect(screen.getByRole('button', { name: '刷新当前页面' })).toBeEnabled()
  })

  it('changes input types independently while preserving navigation and input focus rules', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    const { rerender } = render(<BrowserView endpoint="/browser" input={{ mouse: false, touch: false }} />)
    const frame = await configuredFrame()
    fireEvent(frame, pointerEvent('pointerdown'))
    fireEvent.wheel(frame, { clientX: 100, clientY: 100, deltaY: 10 })
    act(() => {
      vi.advanceTimersByTime(60)
    })
    expect(sentInputs()).toEqual([expect.objectContaining({ kind: 'mouse', action: 'wheel' })])
    rerender(<BrowserView endpoint="/browser" input={{ wheel: false }} />)
    socketMessages.length = 0
    fireEvent.wheel(frame, { clientX: 100, clientY: 100, deltaY: 10 })
    fireEvent(frame, pointerEvent('pointerdown'))
    expect(sentInputs()).toEqual([expect.objectContaining({ kind: 'mouse', action: 'down' })])
    expect(screen.getByLabelText('浏览器直播键盘输入')).toHaveFocus()
  })

  it('falls back to an allowed diagnostic panel when configuration changes', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    const { rerender } = render(
      <BrowserView endpoint="/browser" devtools={{ open: true, panels: ['console', 'network'] }} />,
    )
    await act(async () => {})
    expect(screen.getByRole('button', { name: /^Console/ })).toHaveClass('active')
    rerender(<BrowserView endpoint="/browser" devtools={{ open: true, panels: ['network'] }} />)
    expect(screen.queryByRole('button', { name: /^Console/ })).toBeNull()
    expect(screen.getByRole('button', { name: /^Network/ })).toHaveClass('active')
  })

  it('allows keyboard text while blocking copy and paste paths', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    render(<BrowserView endpoint="/browser" input={{ clipboard: false }} />)
    const frame = await configuredFrame()
    fireEvent.keyDown(frame, { key: 'c', code: 'KeyC', ctrlKey: true })
    fireEvent.keyUp(frame, { key: 'c', code: 'KeyC', ctrlKey: true })
    fireEvent.keyDown(frame, { key: 'v', code: 'KeyV', metaKey: true })
    fireEvent.keyUp(frame, { key: 'v', code: 'KeyV', metaKey: true })
    fireEvent.paste(frame, { clipboardData: { getData: () => '不可粘贴' } })
    expect(socketMessages).toEqual([])
    fireEvent.compositionEnd(screen.getByLabelText('浏览器直播键盘输入'), { data: '允许输入' })
    expect(sentInputs()).toEqual([{ kind: 'text', text: '允许输入' }])
  })

  it('releases held input and drops buffered samples when client input is disabled', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    const { rerender } = render(<BrowserView endpoint="/browser" />)
    const frame = await configuredFrame()
    fireEvent(frame, pointerEvent('pointerdown'))
    fireEvent.keyDown(frame, { key: 'Shift', code: 'ShiftLeft', shiftKey: true })
    sockets.at(-1)!.bufferedAmount = 64 * 1024
    fireEvent(frame, pointerEvent('pointermove'))
    fireEvent.wheel(frame, { clientX: 100, clientY: 100, deltaY: 10 })
    rerender(<BrowserView endpoint="/browser" input={false} />)
    expect(sentInputs()).toContainEqual(expect.objectContaining({ kind: 'mouse', action: 'up', buttons: 0 }))
    expect(sentInputs()).toContainEqual(expect.objectContaining({ kind: 'key', action: 'up', code: 'ShiftLeft' }))
    const count = socketMessages.length
    sockets.at(-1)!.bufferedAmount = 0
    await act(async () => {
      vi.advanceTimersByTime(100)
    })
    expect(socketMessages).toHaveLength(count)
  })

  it('controls takeover and observation copy separately without granting access', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    const { rerender } = render(<BrowserView endpoint="/browser" controls={{ controlStatus: false }} />)
    await act(async () => {})
    await receiveFrameWithControl({ controller: false, can_takeover: true, revision: 1 })
    expect(screen.getByRole('button', { name: '获取操作权' })).toBeInTheDocument()
    expect(screen.queryByText('当前浏览器正在由其他窗口控制')).toBeNull()
    rerender(<BrowserView endpoint="/browser" controls={{ controlStatus: false, takeover: false }} />)
    expect(screen.queryByRole('status', { name: '浏览器观察模式' })).toBeNull()
    const frame = screen.getByRole('application')
    fireEvent.keyDown(frame, { key: 'a', code: 'KeyA' })
    expect(socketMessages).toEqual([])
  })

  it('supports controlled devtools, panel selection and a disabled F12 shortcut', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    const onOpenChange = vi.fn()
    const { rerender } = render(
      <BrowserView
        endpoint="/browser"
        devtools={{
          open: false,
          onOpenChange,
          panels: ['network'],
          shortcut: false,
          captureControl: false,
          resizable: false,
        }}
      />,
    )
    await configuredFrame()
    fireEvent.keyDown(screen.getByRole('region', { name: '浏览器页面直播' }), { key: 'F12' })
    expect(onOpenChange).not.toHaveBeenCalled()
    fireEvent.click(screen.getByRole('button', { name: '切换开发诊断 F12' }))
    expect(onOpenChange).toHaveBeenCalledWith(true)
    expect(screen.queryByRole('region', { name: '开发诊断' })).toBeNull()
    rerender(
      <BrowserView
        endpoint="/browser"
        devtools={{
          open: true,
          onOpenChange,
          panels: ['network'],
          shortcut: false,
          captureControl: false,
          resizable: false,
          closeButton: false,
        }}
      />,
    )
    expect(screen.getByRole('region', { name: '开发诊断' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /^Network/ })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /^Console/ })).toBeNull()
    expect(screen.queryByRole('button', { name: /^Resources/ })).toBeNull()
    expect(screen.queryByRole('button', { name: /开始采集|停止采集/ })).toBeNull()
    expect(screen.queryByRole('button', { name: '关闭开发诊断面板' })).toBeNull()
    expect(screen.queryByRole('separator')).toBeNull()
    rerender(<BrowserView endpoint="/browser" devtools={{ panels: [] }} />)
    expect(screen.queryByRole('button', { name: '切换开发诊断 F12' })).toBeNull()
    expect(screen.queryByRole('region', { name: '开发诊断' })).toBeNull()
  })

  it('moves the devtools capture status into the capture button', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    render(<BrowserView endpoint="/browser" devtools={{ open: true }} />)
    await act(async () => {})
    const capture = screen.getByRole('button', { name: '开始采集' })
    expect(capture.querySelector('i[data-status="idle"]')).toBeInTheDocument()
    expect(screen.queryByText('未开始')).toBeNull()
    expect(document.querySelector('.browser-devtools-header > strong')).toBeNull()
  })

  it('reports rejected live input without dropping browser control', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    const onError = vi.fn()
    render(<BrowserView endpoint="/browser" onError={onError} />)
    await act(async () => {})
    await receiveFrame()
    await act(async () => {
      sockets.at(-1)?.onmessage?.(
        new MessageEvent('message', {
          data: JSON.stringify({
            type: 'browser_input_error',
            error: { kind: 'input_overloaded', message: '浏览器输入队列已满', recoverable: true },
          }),
        }),
      )
    })
    expect(screen.getByText('浏览器输入队列已满')).toBeInTheDocument()
    expect(onError).toHaveBeenCalledWith(expect.objectContaining({ message: '浏览器输入队列已满' }))
    expect(screen.queryByRole('button', { name: '接管浏览器' })).toBeNull()
  })

  it('draws streamed browser frames on canvas without an image element', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())

    render(<BrowserLiveWindow sessionID="session-a" enabled defaultOpen />)
    await act(async () => {})
    const canvas = screen.getByLabelText('浏览器页面当前画面') as HTMLCanvasElement
    expect(screen.getByRole('region', { name: '浏览器页面直播' })).toBeInTheDocument()
    expect(canvas.tagName).toBe('CANVAS')
    expect(document.querySelector('.browser-live-window-frame img')).toBeNull()

    await receiveFrame()
    expect(canvas).toHaveClass('browser-live-window-canvas-ready')
    expect(canvas.width).toBe(1_920)
    expect(canvas.height).toBe(1_080)
    expect(drawImage).toHaveBeenCalledOnce()
  })

  it('keeps only the latest streamed frame while a frame is decoding', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    let release!: (bitmap: { width: number; height: number; close: () => void }) => void
    createBitmap.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          release = resolve
        }),
    )
    render(<BrowserLiveWindow sessionID="session-a" enabled defaultOpen />)
    await act(async () => {})
    const socket = sockets.at(-1)!
    await act(async () => {
      for (const value of ['first', 'second', 'third'])
        socket.onmessage?.(new MessageEvent('message', { data: new Blob([value]) }))
      for (let index = 0; index < 10; index++) await Promise.resolve()
    })
    expect(createBitmap).toHaveBeenCalledTimes(1)
    await act(async () => {
      release({ width: 1_920, height: 1_080, close: vi.fn() })
      for (let index = 0; index < 10; index++) await Promise.resolve()
    })
    expect(drawImage).toHaveBeenCalledTimes(2)
  })

  it('refreshes and clears stale tabs when the state socket closes with the browser runtime', async () => {
    mockedFetchBrowserState
      .mockResolvedValueOnce({
        available: true,
        closed: false,
        active_page_id: 'page-a',
        tabs_revision: 0,
        tabs: [{ page_id: 'page-a', url: 'https://example.com', status: 'ready', active: true }],
      })
      .mockResolvedValue(unavailableState(true))

    render(<BrowserLiveWindow sessionID="session-a" enabled defaultOpen />)
    await act(async () => {})
    await receiveFrame()
    expect(screen.getByRole('region', { name: '浏览器页面直播' })).toBeInTheDocument()

    await act(async () => {
      sockets[0]?.onclose?.()
      await Promise.resolve()
      await Promise.resolve()
    })

    expect(mockedFetchBrowserState).toHaveBeenCalledTimes(2)
    expect(screen.getByRole('region', { name: '浏览器页面直播' })).toBeInTheDocument()
    expect(screen.queryByText('Example')).toBeNull()
  })

  it('places the browser Tab bar above the URL row in the side panel', async () => {
    mockedFetchBrowserState.mockResolvedValue({
      available: true,
      closed: false,
      active_page_id: 'page-a',
      tabs_revision: 0,
      tabs: [{ page_id: 'page-a', title: 'Example', url: 'https://example.com', status: 'complete', active: true }],
    })

    render(<BrowserLiveWindow sessionID="session-a" enabled defaultOpen resourcePanelOpen sidePanelOpen />)
    await act(async () => {})
    await receiveFrame()

    const window = screen.getByRole('region', { name: '浏览器页面直播' })
    const children = Array.from(window.children)
    expect(children.findIndex((child) => child.getAttribute('role') === 'tablist')).toBeLessThan(
      children.findIndex((child) => child.tagName === 'HEADER'),
    )
  })

  it('switches Tab optimistically and reuses the activate response state', async () => {
    const tabs = (active: string) => [
      { page_id: 'page-a', title: 'A', status: 'ready' as const, active: active === 'page-a' },
      { page_id: 'page-b', title: 'B', status: 'ready' as const, active: active === 'page-b' },
    ]
    const stateFor = (active: 'page-a' | 'page-b'): BrowserViewState => ({
      available: true,
      closed: false,
      active_page_id: active,
      tabs_revision: 1,
      url: `https://example.com/${active === 'page-a' ? 'a' : 'b'}`,
      tabs: tabs(active),
    })
    let resolveActivate: ((state: BrowserViewState) => void) | undefined
    const readState = vi.fn(async () => stateFor('page-a'))
    const source = {
      ...httpBrowserSource,
      state: readState,
      cachedFrame: vi.fn(async () => {
        throw new Error('没有缓存帧')
      }),
      activate: vi.fn(() => new Promise<BrowserViewState>((resolve) => (resolveActivate = resolve))),
    }
    render(<BrowserView endpoint="session-a" pageID="page-a" source={source} />)
    await act(async () => {})
    await receiveFrame()
    await receiveState(stateFor('page-a'))
    const tabB = screen.getByRole('tab', { name: 'B' })
    fireEvent.click(tabB)
    // 服务端确认前即完成本地高亮切换。
    expect(tabB).toHaveAttribute('aria-selected', 'true')
    const stateReads = readState.mock.calls.length
    await act(async () => {
      resolveActivate?.(stateFor('page-b'))
      await Promise.resolve()
    })
    expect(readState).toHaveBeenCalledTimes(stateReads)
    expect(screen.getByRole('textbox', { name: '浏览器页面地址' })).toHaveValue('https://example.com/b')
  })

  it('updates Tab titles from a stale state frame without undoing the optimistic selection', async () => {
    const tabs = (active: string, suffix = '') => [
      {
        page_id: 'page-a',
        title: `A${suffix}`,
        url: 'https://example.com/a',
        status: 'ready' as const,
        active: active === 'page-a',
      },
      {
        page_id: 'page-b',
        title: `B${suffix}`,
        url: 'https://example.com/b',
        status: 'ready' as const,
        active: active === 'page-b',
      },
    ]
    const stateFor = (active: 'page-a' | 'page-b', suffix = ''): BrowserViewState => ({
      available: true,
      closed: false,
      active_page_id: active,
      tabs_revision: 1,
      url: `https://example.com/${active === 'page-a' ? 'a' : 'b'}`,
      tabs: tabs(active, suffix),
    })
    let resolveActivate: ((state: BrowserViewState) => void) | undefined
    const source = {
      ...httpBrowserSource,
      state: vi.fn(async () => stateFor('page-a')),
      cachedFrame: vi.fn(async () => {
        throw new Error('没有缓存帧')
      }),
      activate: vi.fn(() => new Promise<BrowserViewState>((resolve) => (resolveActivate = resolve))),
    }
    render(<BrowserView endpoint="session-a" pageID="page-a" source={source} />)
    await act(async () => {})
    await receiveFrame()
    await receiveState(stateFor('page-a'))
    fireEvent.click(screen.getByRole('tab', { name: 'B' }))
    // 切换确认前，旧活动页的状态帧仍可更新标题，但不能把高亮和地址切回 A。
    await receiveState(stateFor('page-a', '-updated'))
    expect(screen.getByRole('tab', { name: 'B-updated' })).toHaveAttribute('aria-selected', 'true')
    expect(screen.getByRole('textbox', { name: '浏览器页面地址' })).toHaveValue('https://example.com/b')
    await act(async () => {
      resolveActivate?.(stateFor('page-b'))
      await Promise.resolve()
    })
  })

  it('does not automatically take control or retry when a Tab switch loses ownership', async () => {
    const tabs = (active: string) => [
      { page_id: 'page-a', title: 'A', status: 'ready' as const, active: active === 'page-a' },
      { page_id: 'page-b', title: 'B', status: 'ready' as const, active: active === 'page-b' },
    ]
    mockedFetchBrowserState.mockResolvedValue({
      available: true,
      closed: false,
      active_page_id: 'page-a',
      tabs_revision: 1,
      url: 'https://example.com/a',
      tabs: tabs('page-a'),
    })
    mockedActivateBrowserTab.mockRejectedValueOnce(new Error('当前窗口没有浏览器操作权'))
    render(<BrowserView endpoint="session-a" pageID="page-a" />)
    await act(async () => {})
    await receiveFrame()
    await receiveState({
      available: true,
      closed: false,
      active_page_id: 'page-a',
      tabs_revision: 1,
      url: 'https://example.com/a',
      tabs: tabs('page-a'),
    })
    fireEvent.click(screen.getByRole('tab', { name: 'B' }))
    await act(async () => {
      await vi.advanceTimersByTimeAsync(200)
    })
    expect(mockedTakeBrowserControl).not.toHaveBeenCalled()
    expect(mockedActivateBrowserTab).toHaveBeenCalledTimes(1)
    expect(screen.getByText(/切换浏览器 Tab 失败/)).toBeInTheDocument()
  })

  it('does not take control when an observer opens or switches a host Tab', async () => {
    mockedFetchBrowserState.mockResolvedValue({
      ...singleTabState(),
      tabs: [
        { page_id: 'page-a', title: 'A', active: true, status: 'ready' },
        { page_id: 'page-b', title: 'B', active: false, status: 'ready' },
      ],
    })
    const view = render(<BrowserView endpoint="session-a" pageID="page-b" followActivePage={false} />)
    await act(async () => {})
    await receiveFrameWithControl({ controller: false, can_takeover: true, revision: 1 })
    await act(async () => {
      await vi.advanceTimersByTimeAsync(500)
    })
    view.rerender(<BrowserView endpoint="session-a" pageID="page-a" followActivePage={false} />)
    view.rerender(<BrowserView endpoint="session-a" pageID="page-b" followActivePage={false} />)
    await act(async () => {
      await vi.advanceTimersByTimeAsync(500)
    })
    expect(mockedTakeBrowserControl).not.toHaveBeenCalled()
    expect(mockedActivateBrowserTab).not.toHaveBeenCalled()
    expect(mockedSetBrowserTabViewport).not.toHaveBeenCalled()
    expect(screen.getByRole('button', { name: '获取操作权' })).toBeEnabled()
  })

  it('places back, forward and reload on the left and sends history directions', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    render(<BrowserView endpoint="/browser" />)
    await configuredFrame()
    const back = screen.getByRole('button', { name: '后退' })
    const forward = screen.getByRole('button', { name: '前进' })
    const reload = screen.getByRole('button', { name: '刷新当前页面' })
    expect(Array.from(back.parentElement!.children).slice(0, 3)).toEqual([back, forward, reload])
    fireEvent.click(back)
    await act(async () => {})
    expect(mockedHistoryBrowserTab).toHaveBeenCalledWith('/browser', 'page-a', 'back', expect.any(String))
    fireEvent.click(forward)
    await act(async () => {})
    expect(mockedHistoryBrowserTab).toHaveBeenCalledWith('/browser', 'page-a', 'forward', expect.any(String))
  })

  it('keeps a newly active Tab when the host selection has not changed', async () => {
    const tabs = (active: string) => [
      { page_id: 'page-a', title: 'A', status: 'ready' as const, active: active === 'page-a' },
      { page_id: 'page-b', title: 'B', status: 'ready' as const, active: active === 'page-b' },
    ]
    mockedFetchBrowserState.mockResolvedValue({
      available: true,
      closed: false,
      active_page_id: 'page-a',
      tabs_revision: 1,
      tabs: tabs('page-a'),
    })
    const { rerender } = render(<BrowserView endpoint="session-a" pageID="page-a" />)
    await act(async () => {})
    await receiveFrame()

    await receiveState({
      available: true,
      closed: false,
      active_page_id: 'page-b',
      tabs_revision: 2,
      tabs: tabs('page-b'),
    })
    expect(mockedActivateBrowserTab).not.toHaveBeenCalled()

    rerender(<BrowserView endpoint="session-a" pageID="page-b" />)
    await act(async () => {})
    expect(mockedActivateBrowserTab).not.toHaveBeenCalled()

    rerender(<BrowserView endpoint="session-a" pageID="page-a" />)
    await act(async () => {})
    expect(mockedActivateBrowserTab).toHaveBeenCalledTimes(1)
    expect(mockedActivateBrowserTab).toHaveBeenCalledWith('session-a', 'page-a', expect.any(String))
  })

  it('activates a newly hosted page when the server is still on the previous page', async () => {
    const pages = (active: string) => [
      { page_id: 'page-a', title: 'A', status: 'ready' as const, active: active === 'page-a' },
      { page_id: 'page-b', title: 'B', status: 'ready' as const, active: active === 'page-b' },
    ]
    mockedFetchBrowserState.mockResolvedValue({
      available: true,
      closed: false,
      active_page_id: 'page-a',
      tabs_revision: 1,
      url: 'https://example.com/a',
      tabs: pages('page-a'),
    })
    render(<BrowserView endpoint="session-a" pageID="page-b" />)
    await act(async () => {})
    await receiveFrame()
    await receiveState({
      available: true,
      closed: false,
      active_page_id: 'page-a',
      tabs_revision: 2,
      url: 'https://example.com/a',
      tabs: pages('page-a'),
    })
    await act(async () => {})
    expect(mockedActivateBrowserTab).toHaveBeenCalledWith('session-a', 'page-b', expect.any(String))
  })

  it('re-activates the host selected Tab after becoming visible again', async () => {
    const tabs = (active: string) => [
      { page_id: 'page-a', title: 'A', status: 'ready' as const, active: active === 'page-a' },
      { page_id: 'page-b', title: 'B', status: 'ready' as const, active: active === 'page-b' },
    ]
    mockedFetchBrowserState
      .mockResolvedValueOnce({
        available: true,
        closed: false,
        active_page_id: 'page-a',
        tabs_revision: 1,
        url: 'https://example.com/a',
        tabs: tabs('page-a'),
      })
      .mockResolvedValue({
        available: true,
        closed: false,
        active_page_id: 'page-b',
        tabs_revision: 2,
        url: 'https://example.com/b',
        tabs: tabs('page-b'),
      })
    const { rerender } = render(<BrowserView endpoint="session-a" pageID="page-a" />)
    await act(async () => {})
    await receiveFrame()
    await receiveState({
      available: true,
      closed: false,
      active_page_id: 'page-b',
      tabs_revision: 2,
      url: 'https://example.com/b',
      tabs: tabs('page-b'),
    })
    expect(mockedActivateBrowserTab).not.toHaveBeenCalled()
    rerender(<BrowserView endpoint="session-a" pageID="page-a" liveView={false} />)
    rerender(<BrowserView endpoint="session-a" pageID="page-a" liveView />)
    await act(async () => {})
    await receiveFrame()
    expect(mockedActivateBrowserTab).toHaveBeenCalledWith('session-a', 'page-a', expect.any(String))
  })

  it('debounces the measured side-panel page area into the active remote viewport', async () => {
    let resized: ResizeObserverCallback | undefined
    vi.stubGlobal(
      'ResizeObserver',
      class implements ResizeObserver {
        constructor(callback: ResizeObserverCallback) {
          resized = callback
        }
        observe() {}
        unobserve() {}
        disconnect() {}
      },
    )
    mockedFetchBrowserState.mockResolvedValue({
      available: true,
      closed: false,
      active_page_id: 'page-a',
      tabs_revision: 0,
      tabs: [
        {
          page_id: 'page-a',
          url: 'https://example.com',
          viewport: { width: 1_920, height: 1_080 },
          status: 'ready',
          active: true,
        },
      ],
    })

    render(<BrowserLiveWindow sessionID="session-a" enabled defaultOpen resourcePanelOpen sidePanelOpen />)
    await act(async () => {})
    await receiveFrame()

    act(() =>
      resized?.([{ contentRect: { width: 812.3, height: 604.4 } } as ResizeObserverEntry], {} as ResizeObserver),
    )
    act(() =>
      resized?.([{ contentRect: { width: 845.8, height: 621.2 } } as ResizeObserverEntry], {} as ResizeObserver),
    )
    await act(async () => {
      vi.advanceTimersByTime(199)
      await Promise.resolve()
    })
    expect(mockedSetBrowserTabViewport).not.toHaveBeenCalled()
    await act(async () => {
      vi.advanceTimersByTime(1)
      await Promise.resolve()
    })
    expect(mockedSetBrowserTabViewport).toHaveBeenCalledOnce()
    expect(mockedSetBrowserTabViewport).toHaveBeenCalledWith(
      'session-a',
      'page-a',
      { width: 846, height: 621 },
      expect.any(String),
      undefined,
    )
    const canvas = screen.getByLabelText('浏览器页面当前画面')
    expect(canvas).toHaveClass('browser-live-window-canvas-ready')
    const drawsBeforeMismatch = drawImage.mock.calls.length
    await receiveFrame()
    expect(canvas).toHaveClass('browser-live-window-canvas-ready')
    expect(drawImage).toHaveBeenCalledTimes(drawsBeforeMismatch + 1)
    await act(async () => {
      vi.advanceTimersByTime(100)
      await Promise.resolve()
    })
    await receiveFrame()
    await act(async () => {
      vi.advanceTimersByTime(100)
      await Promise.resolve()
    })
    expect(mockedSetBrowserTabViewport).toHaveBeenCalledTimes(2)
    await receiveFrame()
    await act(async () => {
      vi.advanceTimersByTime(200)
      await Promise.resolve()
    })
    expect(mockedSetBrowserTabViewport).toHaveBeenCalledTimes(3)
    // 实际帧尺寸与目标尺寸不一致时，重试必须显式要求服务端重新下发视口，
    // 否则 Manager 会因记录的尺寸相同而直接返回。
    expect(mockedSetBrowserTabViewport).toHaveBeenLastCalledWith(
      'session-a',
      'page-a',
      { width: 846, height: 621 },
      expect.any(String),
      true,
    )
    createBitmap.mockResolvedValueOnce({ width: 846, height: 621, close: vi.fn() })
    await receiveFrame()
    expect(canvas).toHaveClass('browser-live-window-canvas-ready')
    await act(async () => {
      vi.advanceTimersByTime(500)
      await Promise.resolve()
    })
    expect(mockedSetBrowserTabViewport).toHaveBeenCalledTimes(3)
  })

  it('keeps later viewers read-only until they explicitly take browser control', async () => {
    let resized: ResizeObserverCallback | undefined
    vi.stubGlobal(
      'ResizeObserver',
      class implements ResizeObserver {
        constructor(callback: ResizeObserverCallback) {
          resized = callback
        }
        observe() {}
        unobserve() {}
        disconnect() {}
      },
    )
    mockedFetchBrowserState.mockResolvedValue({
      available: true,
      closed: false,
      active_page_id: 'page-a',
      tabs_revision: 0,
      tabs: [
        {
          page_id: 'page-a',
          title: 'Example',
          viewport: { width: 1_920, height: 1_080 },
          status: 'ready',
          active: true,
        },
      ],
    })

    render(<BrowserLiveWindow sessionID="session-a" enabled defaultOpen resourcePanelOpen sidePanelOpen />)
    await act(async () => {})
    await receiveFrameWithControl({ controller: false, can_takeover: true, revision: 1 })

    expect(screen.getByRole('status', { name: '浏览器观察模式' })).toHaveTextContent('当前浏览器正在由其他窗口控制')
    expect(screen.getByRole('textbox', { name: '浏览器页面地址' })).toBeDisabled()
    expect(screen.getByRole('tab', { name: 'Example' })).toBeDisabled()
    act(() => resized?.([{ contentRect: { width: 390, height: 700 } } as ResizeObserverEntry], {} as ResizeObserver))
    await act(async () => {
      vi.advanceTimersByTime(200)
      await Promise.resolve()
    })
    expect(mockedSetBrowserTabViewport).not.toHaveBeenCalled()

    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: '获取操作权' }))
      await Promise.resolve()
    })
    expect(mockedTakeBrowserControl).toHaveBeenCalledWith('session-a', expect.any(String))
    const switching = screen.getByRole('button', { name: '正在切换…' })
    expect(switching).toBeDisabled()
    expect(switching).toHaveAttribute('aria-busy', 'true')
    expect(switching.querySelector('.browser-control-spinner')).not.toBeNull()
    fireEvent.click(switching)
    expect(mockedTakeBrowserControl).toHaveBeenCalledTimes(1)
    await act(async () => {
      await vi.advanceTimersByTimeAsync(399)
    })
    expect(screen.getByRole('button', { name: '正在切换…' })).toBeInTheDocument()
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1)
    })
    expect(screen.queryByRole('status', { name: '浏览器观察模式' })).toBeNull()
    expect(screen.getByRole('textbox', { name: '浏览器页面地址' })).toBeEnabled()
    await act(async () => {
      vi.advanceTimersByTime(200)
      await Promise.resolve()
    })
    expect(mockedSetBrowserTabViewport).toHaveBeenCalledWith(
      'session-a',
      'page-a',
      { width: 390, height: 700 },
      expect.any(String),
      true,
    )
  })

  it('shows the backend takeover cooldown to observers', async () => {
    mockedFetchBrowserState.mockResolvedValue({
      available: true,
      closed: false,
      active_page_id: 'page-a',
      tabs_revision: 0,
      tabs: [{ page_id: 'page-a', viewport: defaultViewport, status: 'ready', active: true }],
    })

    render(<BrowserLiveWindow sessionID="session-a" enabled defaultOpen />)
    await act(async () => {})
    await receiveFrameWithControl({ controller: false, can_takeover: false, retry_after_ms: 30_000, revision: 2 })
    expect(screen.getByRole('button', { name: '30 秒后可获取' })).toBeDisabled()
    await act(async () => {
      vi.advanceTimersByTime(1_000)
    })
    expect(screen.getByRole('button', { name: '29 秒后可获取' })).toBeDisabled()

    await act(async () => {
      sockets.at(-1)?.onmessage?.(
        new MessageEvent('message', {
          data: JSON.stringify({ type: 'browser_control', controller: false, can_takeover: true, revision: 3 }),
        }),
      )
    })
    expect(screen.getByRole('button', { name: '获取操作权' })).toBeEnabled()
  })

  it('caches the applied viewport per Tab and does not write it again when switching back', async () => {
    let resized: ResizeObserverCallback | undefined
    let activePageID = 'page-a'
    vi.stubGlobal(
      'ResizeObserver',
      class implements ResizeObserver {
        constructor(callback: ResizeObserverCallback) {
          resized = callback
        }
        observe() {}
        unobserve() {}
        disconnect() {}
      },
    )
    mockedActivateBrowserTab.mockImplementation(async (_sessionID, pageID) => {
      activePageID = pageID
    })
    mockedFetchBrowserState.mockImplementation(async () => ({
      available: true,
      closed: false,
      active_page_id: activePageID,
      tabs_revision: 0,
      tabs: [
        {
          page_id: 'page-a',
          title: 'A',
          url: 'https://example.com/page-a',
          viewport: defaultViewport,
          status: 'ready',
          active: activePageID === 'page-a',
        },
        {
          page_id: 'page-b',
          title: 'B',
          url: 'https://example.com/page-b',
          viewport: defaultViewport,
          status: 'ready',
          active: activePageID === 'page-b',
        },
      ],
    }))

    render(<BrowserLiveWindow sessionID="session-a" enabled defaultOpen resourcePanelOpen sidePanelOpen />)
    await act(async () => {})
    await receiveFrame()
    const canvas = screen.getByLabelText('浏览器页面当前画面')
    act(() => resized?.([{ contentRect: { width: 800, height: 600 } } as ResizeObserverEntry], {} as ResizeObserver))
    await act(async () => {
      vi.advanceTimersByTime(200)
      await Promise.resolve()
    })
    expect(mockedSetBrowserTabViewport).toHaveBeenCalledTimes(1)
    createBitmap.mockResolvedValue({ width: 800, height: 600, close: vi.fn() })
    await receiveFrame()

    await act(async () => {
      fireEvent.click(screen.getByRole('tab', { name: 'B' }))
      await Promise.resolve()
    })
    // page-b 没有本地帧，不能继续显示 page-a 的画面。
    expect(canvas).not.toHaveClass('browser-live-window-canvas-ready')
    await act(async () => {
      vi.advanceTimersByTime(200)
      await Promise.resolve()
    })
    expect(mockedSetBrowserTabViewport).toHaveBeenCalledTimes(2)
    expect(mockedSetBrowserTabViewport).toHaveBeenLastCalledWith(
      'session-a',
      'page-b',
      { width: 800, height: 600 },
      expect.any(String),
      true,
    )

    await act(async () => {
      fireEvent.click(screen.getByRole('tab', { name: 'A' }))
      await Promise.resolve()
    })
    expect(canvas).toHaveClass('browser-live-window-canvas-ready')
    await act(async () => {
      vi.advanceTimersByTime(200)
      await Promise.resolve()
    })
    expect(mockedSetBrowserTabViewport.mock.calls.map((call) => call.slice(0, 3))).toEqual([
      ['session-a', 'page-a', { width: 800, height: 600 }],
      ['session-a', 'page-b', { width: 800, height: 600 }],
    ])
  })

  it('serializes viewport writes and applies only the latest resize after an in-flight request', async () => {
    let resized: ResizeObserverCallback | undefined
    let finishFirst: (() => void) | undefined
    vi.stubGlobal(
      'ResizeObserver',
      class implements ResizeObserver {
        constructor(callback: ResizeObserverCallback) {
          resized = callback
        }
        observe() {}
        unobserve() {}
        disconnect() {}
      },
    )
    mockedSetBrowserTabViewport
      .mockImplementationOnce(
        () =>
          new Promise<void>((resolve) => {
            finishFirst = resolve
          }),
      )
      .mockResolvedValue()
    mockedFetchBrowserState.mockResolvedValue({
      available: true,
      closed: false,
      active_page_id: 'page-a',
      tabs_revision: 0,
      tabs: [{ page_id: 'page-a', viewport: { width: 1_920, height: 1_080 }, status: 'ready', active: true }],
    })

    render(<BrowserLiveWindow sessionID="session-a" enabled defaultOpen resourcePanelOpen sidePanelOpen />)
    await act(async () => {})
    await receiveFrame()
    act(() => resized?.([{ contentRect: { width: 800, height: 600 } } as ResizeObserverEntry], {} as ResizeObserver))
    await act(async () => {
      vi.advanceTimersByTime(200)
      await Promise.resolve()
    })
    expect(mockedSetBrowserTabViewport).toHaveBeenCalledTimes(1)

    act(() => resized?.([{ contentRect: { width: 830, height: 610 } } as ResizeObserverEntry], {} as ResizeObserver))
    await act(async () => {
      vi.advanceTimersByTime(200)
      await Promise.resolve()
    })
    expect(mockedSetBrowserTabViewport).toHaveBeenCalledTimes(1)
    await act(async () => {
      finishFirst?.()
      await Promise.resolve()
      await Promise.resolve()
    })
    expect(mockedSetBrowserTabViewport).toHaveBeenCalledTimes(2)
    expect(mockedSetBrowserTabViewport).toHaveBeenLastCalledWith(
      'session-a',
      'page-a',
      { width: 830, height: 610 },
      expect.any(String),
      undefined,
    )
  })

  it('maps mouse, wheel, and keyboard input into the remote viewport', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    render(<BrowserLiveWindow sessionID="session-a" enabled defaultOpen resourcePanelOpen sidePanelOpen />)
    await act(async () => {})
    await receiveFrame()

    const frame = screen.getByRole('application', { name: '可交互浏览器页面' })
    vi.spyOn(frame, 'getBoundingClientRect').mockReturnValue({
      left: 10,
      top: 20,
      width: 320,
      height: 300,
      right: 330,
      bottom: 320,
      x: 10,
      y: 20,
      toJSON: () => ({}),
    })
    const pointer = (type: string, clientX: number, clientY: number, buttons: number) => {
      const event = new Event(type, { bubbles: true, cancelable: true })
      Object.defineProperties(event, {
        button: { value: 0 },
        buttons: { value: buttons },
        pointerId: { value: 7 },
        pointerType: { value: 'mouse' },
        clientX: { value: clientX },
        clientY: { value: clientY },
        width: { value: 1 },
        height: { value: 1 },
        pressure: { value: buttons ? 0.5 : 0 },
        altKey: { value: false },
        ctrlKey: { value: false },
        metaKey: { value: false },
        shiftKey: { value: false },
      })
      return event
    }

    // 320x300 的容器中，16:9 页面实际位于 y=80..260；中心点仍应精确映射到远端中心。
    fireEvent(frame, pointer('pointerdown', 170, 170, 1))
    // 某些 WebView 在 pointer capture 后会把移动事件的 buttons 错报为 0；
    // 组件应沿用 pointerdown 的按键状态，保证滚动条拖动继续成立。
    fireEvent(frame, pointer('pointermove', 250, 215, 0))
    fireEvent(frame, pointer('pointerup', 250, 215, 0))
    fireEvent.wheel(frame, { clientX: 170, clientY: 170, deltaX: 2, deltaY: 10, deltaMode: 0 })
    fireEvent.keyDown(frame, { key: 'A', code: 'KeyA', keyCode: 65, shiftKey: true })
    fireEvent.keyUp(frame, { key: 'A', code: 'KeyA', keyCode: 65, shiftKey: true })

    const inputs = sentInputs()
    expect(JSON.parse(socketMessages[0]!).page_id).toBe('page-a')
    expect(inputs[0]).toMatchObject({ kind: 'mouse', action: 'down', x: 960, y: 540, button: 'left', buttons: 1 })
    expect(inputs[1]).toMatchObject({ kind: 'mouse', action: 'move', x: 1_440, y: 810, button: 'left', buttons: 1 })
    expect(inputs[2]).toMatchObject({ kind: 'mouse', action: 'up', x: 1_440, y: 810, buttons: 0 })
    expect(inputs[3]).toMatchObject({ kind: 'mouse', action: 'wheel', x: 960, y: 540, delta_x: 12, delta_y: 60 })
    expect(inputs[4]).toMatchObject({ kind: 'key', action: 'down', key: 'A', code: 'KeyA', text: 'A', modifiers: 8 })
    expect(inputs[5]).toMatchObject({ kind: 'key', action: 'up', key: 'A', code: 'KeyA', modifiers: 8 })
  })

  it('clears pressed input state when the live socket reconnects', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState({ viewport: { width: 800, height: 600 } }))
    render(<BrowserLiveWindow sessionID="session-a" enabled defaultOpen resourcePanelOpen sidePanelOpen />)
    await act(async () => {})
    await receiveFrame()

    const frame = screen.getByRole('application', { name: '可交互浏览器页面' })
    vi.spyOn(frame, 'getBoundingClientRect').mockReturnValue({
      left: 0,
      top: 0,
      width: 800,
      height: 600,
      right: 800,
      bottom: 600,
      x: 0,
      y: 0,
      toJSON: () => ({}),
    })
    const pointer = (type: string, buttons: number) => {
      const event = new Event(type, { bubbles: true, cancelable: true })
      Object.defineProperties(event, {
        button: { value: 0 },
        buttons: { value: buttons },
        pointerId: { value: 7 },
        pointerType: { value: 'mouse' },
        clientX: { value: 100 },
        clientY: { value: 100 },
        width: { value: 1 },
        height: { value: 1 },
        pressure: { value: 0 },
        altKey: { value: false },
        ctrlKey: { value: false },
        metaKey: { value: false },
        shiftKey: { value: false },
      })
      return event
    }
    fireEvent(frame, pointer('pointerdown', 1))

    await act(async () => {
      sockets.at(-1)?.onclose?.()
      vi.advanceTimersByTime(500)
      await Promise.resolve()
    })
    await receiveFrameWithControl({ controller: true, can_takeover: false, revision: 2 })
    act(() => {
      fireEvent(frame, pointer('pointermove', 0))
      vi.advanceTimersByTime(60)
    })

    expect(sentInputs().at(-1)).toMatchObject({ kind: 'mouse', action: 'move', buttons: 0 })
    expect(sentInputs().at(-1)).not.toHaveProperty('button')
  })

  it('does not let an older state response bind input back to a previous page', async () => {
    const tabs = (active: string): BrowserTabView[] => [
      {
        page_id: 'page-a',
        title: 'A',
        url: 'https://example.com/a',
        viewport: defaultViewport,
        status: 'ready',
        active: active === 'page-a',
      },
      {
        page_id: 'page-b',
        title: 'B',
        url: 'https://example.com/b',
        viewport: defaultViewport,
        status: 'ready',
        active: active === 'page-b',
      },
    ]
    mockedFetchBrowserState.mockResolvedValue({
      available: true,
      closed: false,
      active_page_id: 'page-a',
      tabs_revision: 1,
      tabs: tabs('page-a'),
    })
    render(<BrowserLiveWindow sessionID="session-a" enabled defaultOpen resourcePanelOpen sidePanelOpen />)
    await act(async () => {})
    await receiveFrame()

    await receiveState({
      available: true,
      closed: false,
      active_page_id: 'page-b',
      tabs_revision: 3,
      tabs: tabs('page-b'),
    })
    await receiveState({
      available: true,
      closed: false,
      active_page_id: 'page-a',
      tabs_revision: 2,
      tabs: tabs('page-a'),
    })

    expect(screen.getByRole('tab', { name: 'B' })).toHaveAttribute('aria-selected', 'true')
    fireEvent.keyDown(screen.getByRole('application', { name: '可交互浏览器页面' }), {
      key: 'b',
      code: 'KeyB',
      keyCode: 66,
    })
    expect(JSON.parse(socketMessages.at(-1)!).page_id).toBe('page-b')
  })

  it('transmits only semantic select-all/copy/paste shortcuts and drops modifier key state', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    render(<BrowserLiveWindow sessionID="session-a" enabled defaultOpen resourcePanelOpen sidePanelOpen />)
    await act(async () => {})
    await receiveFrame()

    const frame = screen.getByRole('application', { name: '可交互浏览器页面' })
    sockets.at(-1)!.bufferedAmount = 256 << 10
    fireEvent.keyDown(frame, { key: 'Control', code: 'ControlLeft', keyCode: 17, ctrlKey: true })
    fireEvent.keyDown(frame, { key: 'a', code: 'KeyA', keyCode: 65, ctrlKey: true })
    fireEvent.keyUp(frame, { key: 'a', code: 'KeyA', keyCode: 65, ctrlKey: false })
    fireEvent.keyUp(frame, { key: 'Control', code: 'ControlLeft', keyCode: 17, ctrlKey: false })
    fireEvent.keyDown(frame, { key: 'c', code: 'KeyC', keyCode: 67, metaKey: true })
    fireEvent.keyUp(frame, { key: 'c', code: 'KeyC', keyCode: 67, metaKey: false })
    fireEvent.keyDown(frame, { key: 'x', code: 'KeyX', keyCode: 88, ctrlKey: true })
    fireEvent.keyUp(frame, { key: 'x', code: 'KeyX', keyCode: 88, ctrlKey: false })
    fireEvent.keyDown(frame, { key: 'v', code: 'KeyV', keyCode: 86, ctrlKey: true })
    fireEvent.keyUp(frame, { key: 'v', code: 'KeyV', keyCode: 86, ctrlKey: false })

    expect(sentInputs()).toEqual([
      { kind: 'shortcut', action: 'select_all', modifiers: 2 },
      { kind: 'shortcut', action: 'copy', modifiers: 4 },
      { kind: 'shortcut', action: 'paste', modifiers: 2 },
    ])
  })

  it('forwards touch and Chinese IME composition through browser input commands', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    render(<BrowserLiveWindow sessionID="session-a" enabled defaultOpen resourcePanelOpen sidePanelOpen />)
    await act(async () => {})
    await receiveFrame()

    const frame = screen.getByRole('application', { name: '可交互浏览器页面' })
    vi.spyOn(frame, 'getBoundingClientRect').mockReturnValue({
      left: 0,
      top: 0,
      width: 320,
      height: 180,
      right: 320,
      bottom: 180,
      x: 0,
      y: 0,
      toJSON: () => ({}),
    })
    const touch = (clientX: number, clientY: number) => ({
      identifier: -11,
      clientX,
      clientY,
      radiusX: 4,
      radiusY: 5,
      force: 0.75,
    })
    const start = touch(80, 45)
    const move = touch(160, 90)
    fireEvent.touchStart(frame, { touches: [start], changedTouches: [start] })
    fireEvent.touchMove(frame, { touches: [move], changedTouches: [move] })
    fireEvent.touchEnd(frame, { touches: [], changedTouches: [move] })

    const textarea = screen.getByRole('textbox', { name: '浏览器直播键盘输入' })
    fireEvent.compositionStart(textarea)
    for (const data of ['z', 'zh', '中文']) fireEvent.compositionUpdate(textarea, { data })
    fireEvent.compositionEnd(textarea, { data: '中文' })

    const inputs = sentInputs()
    expect(inputs[0]).toMatchObject({
      kind: 'touch',
      action: 'start',
      touches: [{ id: 0, x: 480, y: 270, force: 0.75 }],
    })
    expect(inputs[1]).toMatchObject({ kind: 'touch', action: 'move', touches: [{ id: 0, x: 960, y: 540 }] })
    expect(inputs[2]).toEqual(expect.objectContaining({ kind: 'touch', action: 'end', touches: [] }))
    expect(inputs.slice(3)).toEqual([{ kind: 'text', text: '中文' }])
  })

  it('uses TouchEvent once when Android also emits a compatibility PointerEvent', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    render(<BrowserLiveWindow sessionID="session-a" enabled defaultOpen resourcePanelOpen sidePanelOpen />)
    await act(async () => {})
    await receiveFrame()

    const frame = screen.getByRole('application', { name: '可交互浏览器页面' })
    vi.spyOn(frame, 'getBoundingClientRect').mockReturnValue({
      left: 0,
      top: 0,
      width: 320,
      height: 180,
      right: 320,
      bottom: 180,
      x: 0,
      y: 0,
      toJSON: () => ({}),
    })
    const pointer = (type: string, buttons: number) => {
      const event = new Event(type, { bubbles: true, cancelable: true })
      Object.defineProperties(event, {
        button: { value: 0 },
        buttons: { value: buttons },
        pointerId: { value: 11 },
        pointerType: { value: 'touch' },
        clientX: { value: 160 },
        clientY: { value: 90 },
        width: { value: 8 },
        height: { value: 10 },
        pressure: { value: buttons ? 0.75 : 0 },
        altKey: { value: false },
        ctrlKey: { value: false },
        metaKey: { value: false },
        shiftKey: { value: false },
      })
      return event
    }
    const touch = { identifier: 23, clientX: 160, clientY: 90, radiusX: 4, radiusY: 5, force: 2 }

    fireEvent(frame, pointer('pointerdown', 1))
    expect(sentInputs()).toHaveLength(0)
    fireEvent.touchStart(frame, { touches: [touch], changedTouches: [touch] })
    fireEvent(frame, pointer('pointerup', 0))
    fireEvent.touchEnd(frame, { touches: [], changedTouches: [touch] })

    expect(sentInputs()[0]).toMatchObject({
      kind: 'touch',
      action: 'start',
      touches: [{ id: 0, x: 960, y: 540, force: 1 }],
    })
    expect(sentInputs()[1]).toEqual(expect.objectContaining({ kind: 'touch', action: 'end', touches: [] }))
  })

  it('limits touch moves to 30 Hz regardless of refresh rate and flushes the final drag position before end', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    render(<BrowserLiveWindow sessionID="session-a" enabled defaultOpen resourcePanelOpen sidePanelOpen />)
    await act(async () => {})
    await receiveFrame()

    const frame = screen.getByRole('application', { name: '可交互浏览器页面' })
    vi.spyOn(frame, 'getBoundingClientRect').mockReturnValue({
      left: 0,
      top: 0,
      width: 320,
      height: 180,
      right: 320,
      bottom: 180,
      x: 0,
      y: 0,
      toJSON: () => ({}),
    })
    const touch = (clientX: number, clientY: number) => ({
      identifier: 11,
      clientX,
      clientY,
      radiusX: 4,
      radiusY: 5,
      force: 0.75,
    })

    let current = touch(20, 20)
    fireEvent.touchStart(frame, { touches: [current], changedTouches: [current] })
    for (let index = 1; index <= 50; index += 1) {
      current = touch(index * 3, 90)
      fireEvent.touchMove(frame, { touches: [current], changedTouches: [current] })
    }
    // 首次移动即时响应，后续采样在固定窗口内合并。
    expect(sentInputs()).toHaveLength(2)

    act(() => vi.advanceTimersByTime(33))
    expect(sentInputs()).toHaveLength(2)
    act(() => vi.advanceTimersByTime(1))
    expect(sentInputs()).toHaveLength(3)
    expect(sentInputs()[2]).toMatchObject({ kind: 'touch', action: 'move', touches: [{ x: 900, y: 540 }] })

    for (let index = 1; index <= 20; index += 1) {
      current = touch(150 + index, 100)
      fireEvent.touchMove(frame, { touches: [current], changedTouches: [current] })
    }
    // 下一窗口中即使浏览器连续采样，也不能再立即发一个 move。
    expect(sentInputs()).toHaveLength(3)
    current = touch(190, 110)
    fireEvent.touchEnd(frame, { touches: [], changedTouches: [current] })
    const inputs = sentInputs()
    expect(inputs).toHaveLength(5)
    expect(inputs[3]).toMatchObject({ kind: 'touch', action: 'move', touches: [{ x: 1_140, y: 660 }] })
    expect(inputs[4]).toEqual(expect.objectContaining({ kind: 'touch', action: 'end', touches: [] }))
    act(() => vi.advanceTimersByTime(100))
    expect(sentInputs()).toHaveLength(5)
  })

  it('waits until the touch ends before resizing the remote viewport', async () => {
    let resized: ResizeObserverCallback | undefined
    vi.stubGlobal(
      'ResizeObserver',
      class implements ResizeObserver {
        constructor(callback: ResizeObserverCallback) {
          resized = callback
        }
        observe() {}
        unobserve() {}
        disconnect() {}
      },
    )
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    render(<BrowserView endpoint="/browser" />)
    await act(async () => {})
    await receiveFrame()
    const frame = screen.getByRole('application', { name: '可交互浏览器页面' })
    vi.spyOn(frame, 'getBoundingClientRect').mockReturnValue({
      left: 0,
      top: 0,
      width: 320,
      height: 180,
      right: 320,
      bottom: 180,
      x: 0,
      y: 0,
      toJSON: () => ({}),
    })
    const start = { identifier: 1, clientX: 20, clientY: 100 }
    const end = { ...start, clientX: 100 }
    fireEvent.touchStart(frame, { touches: [start], changedTouches: [start] })
    act(() => resized?.([{ contentRect: { width: 390, height: 700 } } as ResizeObserverEntry], {} as ResizeObserver))
    await act(async () => {
      await vi.advanceTimersByTimeAsync(600)
    })
    expect(mockedSetBrowserTabViewport).not.toHaveBeenCalled()
    fireEvent.touchMove(frame, { touches: [end], changedTouches: [end] })
    fireEvent.touchEnd(frame, { touches: [], changedTouches: [end] })
    expect(sentInputs().map((input) => input.action)).toEqual(['start', 'move', 'end'])
    await act(async () => {
      await vi.advanceTimersByTimeAsync(200)
    })
    expect(mockedSetBrowserTabViewport).toHaveBeenCalledExactlyOnceWith(
      '/browser',
      'page-a',
      { width: 390, height: 700 },
      expect.any(String),
      true,
    )
  })

  it('retains captured fingers outside the frame and ignores contacts that started elsewhere', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    render(<BrowserView endpoint="/browser" />)
    await act(async () => {})
    await receiveFrame()
    const frame = screen.getByRole('application', { name: '可交互浏览器页面' })
    vi.spyOn(frame, 'getBoundingClientRect').mockReturnValue({
      left: 0,
      top: 0,
      width: 320,
      height: 180,
      right: 320,
      bottom: 180,
      x: 0,
      y: 0,
      toJSON: () => ({}),
    })
    const one = { identifier: 1, clientX: 100, clientY: 100 }
    const outside = { identifier: 9, clientX: 110, clientY: 100 }
    const two = { identifier: 2, clientX: 200, clientY: 100 }
    fireEvent.touchStart(frame, { touches: [outside, one], changedTouches: [one] })
    expect(sentInputs()[0].touches).toHaveLength(1)
    const edge = { ...one, clientX: 400 }
    fireEvent.touchMove(frame, { touches: [outside, edge], changedTouches: [edge] })
    fireEvent.touchStart(frame, { touches: [outside, edge, two], changedTouches: [two] })
    expect(sentInputs().at(-1).touches).toHaveLength(2)
    fireEvent.touchEnd(frame, { touches: [outside, edge], changedTouches: [two] })
    expect(sentInputs().at(-1)).toMatchObject({ action: 'move', touches: [{ id: 0 }] })
    expect(sentInputs().at(-1).touches[0].x).toBeCloseTo(1919.999)
    fireEvent.touchEnd(frame, { touches: [outside], changedTouches: [edge] })
    expect(sentInputs().at(-1)).toMatchObject({ action: 'end', touches: [] })
    const count = sentInputs().length
    fireEvent.touchMove(frame, { touches: [outside], changedTouches: [outside] })
    fireEvent.touchEnd(frame, { touches: [], changedTouches: [outside] })
    expect(sentInputs()).toHaveLength(count)
  })

  it('preserves the latest congested touch move and reliably cancels without replaying it', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    render(<BrowserView endpoint="/browser" />)
    await act(async () => {})
    await receiveFrame()
    const frame = screen.getByRole('application', { name: '可交互浏览器页面' })
    vi.spyOn(frame, 'getBoundingClientRect').mockReturnValue({
      left: 0,
      top: 0,
      width: 320,
      height: 180,
      right: 320,
      bottom: 180,
      x: 0,
      y: 0,
      toJSON: () => ({}),
    })
    const touch = (clientX: number) => ({ identifier: 1, clientX, clientY: 100 })
    const start = touch(10)
    const event = fireEvent.touchStart(frame, { touches: [start], changedTouches: [start] })
    expect(event).toBe(false) // 原生监听成功 preventDefault。
    sockets.at(-1)!.bufferedAmount = 20_000
    for (let x = 20; x <= 30; x++) {
      const point = touch(x)
      fireEvent.touchMove(frame, { touches: [point], changedTouches: [point] })
      act(() => vi.advanceTimersByTime(8))
    }
    expect(sentInputs()).toHaveLength(1)
    fireEvent.touchCancel(frame, { touches: [], changedTouches: [touch(30)] })
    expect(sentInputs().map((input) => input.action)).toEqual(['start', 'move', 'cancel'])
    sockets.at(-1)!.bufferedAmount = 0
    act(() => vi.advanceTimersByTime(100))
    expect(sentInputs()).toHaveLength(3)
  })

  it('forwards real pointer samples without generating extra positions', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    render(<BrowserLiveWindow sessionID="session-a" enabled defaultOpen resourcePanelOpen sidePanelOpen />)
    await act(async () => {})
    await receiveFrame()

    const frame = screen.getByRole('application', { name: '可交互浏览器页面' })
    vi.spyOn(frame, 'getBoundingClientRect').mockReturnValue({
      left: 0,
      top: 0,
      width: 1_920,
      height: 1_080,
      right: 1_920,
      bottom: 1_080,
      x: 0,
      y: 0,
      toJSON: () => ({}),
    })
    const pointerMove = (clientX: number) => {
      const event = new Event('pointermove', { bubbles: true, cancelable: true })
      Object.defineProperties(event, {
        button: { value: 0 },
        buttons: { value: 0 },
        pointerId: { value: 7 },
        pointerType: { value: 'mouse' },
        clientX: { value: clientX },
        clientY: { value: 200 },
        altKey: { value: false },
        ctrlKey: { value: false },
        metaKey: { value: false },
        shiftKey: { value: false },
      })
      return event
    }

    fireEvent(frame, pointerMove(100))
    fireEvent(frame, pointerMove(101))
    fireEvent(frame, pointerMove(102))
    expect(sentInputs()).toEqual([
      expect.objectContaining({ kind: 'mouse', action: 'move', x: 100, y: 200 }),
      expect.objectContaining({ kind: 'mouse', action: 'move', x: 101, y: 200 }),
      expect.objectContaining({ kind: 'mouse', action: 'move', x: 102, y: 200 }),
    ])
  })

  it('coalesces browser pointer samples to the latest position', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    render(<BrowserLiveWindow sessionID="session-a" enabled defaultOpen resourcePanelOpen sidePanelOpen />)
    await act(async () => {})
    await receiveFrame()

    const frame = screen.getByRole('application', { name: '可交互浏览器页面' })
    vi.spyOn(frame, 'getBoundingClientRect').mockReturnValue({
      left: 0,
      top: 0,
      width: 1_920,
      height: 1_080,
      right: 1_920,
      bottom: 1_080,
      x: 0,
      y: 0,
      toJSON: () => ({}),
    })
    const sample = (clientX: number) => ({
      button: 0,
      buttons: 0,
      pointerId: 7,
      pointerType: 'mouse',
      clientX,
      clientY: 200,
      altKey: false,
      ctrlKey: false,
      metaKey: false,
      shiftKey: false,
    })
    const event = new Event('pointermove', { bubbles: true, cancelable: true })
    Object.defineProperties(event, {
      button: { value: 0 },
      buttons: { value: 0 },
      pointerId: { value: 7 },
      pointerType: { value: 'mouse' },
      clientX: { value: 103 },
      clientY: { value: 200 },
      altKey: { value: false },
      ctrlKey: { value: false },
      metaKey: { value: false },
      shiftKey: { value: false },
      getCoalescedEvents: { value: () => [sample(100), sample(101), sample(103)] },
    })

    fireEvent(frame, event)

    act(() => {
      vi.advanceTimersByTime(60)
    })
    expect(sentInputs()).toEqual([expect.objectContaining({ kind: 'mouse', action: 'move', x: 103, y: 200 })])
  })

  it('coalesces wheel deltas and keeps pointer order', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    render(<BrowserLiveWindow sessionID="session-a" enabled defaultOpen resourcePanelOpen sidePanelOpen />)
    await act(async () => {})
    await receiveFrame()

    const frame = screen.getByRole('application', { name: '可交互浏览器页面' })
    vi.spyOn(frame, 'getBoundingClientRect').mockReturnValue({
      left: 0,
      top: 0,
      width: 1_920,
      height: 1_080,
      right: 1_920,
      bottom: 1_080,
      x: 0,
      y: 0,
      toJSON: () => ({}),
    })
    const pointerMove = (clientX: number, clientY: number) => {
      const event = new Event('pointermove', { bubbles: true, cancelable: true })
      Object.defineProperties(event, {
        button: { value: 0 },
        buttons: { value: 0 },
        pointerId: { value: 7 },
        pointerType: { value: 'mouse' },
        clientX: { value: clientX },
        clientY: { value: clientY },
        altKey: { value: false },
        ctrlKey: { value: false },
        metaKey: { value: false },
        shiftKey: { value: false },
      })
      return event
    }

    fireEvent(frame, pointerMove(50, 60))
    expect(sentInputs()).toEqual([expect.objectContaining({ kind: 'mouse', action: 'move', x: 50, y: 60 })])
    fireEvent.wheel(frame, { clientX: 100, clientY: 200, deltaX: 2, deltaY: 10 })
    fireEvent.wheel(frame, { clientX: 110, clientY: 220, deltaX: 3, deltaY: 20 })
    fireEvent(frame, pointerMove(120, 230))
    expect(sentInputs()).toEqual([
      expect.objectContaining({ kind: 'mouse', action: 'move', x: 50, y: 60 }),
      expect.objectContaining({
        kind: 'mouse',
        action: 'wheel',
        x: 110,
        y: 220,
        delta_x: 5,
        delta_y: 30,
      }),
      expect.objectContaining({ kind: 'mouse', action: 'move', x: 120, y: 230 }),
    ])
  })

  it('coalesces congested HTTP pointer samples and delivers the final position', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    render(<BrowserLiveWindow sessionID="session-a" enabled defaultOpen resourcePanelOpen sidePanelOpen />)
    await act(async () => {})
    await receiveFrame()

    const frame = screen.getByRole('application', { name: '可交互浏览器页面' })
    vi.spyOn(frame, 'getBoundingClientRect').mockReturnValue({
      left: 0,
      top: 0,
      width: 1_920,
      height: 1_080,
      right: 1_920,
      bottom: 1_080,
      x: 0,
      y: 0,
      toJSON: () => ({}),
    })
    const pointerMove = (clientX: number) => {
      const event = new Event('pointermove', { bubbles: true, cancelable: true })
      Object.defineProperties(event, {
        button: { value: 0 },
        buttons: { value: 0 },
        pointerId: { value: 7 },
        pointerType: { value: 'mouse' },
        clientX: { value: clientX },
        clientY: { value: 200 },
        altKey: { value: false },
        ctrlKey: { value: false },
        metaKey: { value: false },
        shiftKey: { value: false },
      })
      return event
    }

    sockets.at(-1)!.bufferedAmount = 16 << 10
    fireEvent(frame, pointerMove(100))
    fireEvent(frame, pointerMove(101))
    fireEvent(frame, pointerMove(102))
    expect(sentInputs()).toEqual([])
    sockets.at(-1)!.bufferedAmount = 0
    await act(async () => { vi.advanceTimersByTime(32) })
    expect(sentInputs()).toEqual([
      expect.objectContaining({ kind: 'mouse', action: 'move', x: 102, y: 200 }),
    ])
  })

  it('does not interpolate sparse pointer samples in the browser frontend', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    render(<BrowserLiveWindow sessionID="session-a" enabled defaultOpen resourcePanelOpen sidePanelOpen />)
    await act(async () => {})
    await receiveFrame()

    const frame = screen.getByRole('application', { name: '可交互浏览器页面' })
    vi.spyOn(frame, 'getBoundingClientRect').mockReturnValue({
      left: 0,
      top: 0,
      width: 1_920,
      height: 1_080,
      right: 1_920,
      bottom: 1_080,
      x: 0,
      y: 0,
      toJSON: () => ({}),
    })
    const pointerMove = (clientX: number, timestamp: number) => {
      const event = new Event('pointermove', { bubbles: true, cancelable: true })
      Object.defineProperties(event, {
        button: { value: 0 },
        buttons: { value: 0 },
        pointerId: { value: 7 },
        pointerType: { value: 'mouse' },
        clientX: { value: clientX },
        clientY: { value: 200 },
        timeStamp: { value: timestamp },
        altKey: { value: false },
        ctrlKey: { value: false },
        metaKey: { value: false },
        shiftKey: { value: false },
      })
      return event
    }

    fireEvent(frame, pointerMove(100, 0))
    fireEvent(frame, pointerMove(200, 32))
    expect(sentInputs()).toEqual([
      expect.objectContaining({ kind: 'mouse', action: 'move', x: 100 }),
      expect.objectContaining({ kind: 'mouse', action: 'move', x: 200 }),
    ])
  })

  it('requires the mobile input badge to toggle the soft-keyboard input mode', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    render(<BrowserLiveWindow sessionID="session-a" enabled defaultOpen resourcePanelOpen sidePanelOpen />)
    await act(async () => {})
    await receiveFrame()

    const frame = screen.getByRole('application', { name: '可交互浏览器页面' })
    const textarea = screen.getByRole('textbox', { name: '浏览器直播键盘输入' })
    vi.spyOn(frame, 'getBoundingClientRect').mockReturnValue({
      left: 0,
      top: 0,
      width: 320,
      height: 180,
      right: 320,
      bottom: 180,
      x: 0,
      y: 0,
      toJSON: () => ({}),
    })
    const touch = new Event('pointerdown', { bubbles: true, cancelable: true })
    Object.defineProperties(touch, {
      button: { value: 0 },
      buttons: { value: 1 },
      pointerId: { value: 11 },
      pointerType: { value: 'touch' },
      clientX: { value: 80 },
      clientY: { value: 45 },
      width: { value: 8 },
      height: { value: 10 },
      pressure: { value: 0.75 },
      altKey: { value: false },
      ctrlKey: { value: false },
      metaKey: { value: false },
      shiftKey: { value: false },
    })

    fireEvent(frame, touch)
    expect(textarea).not.toHaveFocus()

    const openInput = screen.getByLabelText('开启页面输入')
    fireEvent.click(openInput)
    expect(textarea).toHaveFocus()
    expect(frame).toHaveClass('browser-live-window-frame-input-active')
    const closeInput = screen.getByLabelText('关闭页面输入')
    expect(closeInput).toHaveAttribute('aria-pressed', 'true')

    // 角标点击可能先触发 textarea blur，再触发 button click；失焦目标是
    // 角标时不能提前清掉输入模式，否则同一次点击会被重新打开。
    fireEvent.blur(textarea, { relatedTarget: closeInput })
    expect(closeInput).toHaveAttribute('aria-pressed', 'true')

    fireEvent.click(closeInput)
    expect(textarea).not.toHaveFocus()
    expect(frame).not.toHaveClass('browser-live-window-frame-input-active')
    expect(screen.getByLabelText('开启页面输入')).toHaveAttribute('aria-pressed', 'false')
  })

  it('returns to the non-input mode when the IME or another control blurs the hidden input', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    render(<BrowserLiveWindow sessionID="session-a" enabled defaultOpen resourcePanelOpen sidePanelOpen />)
    await act(async () => {})
    await receiveFrame()

    const textarea = screen.getByRole('textbox', { name: '浏览器直播键盘输入' })
    fireEvent.click(screen.getByLabelText('开启页面输入'))
    expect(textarea).toHaveFocus()
    expect(screen.getByLabelText('关闭页面输入')).toBeInTheDocument()

    // 系统输入法的“完成/对号”通常使原生输入框失焦，relatedTarget 为空。
    fireEvent.blur(textarea)
    expect(screen.getByLabelText('开启页面输入')).toHaveAttribute('aria-pressed', 'false')

    fireEvent.click(screen.getByLabelText('开启页面输入'))
    expect(textarea).toHaveFocus()
    const outside = document.createElement('button')
    document.body.appendChild(outside)
    fireEvent.blur(textarea, { relatedTarget: outside })
    expect(screen.getByLabelText('开启页面输入')).toHaveAttribute('aria-pressed', 'false')
    outside.remove()
  })

  it('does not send intermediate IME composition updates during rapid Chinese input', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    render(<BrowserLiveWindow sessionID="session-a" enabled defaultOpen resourcePanelOpen sidePanelOpen />)
    await act(async () => {})
    await receiveFrame()

    const textarea = screen.getByRole('textbox', { name: '浏览器直播键盘输入' })
    sockets.at(-1)!.bufferedAmount = 256 << 10
    fireEvent.compositionStart(textarea)
    for (let index = 0; index < 200; index += 1) {
      fireEvent.compositionUpdate(textarea, { data: `拼音${index}` })
    }
    expect(socketMessages).toHaveLength(0)

    fireEvent.compositionEnd(textarea, { data: '拼音完成' })
    expect(sentInputs()).toEqual([{ kind: 'text', text: '拼音完成' }])

    fireEvent.compositionStart(textarea)
    fireEvent.compositionUpdate(textarea, { data: '未完成' })
    fireEvent.compositionEnd(textarea, { data: '' })
    expect(sentInputs()).toEqual([{ kind: 'text', text: '拼音完成' }])
  })
  it('supports multiple inline instances with separate viewer identities', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    const { container } = render(
      <>
        <BrowserView endpoint="/one" />
        <BrowserView endpoint="/two" />
      </>,
    )
    await act(async () => {
      await Promise.resolve()
      await Promise.resolve()
    })
    expect(container.querySelectorAll('[data-part="root"]')).toHaveLength(2)
    const one = mockedBrowserStreamURL.mock.calls.find((call) => call[0] === '/one')
    const two = mockedBrowserStreamURL.mock.calls.find((call) => call[0] === '/two')
    expect(one).toBeDefined()
    expect(two).toBeDefined()
    expect(one?.[1]).not.toBe(two?.[1])
  })

  it('toggles its own diagnostics with F12 while focused', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    render(<BrowserView endpoint="/browser" devtools />)
    await act(async () => {
      await Promise.resolve()
    })
    expect(screen.getByLabelText('切换开发诊断 F12')).toHaveAttribute('aria-pressed', 'false')
    fireEvent.keyDown(screen.getByRole('region', { name: '浏览器页面直播' }), { key: 'F12' })
    expect(screen.getByLabelText('切换开发诊断 F12')).toHaveAttribute('aria-pressed', 'true')
    // 桌面布局隐藏移动端 Tab，直接验证实际展开的诊断面板。
    expect(screen.getByRole('region', { name: '开发诊断' })).toBeVisible()
  })

  it('joins a read-only instance as an observer and keeps custom container styles', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    const { container } = render(
      <BrowserView
        endpoint="/browser"
        readOnly
        className="customer"
        style={{ height: 430 }}
        classNames={{ toolbar: 'customer-toolbar' }}
      />,
    )
    await act(async () => {
      await Promise.resolve()
      await Promise.resolve()
    })
    expect(mockedBrowserStreamURL).toHaveBeenCalledWith('/browser', expect.any(String), true)
    expect(container.querySelector('.customer')).toHaveStyle({ height: '430px' })
    expect(container.querySelector('[data-part="toolbar"]')).toHaveClass('customer-toolbar')
  })

  it('keeps close available while busy and notifies the host before cleanup completes', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    let finish: (() => void) | undefined
    mockedCloseBrowser.mockImplementation(
      () =>
        new Promise<void>((resolve) => {
          finish = resolve
        }),
    )
    const onClose = vi.fn()
    render(<BrowserView endpoint="/browser" busy onClose={onClose} />)
    await act(async () => {})
    await receiveFrame()
    expect(screen.getByRole('textbox', { name: '浏览器页面地址' })).toBeDisabled()
    expect(document.querySelector('.browser-live-panel-body')).toHaveAttribute('inert')
    const close = screen.getByRole('button', { name: '关闭浏览器' })
    expect(close).toBeEnabled()
    fireEvent.click(close)
    expect(onClose).toHaveBeenCalledOnce()
    expect(mockedCloseBrowser).toHaveBeenCalledWith('/browser', expect.any(String))
    expect(close).toBeDisabled()
    await act(async () => {
      finish?.()
    })
  })

  it('keeps the Agent overlay and input lock for 300ms after busy ends', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    const props = { endpoint: '/browser', viewerID: 'viewer-agent-overlay' }
    const { rerender } = render(<BrowserView {...props} busy />)
    await act(async () => {
      await Promise.resolve()
      await Promise.resolve()
    })
    await receiveFrame()
    rerender(<BrowserView {...props} busy={false} />)
    expect(screen.getByText('Agent 正在操作浏览器…')).toBeInTheDocument()
    expect(screen.getByRole('textbox', { name: '浏览器页面地址' })).toBeDisabled()
    await act(async () => {
      vi.advanceTimersByTime(299)
    })
    expect(screen.getByText('Agent 正在操作浏览器…')).toBeInTheDocument()
    await act(async () => {
      vi.advanceTimersByTime(1)
    })
    expect(screen.queryByText('Agent 正在操作浏览器…')).not.toBeInTheDocument()
    expect(screen.getByRole('textbox', { name: '浏览器页面地址' })).toBeEnabled()

    rerender(<BrowserView {...props} busy />)
    rerender(<BrowserView {...props} busy={false} />)
    await act(async () => {
      vi.advanceTimersByTime(150)
    })
    rerender(<BrowserView {...props} busy />)
    await act(async () => {
      vi.advanceTimersByTime(300)
    })
    expect(screen.getByText('Agent 正在操作浏览器…')).toBeInTheDocument()
  })

  it('导航返回前已绘制最终帧时不重新显示加载遮罩', async () => {
    const target = 'https://example.com/?query=kept#fragment'
    mockedFetchBrowserState
      .mockResolvedValueOnce(singleTabState({ url: 'about:blank' }))
      .mockResolvedValue(singleTabState({ url: target }))
    let finishNavigation!: () => void
    vi.mocked(navigateBrowserTab).mockImplementationOnce(
      () =>
        new Promise<void>((resolve) => {
          finishNavigation = resolve
        }),
    )
    render(<BrowserView endpoint="/browser" viewerID="viewer-navigation" />)
    await act(async () => {
      await Promise.resolve()
      await Promise.resolve()
    })
    await receiveFrame()
    const input = screen.getByRole('textbox', { name: '浏览器页面地址' })
    fireEvent.change(input, { target: { value: target } })
    fireEvent.click(screen.getByRole('button', { name: '前往页面' }))
    await receiveFrame()
    await act(async () => {
      finishNavigation()
      await Promise.resolve()
      await Promise.resolve()
    })
    expect(input).toHaveValue(target)
    expect(screen.queryByText('页面加载中…')).not.toBeInTheDocument()
  })

  it('导航失败时保留画面并结束加载等待', async () => {
    mockedFetchBrowserState.mockResolvedValue(singleTabState())
    vi.mocked(navigateBrowserTab).mockRejectedValueOnce(new Error('navigation failed'))
    render(<BrowserView endpoint="/browser" viewerID="viewer-navigation-error" />)
    await act(async () => {
      await Promise.resolve()
      await Promise.resolve()
    })
    await receiveFrame()
    fireEvent.change(screen.getByRole('textbox', { name: '浏览器页面地址' }), {
      target: { value: 'https://example.com/unavailable' },
    })
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: '前往页面' }))
      await Promise.resolve()
      await Promise.resolve()
    })
    expect(screen.getByText('前往页面失败：navigation failed')).toBeInTheDocument()
    expect(screen.queryByText('页面加载中…')).not.toBeInTheDocument()
  })
})
