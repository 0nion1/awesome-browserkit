import {
  activateBrowserTab,
  browserDevtoolsStreamURL,
  browserStateStreamURL,
  browserStreamURL,
  closeBrowser,
  closeBrowserTab,
  decodeFrame,
  fetchBrowserAssets,
  fetchBrowserDevtoolsState,
  fetchBrowserState,
  fetchCachedBrowserFrame,
  historyBrowserTab,
  navigateBrowserTab,
  newBrowserTab,
  reloadBrowserTab,
  setBrowserTabViewport,
  startBrowserDevtools,
  stopBrowserDevtools,
  takeBrowserControl,
  startBrowserSequenceRecording,
  finishBrowserSequenceRecording,
  type BrowserAsset,
  type BrowserControlState,
  type BrowserDevtoolsState,
  type BrowserFrame,
  type BrowserViewState,
} from './client'

/** 组件所需的输入连接。实现负责排队、背压及断线释放。 */
export interface BrowserInputConnection {
  readonly readyState: number
  readonly bufferedAmount: number
  send(value: string): void
  /** 丢弃尚未发送的连续采样，操作权或页面变化时调用。 */
  discardPendingInputs?(): void
  close(): void
}

/** 页面元素选择结果。bounds 使用页面视口 CSS 像素。 */
export interface BrowserInspectedElement {
  ref: string
  revision: number
  role: string
  name?: string
  url: string
  bounds: { x: number; y: number; width: number; height: number }
}

/** 浏览器历史方向。 */
export type BrowserHistoryDirection = 'back' | 'forward'

/** 浏览器组件的数据入口。宿主可使用任意传输实现这些操作。 */
export interface BrowserSource {
  state(sessionID: string): Promise<BrowserViewState>
  cachedFrame(sessionID: string): Promise<Blob>
  subscribeState(sessionID: string, onState: (state: BrowserViewState) => void, onClose: () => void): () => void
  connect(
    sessionID: string,
    viewerID: string,
    readOnly: boolean,
    handlers: {
      frame: (frame: BrowserFrame) => void
      control: (state: BrowserControlState) => void
      error: (message: string) => void
      close: () => void
    },
  ): BrowserInputConnection
  /** 激活 Tab；返回服务端已经算好的状态，省去一次 state 往返。 */
  activate(sessionID: string, pageID: string, viewerID: string): Promise<BrowserViewState | void>
  inspectPoint?(
    sessionID: string,
    pageID: string,
    x: number,
    y: number,
    viewerID: string,
  ): Promise<BrowserInspectedElement>
  navigate(sessionID: string, pageID: string, url: string, viewerID: string): Promise<BrowserViewState | void>
  reload(sessionID: string, pageID: string, viewerID: string): Promise<BrowserViewState | void>
  history?(
    sessionID: string,
    pageID: string,
    direction: BrowserHistoryDirection,
    viewerID: string,
  ): Promise<BrowserViewState | void>
  newTab(sessionID: string, viewerID: string): Promise<void>
  closeTab(sessionID: string, pageID: string, viewerID: string): Promise<void>
  viewport(
    sessionID: string,
    pageID: string,
    size: { width: number; height: number },
    viewerID: string,
    force?: boolean,
  ): Promise<BrowserViewState | void>
  takeControl(sessionID: string, viewerID: string): Promise<BrowserControlState>
  closeBrowser(sessionID: string, viewerID: string): Promise<void>
  startRecording?(sessionID: string, pageID: string, viewerID: string): Promise<void>
  finishRecording?(sessionID: string, outputPath: string, viewerID: string): Promise<{ path: string }>
  diagnostics: BrowserDiagnosticsSource
}

/** 开发诊断的数据入口，与画面共享同一个组件源对象。 */
export interface BrowserDiagnosticsSource {
  read(sessionID: string, pageID: string, after: number, limit: number): Promise<BrowserDevtoolsState>
  subscribe(sessionID: string, pageID: string, onState: (state: BrowserDevtoolsState) => void): () => void
  assets(sessionID: string, pageID: string, signal?: AbortSignal): Promise<BrowserAsset[]>
  start(sessionID: string, pageID: string, viewerID?: string): Promise<BrowserDevtoolsState>
  stop(sessionID: string, pageID: string, viewerID?: string): Promise<BrowserDevtoolsState>
}

/** BrowserKit HTTP/WebSocket 协议的默认数据源。 */
export const httpBrowserSource: BrowserSource = {
  state: (sessionID) => fetchBrowserState(sessionID),
  cachedFrame: (sessionID) => fetchCachedBrowserFrame(sessionID),
  subscribeState(sessionID, onState, onClose) {
    const socket = new WebSocket(browserStateStreamURL(sessionID))
    socket.onmessage = (event) => {
      if (typeof event.data !== 'string') return
      try {
        onState(JSON.parse(event.data) as BrowserViewState)
      } catch {
        /* 下一条状态可恢复。 */
      }
    }
    socket.onclose = onClose
    socket.onerror = () => socket.close()
    return () => socket.close()
  },
  connect(sessionID, viewerID, readOnly, handlers) {
    const socket = new WebSocket(browserStreamURL(sessionID, viewerID, readOnly))
    socket.binaryType = 'blob'
    // HTTP 数据源也要吸收链路拥塞；只合并连续 mouse move，离散输入前
    // 先发送最后一个目标位置，保证 down/up/滚轮/按键的顺序。
    let pendingMove: { pageID: string; data: string } | undefined
    let moveTimer: ReturnType<typeof setTimeout> | undefined
    const discardPendingInputs = () => {
      clearTimeout(moveTimer)
      moveTimer = undefined
      pendingMove = undefined
    }
    const flushMove = () => {
      moveTimer = undefined
      if (!pendingMove) return
      if (socket.readyState !== 1) {
        discardPendingInputs()
        return
      }
      if (socket.bufferedAmount >= 16 << 10) {
        moveTimer = setTimeout(flushMove, 16)
        return
      }
      const data = pendingMove.data
      discardPendingInputs()
      socket.send(data)
    }
    // 实时画面只解码最新帧；读取或解码慢时直接跳过已经过期的帧。
    let pendingFrame: Blob | undefined
    let decoding = false
    const decodeLatest = async () => {
      if (decoding) return
      decoding = true
      try {
        while (pendingFrame) {
          const blob = pendingFrame
          pendingFrame = undefined
          try {
            handlers.frame(await decodeFrame(blob))
          } catch {
            /* 下一帧会继续覆盖。 */
          }
        }
      } finally {
        decoding = false
        if (pendingFrame) void decodeLatest()
      }
    }
    socket.onmessage = (event) => {
      if (typeof event.data === 'string') {
        try {
          const message = JSON.parse(event.data)
          if (message.type === 'browser_input_error') handlers.error(message.error.message)
          else {
            if (message.controller === false) discardPendingInputs()
            handlers.control(message as BrowserControlState)
          }
        } catch {
          /* 下一条控制消息可恢复。 */
        }
      } else if (event.data instanceof Blob) {
        pendingFrame = event.data
        void decodeLatest()
      }
    }
    socket.onclose = () => {
      discardPendingInputs()
      pendingFrame = undefined
      handlers.close()
    }
    socket.onerror = () => socket.close()
    return {
      get readyState() {
        return socket.readyState
      },
      get bufferedAmount() {
        return socket.bufferedAmount
      },
      send(value) {
        const input = JSON.parse(value) as { kind: string; action?: string; page_id: string }
        if (pendingMove && pendingMove.pageID !== input.page_id) discardPendingInputs()
        if (input.kind === 'mouse' && input.action === 'move' && socket.bufferedAmount >= 16 << 10) {
          pendingMove = { pageID: input.page_id, data: value }
          if (moveTimer === undefined) moveTimer = setTimeout(flushMove, 16)
          return
        }
        if (pendingMove) {
          const data = pendingMove.data
          discardPendingInputs()
          socket.send(data)
        }
        socket.send(value)
      },
      discardPendingInputs,
      close() {
        discardPendingInputs()
        pendingFrame = undefined
        socket.close()
      },
    }
  },
  activate: (sessionID, pageID, viewerID) => activateBrowserTab(sessionID, pageID, viewerID),
  inspectPoint: async (sessionID, pageID, x, y, viewerID) => {
    const response = await fetch(`${sessionID.replace(/\/$/, '')}/tabs/${encodeURIComponent(pageID)}/inspect`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'X-Browserkit-Viewer': viewerID },
      body: JSON.stringify({ x, y }),
      credentials: 'include',
    })
    if (!response.ok) throw new Error((await response.json().catch(() => ({}))).error || '元素选择失败')
    return response.json() as Promise<BrowserInspectedElement>
  },
  navigate: (sessionID, pageID, url, viewerID) => navigateBrowserTab(sessionID, pageID, url, viewerID),
  reload: (sessionID, pageID, viewerID) => reloadBrowserTab(sessionID, pageID, viewerID),
  history: (sessionID, pageID, direction, viewerID) => historyBrowserTab(sessionID, pageID, direction, viewerID),
  newTab: (sessionID, viewerID) => newBrowserTab(sessionID, viewerID),
  closeTab: (sessionID, pageID, viewerID) => closeBrowserTab(sessionID, pageID, viewerID),
  viewport: (sessionID, pageID, size, viewerID, force) =>
    setBrowserTabViewport(sessionID, pageID, size, viewerID, force),
  takeControl: (sessionID, viewerID) => takeBrowserControl(sessionID, viewerID),
  closeBrowser: (sessionID, viewerID) => closeBrowser(sessionID, viewerID),
  startRecording: (sessionID, pageID, viewerID) => startBrowserSequenceRecording(sessionID, pageID, viewerID),
  finishRecording: (sessionID, outputPath, viewerID) => finishBrowserSequenceRecording(sessionID, outputPath, viewerID),
  diagnostics: {
    read: (sessionID, pageID, after, limit) => fetchBrowserDevtoolsState(sessionID, pageID, after, limit),
    subscribe(sessionID, pageID, onState) {
      const socket = new WebSocket(browserDevtoolsStreamURL(sessionID, pageID))
      socket.onmessage = (event) => {
        if (typeof event.data !== 'string') return
        try {
          onState(JSON.parse(event.data) as BrowserDevtoolsState)
        } catch {
          /* 下一条诊断快照可恢复。 */
        }
      }
      socket.onerror = () => socket.close()
      return () => socket.close()
    },
    assets: async (sessionID, pageID, signal) => (await fetchBrowserAssets(sessionID, pageID, '', 500, signal)).assets,
    start: (sessionID, pageID, viewerID) => startBrowserDevtools(sessionID, pageID, viewerID),
    stop: (sessionID, pageID, viewerID) => stopBrowserDevtools(sessionID, pageID, viewerID),
  },
}
