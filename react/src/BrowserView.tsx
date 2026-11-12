/* eslint-disable react-hooks/exhaustive-deps -- 保持上游画面连接与输入生命周期；这里的 effect 使用 ref 取实时状态，不能因每次 render 生成的处理器重建连接。 */
import {
  Plus,
  Code2,
  ArrowLeft,
  ArrowRight,
  CornerDownLeft,
  Keyboard,
  RefreshCw,
  X,
  MousePointer2,
  Circle,
  Square,
} from 'lucide-react'
import type { CSSProperties } from 'react'
import { useEffect, useRef, useState } from 'react'
import { createReconnect } from './reconnect'
import { type BrowserControlState, type BrowserTabView, type BrowserViewState } from './client'
import {
  httpBrowserSource,
  type BrowserInputConnection,
  type BrowserSource,
  type BrowserInspectedElement,
} from './source'
import { BrowserDevtoolsPanel } from './BrowserDevtoolsPanel'
import type { BrowserControls, BrowserInputOptions, BrowserDevtoolsOptions } from './options'
import { createBrowserInputHandlers, type BrowserInput, type PendingMove, type RemotePoint } from './browserViewInput'

type DevtoolsResize = { pointerID: number }
type BrowserViewport = { width: number; height: number }
type ViewportTarget = BrowserViewport & {
  sessionID: string
  pageID: string
  generation: number
  attempt: number
  force: boolean
}
const viewportResizeDebounceMS = 200
const viewportResizeRetryMS = 500
const agentBusyOverlayCloseDelayMS = 300
const inspectSampleIntervalMS = 100

const sameViewport = (left: BrowserViewport | undefined, right: BrowserViewport) =>
  left?.width === right.width && left.height === right.height

// 直播/操作连接在切换 Tab 后会短暂重建；元素检查在有限时间内重试，
// 不把重连窗口直接呈现成用户错误。
const transientInspectionError = (message: string) =>
  message.includes('浏览器操作连接尚未就绪') ||
  message.includes('浏览器操作连接尚未建立') ||
  message.includes('浏览器操作连接已关闭')

// ResizeObserver 的 contentRect 就是页面画布的 CSS content box：Tab 栏和 URL 栏
// 是它的兄弟节点，因此无需再猜测或减去任何栏位高度。CDP 只接受整数 CSS 像素。
const viewportFromContentRect = (rect: Pick<DOMRectReadOnly, 'width' | 'height'>): BrowserViewport | undefined => {
  const width = Math.round(rect.width)
  const height = Math.round(rect.height)
  return width > 0 && height > 0 ? { width, height } : undefined
}

const createBrowserViewerID = () => {
  return globalThis.crypto?.randomUUID?.() ?? `viewer-${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`
}

/** 浏览器组件参数。 */
export interface BrowserViewProps {
  endpoint: string
  /** 数据与操作入口；省略时使用 BrowserKit HTTP/WebSocket 协议。 */
  source?: BrowserSource
  /** 使用宿主标签时指定当前页面。 */
  pageID?: string
  className?: string
  style?: CSSProperties
  classNames?: Partial<Record<'toolbar' | 'tabs' | 'tab' | 'viewport' | 'devtools', string>>
  readOnly?: boolean
  /** 暂停页面交互，但保留控制方的关闭入口。 */
  busy?: boolean
  /** 用户请求关闭时立即通知宿主，不等待浏览器回收。 */
  onClose?: () => void
  /** 区域与按钮显示开关；false 隐藏全部内置控制区域。 */
  controls?: false | BrowserControls
  /** 页面输入开关；false 停止所有页面输入，仍可使用导航和 Tab。 */
  input?: false | BrowserInputOptions
  devtools?: boolean | BrowserDevtoolsOptions
  liveView?: boolean
  /** 为 false 时固定显示 pageID，服务端活动页变化不会切走该页面。 */
  followActivePage?: boolean
  viewport?: 'container' | { width: number; height: number }
  viewerID?: string
  onStateChange?: (state: BrowserViewState) => void
  /** 选择完成后由宿主将元素和批注放入聊天草稿。 */
  onElementSelect?: (value: { pageID: string; element: BrowserInspectedElement; annotation: string }) => void
  onAreaSelect?: (value: {
    pageID: string
    url: string
    bounds: { x: number; y: number; width: number; height: number }
    viewport: BrowserViewport
    image: Blob
    annotation: string
  }) => void
  onError?: (error: Error) => void
}

export function BrowserView(props: BrowserViewProps) {
  return <BrowserSurface key={props.endpoint} {...props} />
}

function BrowserSurface({
  endpoint: sessionID,
  source = httpBrowserSource,
  pageID: selectedPageID,
  className = '',
  style,
  classNames,
  readOnly = false,
  busy = false,
  onClose,
  controls,
  input,
  devtools = false,
  liveView: liveViewEnabled = true,
  followActivePage = true,
  viewport: configuredViewport = 'container',
  viewerID: suppliedViewerID,
  onStateChange,
  onElementSelect,
  onAreaSelect,
  onError,
}: BrowserViewProps) {
  const visible = (name: keyof BrowserControls) => controls !== false && controls?.[name] !== false
  const mouseEnabled = input !== false && input?.mouse !== false
  const touchEnabled = input !== false && input?.touch !== false
  const wheelEnabled = input !== false && input?.wheel !== false
  const keyboardEnabled = input !== false && input?.keyboard !== false
  const clipboardEnabled = input !== false && input?.clipboard !== false
  const manualKeyboard = input !== false && input?.keyboardActivation === 'manual'
  const devtoolsOptions = typeof devtools === 'object' ? devtools : undefined
  const devtoolsEnabled = Boolean(devtools) && devtoolsOptions?.panels?.length !== 0
  const [generatedViewerID] = useState(createBrowserViewerID)
  const viewerID = suppliedViewerID || generatedViewerID
  const stateCallback = useRef(onStateChange)
  stateCallback.current = onStateChange

  const [pageURL, setPageURL] = useState<string | undefined>(undefined)
  const [addressValue, setAddressValue] = useState('')
  const [viewport, setViewport] = useState<BrowserViewport | undefined>(undefined)
  const [tabs, setTabs] = useState<BrowserTabView[]>([])
  const [activePageID, setActivePageID] = useState<string | undefined>(undefined)
  const [hasFrame, setHasFrame] = useState(false)
  const [waitingForFrame, setWaitingForFrame] = useState(false)
  const [busyOverlayVisible, setBusyOverlayVisible] = useState(busy)
  const interactionBusy = busy || busyOverlayVisible
  const [syncing, setSyncing] = useState(false)
  const [closing, setClosing] = useState(false)
  const closingRef = useRef(false)
  const [notice, setNotice] = useState<string>()
  const [recording, setRecording] = useState(false)
  const [recordedSteps, setRecordedSteps] = useState(0)
  const [recordingDialog, setRecordingDialog] = useState(false)
  const [recordingSaveMode, setRecordingSaveMode] = useState<'temporary' | 'path'>('temporary')
  const [recordingPath, setRecordingPath] = useState('')
  const [recordingPending, setRecordingPending] = useState(false)
  const [inspecting, setInspecting] = useState(false)
  const [inspected, setInspected] = useState<BrowserInspectedElement>()
  const [hovered, setHovered] = useState<BrowserInspectedElement>()
  const [area, setArea] = useState<{ x: number; y: number; width: number; height: number; image: Blob }>()
  const [dragArea, setDragArea] = useState<{ x: number; y: number; width: number; height: number }>()
  const dragStart = useRef<{ x: number; y: number } | undefined>(undefined)
  const ignoreInspectClick = useRef(false)
  const [annotation, setAnnotation] = useState('')
  const inspectSequence = useRef(0)
  const inspectSelectionPending = useRef(false)
  const inspectTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  const inspectSample = useRef<(() => void) | undefined>(undefined)
  const inspectLastSample = useRef(-Infinity)
  const inspectHoverPending = useRef(false)
  useEffect(
    () => () => {
      clearTimeout(inspectTimer.current)
      inspectSample.current = undefined
      inspectSequence.current++
    },
    [],
  )
  const inspectingRef = useRef(false)
  inspectingRef.current = inspecting
  const selectionLockedRef = useRef(false)
  selectionLockedRef.current = Boolean(inspected || area || inspectSelectionPending.current)
  const errorCallback = useRef(onError)
  errorCallback.current = onError
  useEffect(() => {
    if (notice) errorCallback.current?.(new Error(notice))
  }, [notice])
  const [panelHost, setPanelHost] = useState<HTMLElement | null>(null)
  const [sideViewportSize, setSideViewportSize] = useState<BrowserViewport>()
  const [mobilePanel, setMobilePanel] = useState<'page' | 'devtools'>(
    devtoolsEnabled && (devtoolsOptions?.open ?? devtoolsOptions?.defaultOpen) ? 'devtools' : 'page',
  )
  const [mobileInputActive, setMobileInputActive] = useState(false)
  const [devtoolsHeight, setDevtoolsHeight] = useState<string>()
  const [internalDevtoolsOpen, setInternalDevtoolsOpen] = useState(devtoolsOptions?.defaultOpen ?? false)
  const devtoolsPanelOpen = devtoolsEnabled && (devtoolsOptions?.open ?? internalDevtoolsOpen)
  const setDevtoolsPanelOpen = (open: boolean) => {
    if (devtoolsOptions?.open === undefined) setInternalDevtoolsOpen(open)
    devtoolsOptions?.onOpenChange?.(open)
  }
  useEffect(() => {
    setMobilePanel(devtoolsPanelOpen ? 'devtools' : 'page')
  }, [devtoolsPanelOpen])
  const [browserControl, setBrowserControl] = useState<BrowserControlState>()
  const [controlRetryAfterMS, setControlRetryAfterMS] = useState(0)
  const [takingControl, setTakingControl] = useState(false)
  const seenRef = useRef(false)
  const activePageRef = useRef<string | undefined>(undefined)
  const stateRevisionRef = useRef(0)
  // 连接代次：重连会重置 revision，旧连接发出的状态查询不能在新代次里落地。
  const stateEpochRef = useRef(0)
  const tabSelectionRef = useRef(0)
  // 已发出但尚未被服务端确认的 Tab 切换；期间到达的旧活动页状态不能把界面切回去。
  const pendingTabRef = useRef<string | undefined>(undefined)
  // 服务端当前活动页；隐藏标签不跟随它更新画面，但恢复可见时据此重新激活宿主选择的 Tab。
  const serverActivePageRef = useRef<string | undefined>(undefined)
  // 恢复可见后的第一次状态同步需要重新应用宿主的页面选择；普通活动页变化仍跟随服务端。
  const resumeSelectionRef = useRef(false)
  const liveViewRef = useRef(liveViewEnabled)
  liveViewRef.current = liveViewEnabled
  // 状态订阅不应因宿主选择变化而重建；闭包通过 ref 读取最新选择。
  const selectedPageRef = useRef(selectedPageID)
  selectedPageRef.current = selectedPageID
  const addressEditingRef = useRef(false)
  // 浏览器 Tab 切换期间先复用该 Tab 的本地最后一帧，避免等待 HTTP/WS 往返。
  // Blob 本身不可变；Tab 关闭时从 Map 删除即可释放引用。
  const frameGenerations = useRef(new Map<string, number>())
  const frameCacheRef = useRef(new Map<string, Blob>())
  const framePageRef = useRef<string | undefined>(undefined)
  const frameSequenceRef = useRef(0)
  // 直播帧只保留最新一帧等待解码；解码中的旧帧结束后直接处理最新帧，
  // 避免高帧率下多个 createImageBitmap 同时竞争主线程。
  const pendingStreamFrameRef = useRef<{ blob: Blob; pageID: string; sequence: number } | undefined>(undefined)
  const streamFramePumpRef = useRef(false)
  const devtoolsResizeRef = useRef<DevtoolsResize | undefined>(undefined)
  const devtoolsResizeFrame = useRef<number | undefined>(undefined)
  const pendingDevtoolsHeight = useRef<string | undefined>(undefined)
  const socketRef = useRef<BrowserInputConnection | undefined>(undefined)
  const frameRef = useRef<HTMLDivElement | null>(null)
  const canvasRef = useRef<HTMLCanvasElement | null>(null)
  const inputRef = useRef<HTMLTextAreaElement | null>(null)
  const touchesRef = useRef(
    new Map<number, RemotePoint & { id: number; radiusX: number; radiusY: number; force: number }>(),
  )
  const nextTouchIDRef = useRef(0)
  const pendingTouchMoveRef = useRef<PendingMove | undefined>(undefined)
  const touchMoveTimerRef = useRef<number | undefined>(undefined)
  const pendingWheelRef = useRef<PendingMove | undefined>(undefined)
  const mouseMoveFrameRef = useRef<number | undefined>(undefined)
  const mobileInputToggleRef = useRef(false)
  const composingRef = useRef(false)
  const suppressInputRef = useRef(false)
  const shortcutKeysRef = useRef(new Set<string>())
  const suppressedKeyRefs = useRef(new Set<string>())
  const clickRef = useRef<{ button: number; x: number; y: number; at: number; count: number } | undefined>(undefined)
  const heldKeysRef = useRef(new Map<string, { pageID: string; input: BrowserInput }>())
  const heldMouseRef = useRef(new Map<string, { pageID: string; input: BrowserInput }>())
  const mouseButtonsRef = useRef(new Map<number, number>())
  // CDP 需要在 mouseMoved 中同时带上上一次按下的 button。仅传 buttons
  // 位掩码在普通页面上通常可用，但 Chromium 的原生滚动条拖动会因此
  // 把移动识别成未按键移动，导致滚动条只高亮、不跟随指针。
  const mouseButtonRef = useRef(new Map<number, number>())
  const expectedFrameRef = useRef<(BrowserViewport & { pageID: string }) | undefined>(undefined)
  const sideViewportSizeRef = useRef<BrowserViewport | undefined>(undefined)
  const viewportGenerationRef = useRef(0)
  const viewportPendingRef = useRef<ViewportTarget | undefined>(undefined)
  const viewportInFlightRef = useRef(false)
  const viewportAppliedRef = useRef(new Map<string, BrowserViewport>())
  // 最近一次已排入视口同步的活动页。新页面激活时需要强制下发，因为
  // Manager 记录的继承尺寸可能与浏览器实际页面不一致。
  const viewportPageRef = useRef<string | undefined>(undefined)
  const viewportDebounceTimerRef = useRef<number | undefined>(undefined)
  const viewportRetryTimerRef = useRef<number | undefined>(undefined)
  const flushViewportRef = useRef<() => void>(() => undefined)
  const correctViewportRef = useRef<(pageID: string) => void>(() => undefined)
  const browserControllerRef = useRef(false)
  const busyRef = useRef(busy)
  busyRef.current = busy
  const readOnlyRef = useRef(readOnly)
  readOnlyRef.current = readOnly || interactionBusy || closing
  const browserControlRevisionRef = useRef(0)
  sideViewportSizeRef.current = sideViewportSize
  browserControllerRef.current = !readOnlyRef.current && browserControl?.controller === true

  useEffect(() => {
    if (!visible('toolbar') || !visible('addressBar')) addressEditingRef.current = false
    if (!addressEditingRef.current) setAddressValue(pageURL || '')
  }, [pageURL, controls === false ? false : controls?.toolbar, controls === false ? false : controls?.addressBar])

  const clearKeyboardState = () => {
    heldKeysRef.current.clear()
    shortcutKeysRef.current.clear()
    suppressedKeyRefs.current.clear()
    composingRef.current = false
    suppressInputRef.current = false
  }

  const cancelPendingTouchMove = () => {
    if (touchMoveTimerRef.current !== undefined) window.clearTimeout(touchMoveTimerRef.current)
    touchMoveTimerRef.current = undefined
    pendingTouchMoveRef.current = undefined
  }

  const cancelPendingMouseMove = () => {
    if (mouseMoveFrameRef.current !== undefined) cancelAnimationFrame(mouseMoveFrameRef.current)
    mouseMoveFrameRef.current = undefined
    pendingWheelRef.current = undefined
  }

  const cancelPendingMoves = () => {
    socketRef.current?.discardPendingInputs?.()
    cancelPendingTouchMove()
    cancelPendingMouseMove()
  }

  // 操作权丢失和直播连接断开具有相同的输入边界：两者都必须取消尚未
  // 提交的 viewport/触摸操作，并清除本地按键状态，避免重连后续接旧手势。
  const loseBrowserControl = () => {
    browserControllerRef.current = false
    viewportPageRef.current = undefined
    viewportGenerationRef.current += 1
    viewportPendingRef.current = undefined
    expectedFrameRef.current = undefined
    if (viewportDebounceTimerRef.current !== undefined) window.clearTimeout(viewportDebounceTimerRef.current)
    if (viewportRetryTimerRef.current !== undefined) window.clearTimeout(viewportRetryTimerRef.current)
    viewportDebounceTimerRef.current = undefined
    viewportRetryTimerRef.current = undefined
    cancelPendingMoves()
    touchesRef.current.clear()
    heldMouseRef.current.clear()
    mouseButtonsRef.current.clear()
    mouseButtonRef.current.clear()
    clickRef.current = undefined
    inputRef.current?.blur()
    setMobileInputActive(false)
    clearKeyboardState()
  }

  useEffect(() => {
    if (busy) loseBrowserControl()
  }, [busy])

  useEffect(() => {
    if (busy) {
      setBusyOverlayVisible(true)
      return
    }
    if (!busyOverlayVisible) return
    const timer = window.setTimeout(() => setBusyOverlayVisible(false), agentBusyOverlayCloseDelayMS)
    return () => window.clearTimeout(timer)
  }, [busy, busyOverlayVisible])

  const applyBrowserControl = (state: BrowserControlState) => {
    if (state.type !== 'browser_control') return
    if (state.revision < browserControlRevisionRef.current) return
    browserControlRevisionRef.current = state.revision
    const wasController = browserControllerRef.current
    browserControllerRef.current = !readOnlyRef.current && state.controller
    setBrowserControl(state)
    setControlRetryAfterMS(Math.max(0, state.retry_after_ms || 0))
    if (wasController && !state.controller) loseBrowserControl()
  }

  useEffect(() => {
    if (controlRetryAfterMS <= 0) return
    const startedAt = Date.now()
    const initial = controlRetryAfterMS
    const timer = window.setInterval(() => {
      const remaining = Math.max(0, initial - (Date.now() - startedAt))
      setControlRetryAfterMS(remaining)
      if (remaining === 0) window.clearInterval(timer)
    }, 250)
    return () => window.clearInterval(timer)
  }, [browserControl?.revision])

  // HTTP 初始读取、独立状态 WebSocket 和用户操作后的读取可能乱序到达。
  // tabs_revision 只在 Manager 的可见 Tab 投影变化时递增；旧状态不能
  // 把输入重新绑定到已经离开的 page_id，也不能回滚 Tab 的加载状态。
  const acceptState = (state: BrowserViewState) => {
    const revision = state.tabs_revision || 0
    if (revision && revision < stateRevisionRef.current) return false
    if (revision) stateRevisionRef.current = revision
    return true
  }

  const clearFrame = () => {
    cancelPendingMoves()
    touchesRef.current.clear()
    frameSequenceRef.current += 1
    pendingStreamFrameRef.current = undefined
    framePageRef.current = undefined
    setHasFrame(false)
    setWaitingForFrame(true)
  }

  const drawFrame = async (blob: Blob, pageID = activePageRef.current, streamed = true): Promise<boolean> => {
    if (closingRef.current) return false
    const knownPage = (blob as Blob & { pageID?: string }).pageID
    if (knownPage && pageID !== knownPage) return false
    const sequence = ++frameSequenceRef.current
    try {
      const bitmap = await createImageBitmap(blob)
      if (pageID && activePageRef.current && pageID !== activePageRef.current) {
        bitmap.close()
        return false
      }
      if (pageID) frameCacheRef.current.set(pageID, blob)
      if (sequence !== frameSequenceRef.current) {
        bitmap.close()
        return false
      }
      const canvas = canvasRef.current
      const context = canvas?.getContext('2d')
      if (!canvas || !context) {
        bitmap.close()
        return false
      }
      if (canvas.width !== bitmap.width) canvas.width = bitmap.width
      if (canvas.height !== bitmap.height) canvas.height = bitmap.height
      context.drawImage(bitmap, 0, 0)
      const actualViewport = { width: bitmap.width, height: bitmap.height }
      setViewport((current) =>
        current?.width === actualViewport.width && current.height === actualViewport.height ? current : actualViewport,
      )
      framePageRef.current = pageID
      const expected = expectedFrameRef.current
      const desired = sideViewportSizeRef.current
      if (browserControllerRef.current && pageID && activePageRef.current === pageID && desired) {
        if (bitmap.width === desired.width && bitmap.height === desired.height) {
          if (sessionID) viewportAppliedRef.current.set(pageID, desired)
          const pending = viewportPendingRef.current
          if (pending?.pageID === pageID && sameViewport(pending, desired)) {
            viewportPendingRef.current = undefined
            if (viewportDebounceTimerRef.current !== undefined) window.clearTimeout(viewportDebounceTimerRef.current)
            viewportDebounceTimerRef.current = undefined
          }
          if (expected?.pageID === pageID) expectedFrameRef.current = undefined
          setViewport((current) =>
            current?.width === desired.width && current.height === desired.height ? current : desired,
          )
        } else {
          // 旧尺寸帧仍然有显示价值；保留并绘制它，同时在独立防抖窗口后
          // 重新校正远端 viewport。后续错误帧不会不断推迟这个计时器。
          correctViewportRef.current(pageID)
        }
      } else if (
        expected &&
        expected.pageID === pageID &&
        bitmap.width === expected.width &&
        bitmap.height === expected.height
      ) {
        expectedFrameRef.current = undefined
      }
      bitmap.close()
      setHasFrame(true)
      if (streamed) setWaitingForFrame(false)
      return true
    } catch {
      // 页面切换期间可能收到已经失效或不完整的帧，下一帧会继续覆盖。
      return false
    }
  }

  // 直播帧按“最新帧优先”解码：同时最多一个 createImageBitmap，
  // 等待期间到达的帧只保留最后一帧。缓存帧和首帧仍直接绘制。
  const pumpStreamFrames = async () => {
    if (streamFramePumpRef.current) return
    streamFramePumpRef.current = true
    try {
      while (pendingStreamFrameRef.current) {
        const next = pendingStreamFrameRef.current
        pendingStreamFrameRef.current = undefined
        if (next.sequence !== frameSequenceRef.current) continue
        await drawFrame(next.blob, next.pageID, true)
      }
    } finally {
      streamFramePumpRef.current = false
      if (pendingStreamFrameRef.current) void pumpStreamFrames()
    }
  }

  const drawStreamFrame = (blob: Blob, pageID: string) => {
    pendingStreamFrameRef.current = { blob, pageID, sequence: frameSequenceRef.current }
    void pumpStreamFrames()
  }

  const flushViewport = () => {
    if (viewportInFlightRef.current) return
    const target = viewportPendingRef.current
    if (!target) return
    if (!target.force && sameViewport(viewportAppliedRef.current.get(target.pageID), target)) {
      viewportPendingRef.current = undefined
      return
    }
    // 首帧校正、横竖屏或软键盘改变容器尺寸时，等手指抬起再调整远端布局；
    // 否则清空触点和切换坐标系会截断正在进行的滚动或拖动。
    if (touchesRef.current.size > 0) {
      if (viewportDebounceTimerRef.current === undefined)
        viewportDebounceTimerRef.current = window.setTimeout(() => {
          viewportDebounceTimerRef.current = undefined
          flushViewportRef.current()
        }, viewportResizeDebounceMS)
      return
    }
    viewportPendingRef.current = undefined
    viewportInFlightRef.current = true
    if (target.generation === viewportGenerationRef.current && activePageRef.current === target.pageID) {
      expectedFrameRef.current = { width: target.width, height: target.height, pageID: target.pageID }
      touchesRef.current.clear()
      clickRef.current = undefined
      cancelPendingMouseMove()
      clearKeyboardState()
    }
    const request = target.force
      ? source.viewport(target.sessionID, target.pageID, { width: target.width, height: target.height }, viewerID, true)
      : source.viewport(target.sessionID, target.pageID, { width: target.width, height: target.height }, viewerID)
    void request
      .then((state) => {
        if (target.generation !== viewportGenerationRef.current) return
        viewportAppliedRef.current.set(target.pageID, { width: target.width, height: target.height })
        if (activePageRef.current !== target.pageID) return
        // 视口响应携带同一份最新投影；直接采用，避免旧 live 状态短暂回滚尺寸。
        if (state?.tabs) setTabs(state.tabs)
        else
          setTabs((current) =>
            current.map((tab) =>
              tab.page_id === target.pageID
                ? { ...tab, viewport: { width: target.width, height: target.height } }
                : tab,
            ),
          )
        setViewport({ width: target.width, height: target.height })
        setNotice(undefined)
      })
      .catch((error) => {
        const stillCurrent =
          target.generation === viewportGenerationRef.current && activePageRef.current === target.pageID
        const message = error instanceof Error ? error.message : String(error)
        if (
          stillCurrent &&
          !message.includes('没有浏览器操作权') &&
          target.attempt < 2 &&
          !viewportPendingRef.current &&
          target.attempt === 0
        ) {
          viewportPendingRef.current = { ...target, attempt: target.attempt + 1 }
          viewportRetryTimerRef.current = window.setTimeout(() => {
            viewportRetryTimerRef.current = undefined
            flushViewportRef.current()
          }, viewportResizeRetryMS)
          return
        }
        if (stillCurrent && !message.includes('没有浏览器操作权')) {
          expectedFrameRef.current = undefined
          setNotice(error instanceof Error ? `调整浏览器视口失败：${error.message}` : '调整浏览器视口失败')
        }
      })
      .finally(() => {
        viewportInFlightRef.current = false
        if (
          viewportPendingRef.current &&
          viewportDebounceTimerRef.current === undefined &&
          viewportRetryTimerRef.current === undefined
        )
          flushViewportRef.current()
      })
  }
  flushViewportRef.current = flushViewport

  const scheduleViewport = (pageID: string, size: BrowserViewport, force: boolean, restartDebounce: boolean) => {
    // 视口写入会改变共享页面布局，必须由当前控制方执行，避免多个观看者互相干扰。
    if (!browserControllerRef.current || !sessionID || activePageRef.current !== pageID) return
    if (force) viewportAppliedRef.current.delete(pageID)
    viewportPendingRef.current = {
      sessionID,
      pageID,
      width: size.width,
      height: size.height,
      generation: viewportGenerationRef.current,
      attempt: 0,
      force,
    }
    if (viewportRetryTimerRef.current !== undefined) {
      window.clearTimeout(viewportRetryTimerRef.current)
      viewportRetryTimerRef.current = undefined
    }
    if (restartDebounce && viewportDebounceTimerRef.current !== undefined) {
      window.clearTimeout(viewportDebounceTimerRef.current)
      viewportDebounceTimerRef.current = undefined
    }
    if (viewportDebounceTimerRef.current === undefined) {
      viewportDebounceTimerRef.current = window.setTimeout(() => {
        viewportDebounceTimerRef.current = undefined
        flushViewportRef.current()
      }, viewportResizeDebounceMS)
    }
  }
  correctViewportRef.current = (pageID) => {
    const size = sideViewportSizeRef.current
    if (size) scheduleViewport(pageID, size, true, false)
  }

  useEffect(() => {
    viewportGenerationRef.current += 1
    viewportPendingRef.current = undefined
    expectedFrameRef.current = undefined
    setSideViewportSize(undefined)
    if (viewportDebounceTimerRef.current !== undefined) window.clearTimeout(viewportDebounceTimerRef.current)
    if (viewportRetryTimerRef.current !== undefined) window.clearTimeout(viewportRetryTimerRef.current)
    viewportDebounceTimerRef.current = undefined
    viewportRetryTimerRef.current = undefined
  }, [sessionID])

  useEffect(() => {
    viewportAppliedRef.current.clear()
    viewportPageRef.current = undefined
    browserControllerRef.current = false
    browserControlRevisionRef.current = 0
    setBrowserControl(undefined)
    setControlRetryAfterMS(0)
    setTakingControl(false)
  }, [sessionID])

  useEffect(() => {
    if (!liveViewEnabled || !panelHost || typeof ResizeObserver === 'undefined') return
    const frame = frameRef.current
    if (!frame) return
    const observer = new ResizeObserver((entries) => {
      const rect = entries[0]?.contentRect
      if (!rect) return
      const size = configuredViewport === 'container' ? viewportFromContentRect(rect) : configuredViewport
      if (!size) return
      setSideViewportSize((current) => (sameViewport(current, size) ? current : size))
    })
    observer.observe(frame)
    return () => observer.disconnect()
  }, [liveViewEnabled, panelHost])

  useEffect(() => {
    if (
      readOnly ||
      interactionBusy ||
      closing ||
      !browserControl?.controller ||
      !liveViewEnabled ||
      !sessionID ||
      !activePageID ||
      !sideViewportSize
    )
      return
    const force = viewportPageRef.current !== activePageID
    viewportPageRef.current = activePageID
    scheduleViewport(activePageID, sideViewportSize, force, true)
  }, [
    activePageID,
    browserControl?.controller,
    liveViewEnabled,
    sessionID,
    sideViewportSize,
    readOnly,
    interactionBusy,
    closing,
  ])

  useEffect(() => {
    // 浮窗、侧栏和最小化状态会重建 canvas；旧 backing store 不能复用。
    clearFrame()
  }, [])

  useEffect(() => {
    inputRef.current?.blur()
    setMobileInputActive(false)
    cancelPendingMoves()
    touchesRef.current.clear()
  }, [activePageID, liveViewEnabled, mobilePanel, sessionID])

  useEffect(() => {
    setPageURL(undefined)
    setAddressValue('')
    addressEditingRef.current = false
    setViewport(undefined)
    setTabs([])
    setActivePageID(undefined)
    frameCacheRef.current.clear()
    seenRef.current = false
    activePageRef.current = undefined
    stateRevisionRef.current = 0
    tabSelectionRef.current += 1
    pendingTabRef.current = undefined
    serverActivePageRef.current = undefined
    resumeSelectionRef.current = false
    appliedSelectionRef.current = undefined

    setSyncing(false)
    setWaitingForFrame(false)

    clearKeyboardState()
    heldMouseRef.current.clear()
    mouseButtonsRef.current.clear()
    mouseButtonRef.current.clear()
    clearFrame()
  }, [sessionID])

  useEffect(() => {
    if (!sessionID) {
      seenRef.current = false

      setSyncing(false)
      setWaitingForFrame(false)
      setPageURL(undefined)
      setAddressValue('')
      addressEditingRef.current = false
      setViewport(undefined)
      activePageRef.current = undefined
      stateRevisionRef.current = 0
      tabSelectionRef.current += 1
      pendingTabRef.current = undefined
      serverActivePageRef.current = undefined
      resumeSelectionRef.current = false
      appliedSelectionRef.current = undefined
      setTabs([])
      setActivePageID(undefined)
      frameCacheRef.current.clear()

      clearKeyboardState()
      heldMouseRef.current.clear()
      mouseButtonsRef.current.clear()
      mouseButtonRef.current.clear()
      clearFrame()
      return
    }
    let active = true
    const applyState = (state: BrowserViewState) => {
      if (!active) return
      if (!acceptState(state)) return
      if (state.active_page_id) serverActivePageRef.current = state.active_page_id
      // 隐藏期间不要清除恢复标记；否则重新可见时无法重新应用宿主的页面选择。
      if (liveViewEnabled && resumeSelectionRef.current && state.active_page_id === selectedPageRef.current)
        resumeSelectionRef.current = false
      // 缓存清理不能等可见性判断；隐藏标签也要及时释放已经关闭的 Tab 画面。
      if (Array.isArray(state.tabs)) {
        if (state.tabs.length === 0) {
          frameCacheRef.current.clear()
          viewportAppliedRef.current.clear()
          frameGenerations.current.clear()
        } else {
          const pages = new Set(state.tabs.map((tab) => tab.page_id))
          for (const pageID of frameCacheRef.current.keys())
            if (!pages.has(pageID)) frameCacheRef.current.delete(pageID)
          for (const pageID of viewportAppliedRef.current.keys())
            if (!pages.has(pageID)) viewportAppliedRef.current.delete(pageID)
          for (const pageID of frameGenerations.current.keys())
            if (!pages.has(pageID)) frameGenerations.current.delete(pageID)
        }
      }
      // 乐观切换期间，旧活动页的状态可能先到达；它不能覆盖用户刚点选的 Tab，
      // 但仍要更新标题、Tab 列表等与活动页无关的信息，不能整帧丢弃。
      const staleSelection = Boolean(
        pendingTabRef.current && state.active_page_id && state.active_page_id !== pendingTabRef.current,
      )
      if (!staleSelection) pendingTabRef.current = undefined
      stateCallback.current?.(state)
      if (staleSelection) {
        if (Array.isArray(state.tabs)) setTabs(state.tabs)
        return
      }
      // 隐藏标签只更新标题等状态回调，不跟随服务端活动页覆盖自己的最后一帧。
      if (!liveViewEnabled) return
      if (state.recording !== undefined) setRecording(state.recording)
      if (state.recorded_steps !== undefined) setRecordedSteps(state.recorded_steps)
      if (state.closed) {
        seenRef.current = false

        setSyncing(false)
        setWaitingForFrame(false)

        setPageURL(undefined)
        setAddressValue('')
        addressEditingRef.current = false
        setViewport(undefined)
        activePageRef.current = undefined
        stateRevisionRef.current = 0
        tabSelectionRef.current += 1
        pendingTabRef.current = undefined
        serverActivePageRef.current = undefined
        setTabs([])
        setActivePageID(undefined)
        frameCacheRef.current.clear()
        viewportAppliedRef.current.clear()

        clearKeyboardState()
        heldMouseRef.current.clear()
        mouseButtonsRef.current.clear()
        mouseButtonRef.current.clear()
        clearFrame()

        return
      }

      setSyncing(!state.available && seenRef.current)
      const nextTabs = state.tabs ?? []
      // 宿主标签与浏览器页面一一对应时固定自己的页面；Agent 在别的页面操作
      // 不应把当前标签的显示内容切走。
      const followsServerPage =
        followActivePage || !selectedPageRef.current || state.active_page_id === selectedPageRef.current
      const activeChanged =
        followsServerPage && activePageRef.current !== undefined && activePageRef.current !== state.active_page_id
      if (activeChanged && state.active_page_id) setWaitingForFrame(true)
      if (Array.isArray(state.tabs) && nextTabs.length === 0) {
        // 服务端明确返回 tabs: [] 即表示所有 Tab 已回收；清空画布和
        // 所有本地帧缓存，不依赖 available 状态。
        frameCacheRef.current.clear()
        viewportAppliedRef.current.clear()
        activePageRef.current = undefined
        setActivePageID(undefined)
        setTabs([])
        setPageURL(undefined)
        setViewport(undefined)
        clearFrame()
        setWaitingForFrame(false)
      }
      if (state.available) {
        const serverPageID = state.active_page_id
        // 服务端仍在别的 Tab 时，先保留宿主选择的本地画面，等激活完成；
        // 否则隐藏标签切回来会先闪一下上一个活动页。
        const hostSelectionDiffers = Boolean(
          resumeSelectionRef.current &&
          liveViewEnabled &&
          selectedPageRef.current &&
          serverPageID &&
          serverPageID !== selectedPageRef.current,
        )
        if (!hostSelectionDiffers && followsServerPage) {
          if (activePageRef.current !== undefined && activePageRef.current !== state.active_page_id && sessionID) {
            // 先显示 Manager 按 Tab 保存的最后帧；新 screencast 首帧到达后再覆盖，避免黑屏。
            void source
              .cachedFrame(sessionID)
              .then((cached) => drawFrame(cached, state.active_page_id, false))
              .catch(() => undefined)
          }
          activePageRef.current = state.active_page_id
        }
        seenRef.current = true

        // 新宿主标签可能在控制权状态之后才收到自己的 Tab 列表；如果服务端
        // 仍停在旧页面，必须在这里补一次激活，不能等用户切走再切回。
        const selectedPage = selectedPageRef.current
        if (
          !followsServerPage &&
          liveViewEnabled &&
          !busyRef.current &&
          browserControllerRef.current &&
          selectedPage &&
          state.tabs?.some((tab) => tab.page_id === selectedPage) &&
          pendingTabRef.current !== selectedPage
        ) {
          pendingTabRef.current = selectedPage
          void selectTab(selectedPage, true)
        }

        setTabs(nextTabs)
        if (!hostSelectionDiffers && followsServerPage) {
          setPageURL(state.url)
          setActivePageID(state.active_page_id)
          if (state.viewport?.width && state.viewport?.height) {
            const expected = expectedFrameRef.current
            if (
              !expected ||
              expected.pageID !== state.active_page_id ||
              (expected.width === state.viewport.width && expected.height === state.viewport.height)
            )
              setViewport(state.viewport)
          }
        } else if (hostSelectionDiffers && selectedPageRef.current) {
          pendingTabRef.current = selectedPageRef.current
          void selectTab(selectedPageRef.current, true)
        }
      }
    }
    const refresh = () => {
      const epoch = stateEpochRef.current
      void source
        .state(sessionID)
        .then((state) => {
          if (epoch === stateEpochRef.current) applyState(state)
        })
        .catch(() => {
          // 状态查询失败可能只是 CDP 在切换 target；保留已经打开的浮窗和最后一帧。
          if (active && epoch === stateEpochRef.current) setSyncing(seenRef.current)
        })
    }
    let unsubscribe: (() => void) | undefined
    const connect = () => {
      if (!active) return
      // 状态流也会在 Manager 重建后从头计数，新连接不再沿用旧 revision。
      stateEpochRef.current += 1
      stateRevisionRef.current = 0
      // 插件源的状态订阅是本地监听器；重连前先撤销，避免重复注册。
      unsubscribe?.()
      unsubscribe = source.subscribeState(
        sessionID,
        (state) => {
          if (!active) return
          // HTTP 快照只能更新界面；收到状态流数据才表示订阅恢复。
          reconnect.connected()
          applyState(state)
        },
        () => {
          if (!active) return
          // 旧连接结束时新 Manager 可能已经重置版本，立即刷新也要接受新 revision。
          stateEpochRef.current += 1
          stateRevisionRef.current = 0
          refresh()
          reconnect.retry()
        },
      )
      // 订阅建立（代次递增）后再查询，结果才属于当前连接。
      refresh()
    }
    const reconnect = createReconnect(connect)
    connect()
    return () => {
      active = false
      reconnect.close()
      unsubscribe?.()
    }
  }, [devtoolsEnabled, liveViewEnabled, sessionID])

  useEffect(() => {
    // 隐藏标签只保留状态订阅；直播和输入连接必须关闭，避免多个标签同时抢占
    // 操作权和重复解码同一路画面。
    if (!liveViewEnabled || !sessionID || !panelHost) return
    let active = true
    let connection: BrowserInputConnection | undefined
    const connect = () => {
      if (!active) return
      // 新连接可能来自重建后的 Manager，控制版本和画面世代都会从头开始；
      // 跨连接比较旧版本会永久忽略新的操作权状态或新画面。
      browserControlRevisionRef.current = 0
      browserControllerRef.current = false
      viewportPageRef.current = undefined
      frameGenerations.current.clear()
      connection = source.connect(sessionID, viewerID, readOnly, {
        frame: (frame) => {
          if (!active || frame.pageID !== activePageRef.current) return
          if (frame.generation < (frameGenerations.current.get(frame.pageID) ?? 0)) return
          reconnect.connected()
          setNotice((value) => (value?.startsWith('连接中断，') ? undefined : value))
          frameGenerations.current.set(frame.pageID, frame.generation)
          drawStreamFrame(frame.blob, frame.pageID)
        },
        control: (state) => {
          reconnect.connected()
          setNotice((value) => (value?.startsWith('连接中断，') ? undefined : value))
          applyBrowserControl(state)
        },
        error: setNotice,
        close: () => {
          if (socketRef.current === connection) socketRef.current = undefined
          if (active) {
            loseBrowserControl()
            setBrowserControl(undefined)
            reconnect.retry()
          }
        },
      })
      socketRef.current = connection
    }
    const reconnect = createReconnect(connect, (delay) =>
      setNotice(`连接中断，${Math.ceil(delay / 1000)} 秒后自动重连…`),
    )
    connect()
    return () => {
      active = false
      reconnect.close()
      if (socketRef.current === connection) socketRef.current = undefined
      connection?.close()
    }
  }, [liveViewEnabled, panelHost, sessionID, viewerID, readOnly])

  useEffect(
    () => () => {
      frameSequenceRef.current += 1
      pendingStreamFrameRef.current = undefined
      if (devtoolsResizeFrame.current !== undefined) cancelAnimationFrame(devtoolsResizeFrame.current)
      cancelPendingMoves()
    },
    [],
  )

  useEffect(() => {
    if (!notice) return
    const timer = window.setTimeout(() => setNotice(undefined), 3_500)
    return () => window.clearTimeout(timer)
  }, [notice])

  const startDevtoolsResize = (event: React.PointerEvent<HTMLDivElement>) => {
    if ((panelHost?.clientWidth ?? 0) <= 520) return
    devtoolsResizeRef.current = { pointerID: event.pointerId }
    event.currentTarget.setPointerCapture?.(event.pointerId)
    event.preventDefault()
  }

  const moveDevtoolsResize = (event: React.PointerEvent<HTMLDivElement>) => {
    if (devtoolsResizeRef.current?.pointerID !== event.pointerId) return
    const body = event.currentTarget.parentElement
    if (!body) return
    const rect = body.getBoundingClientRect()
    const height = Math.min(Math.max(190, rect.bottom - event.clientY), Math.max(190, rect.height - 150 - 8))
    pendingDevtoolsHeight.current = `${Math.round(height)}px`
    if (devtoolsResizeFrame.current === undefined) {
      devtoolsResizeFrame.current = requestAnimationFrame(() => {
        devtoolsResizeFrame.current = undefined
        const next = pendingDevtoolsHeight.current
        pendingDevtoolsHeight.current = undefined
        if (next) setDevtoolsHeight(next)
      })
    }
  }

  const endDevtoolsResize = (event: React.PointerEvent<HTMLDivElement>) => {
    if (devtoolsResizeRef.current?.pointerID !== event.pointerId) return
    devtoolsResizeRef.current = undefined
    if (devtoolsResizeFrame.current !== undefined) {
      cancelAnimationFrame(devtoolsResizeFrame.current)
      devtoolsResizeFrame.current = undefined
    }
    const next = pendingDevtoolsHeight.current
    pendingDevtoolsHeight.current = undefined
    if (next) setDevtoolsHeight(next)
    event.currentTarget.releasePointerCapture?.(event.pointerId)
  }

  const openDevtoolsPanel = () => {
    if (devtoolsEnabled) setDevtoolsPanelOpen(true)
  }

  const closeDevtoolsPanel = () => {
    setDevtoolsPanelOpen(false)
  }

  const {
    sendInput,
    pointAt,
    modifiers,
    toggleMobileInput,
    onInputBlur,
    onPageTouch,
    onPagePointerDown,
    onPagePointerMove,
    finishPagePointer,
    sendKey,
    sendText,
    onBeforeInput,
    onLocalInput,
    onCompositionStart,
    onCompositionUpdate,
    onCompositionEnd,
  } = createBrowserInputHandlers({
    liveViewEnabled,
    browserControllerRef,
    inspectingRef,
    selectionLockedRef,
    wheelEnabled,
    mouseEnabled,
    touchEnabled,
    keyboardEnabled,
    clipboardEnabled,
    activePageRef,
    hasFrame,
    expectedFrameRef,
    socketRef,
    pendingWheelRef,
    mouseMoveFrameRef,
    heldKeysRef,
    heldMouseRef,
    touchesRef,
    mouseButtonsRef,
    mouseButtonRef,
    clickRef,
    pendingTouchMoveRef,
    touchMoveTimerRef,
    frameRef,
    viewport,
    canvasRef,
    inputRef,
    mobileInputToggleRef,
    mobileInputActive,
    setMobileInputActive,
    clearKeyboardState,
    manualKeyboard,
    inspecting,
    stopInspecting: () => stopInspecting(),
    nextTouchIDRef,
    shortcutKeysRef,
    suppressedKeyRefs,
    composingRef,
    suppressInputRef,
  })

  // 动态关闭输入时补齐已经发送的按下边界，并取消尚未发送的采样。
  // 只发送释放事件，目标仍是按下时的页面，不会把旧手势带入新 Tab。
  useEffect(() => {
    const release = (pageID: string, input: BrowserInput) => {
      const socket = socketRef.current
      if (!browserControllerRef.current || pageID !== activePageRef.current || socket?.readyState !== 1) return
      try {
        socket.send(JSON.stringify({ ...input, page_id: pageID }))
      } catch {
        /* 连接结束时由服务端回收。 */
      }
    }
    if (!mouseEnabled) {
      cancelPendingMouseMove()
      for (const { pageID, input } of heldMouseRef.current.values())
        release(pageID, { ...input, action: 'up', buttons: 0 })
      heldMouseRef.current.clear()
      mouseButtonsRef.current.clear()
      mouseButtonRef.current.clear()
      clickRef.current = undefined
    }
    if (!touchEnabled) {
      cancelPendingTouchMove()
      if (touchesRef.current.size && activePageRef.current)
        release(activePageRef.current, { kind: 'touch', action: 'cancel', touches: [] })
      touchesRef.current.clear()
    }
    if (!keyboardEnabled) {
      for (const { pageID, input } of heldKeysRef.current.values())
        release(pageID, { ...input, action: 'up', text: undefined, auto_repeat: false })
      heldKeysRef.current.clear()
    }
  }, [mouseEnabled, touchEnabled, keyboardEnabled])

  const stopInspecting = () => {
    inspectSequence.current++
    inspectingRef.current = false
    setInspecting(false)
    clearInspection()
  }
  const clearInspection = () => {
    clearTimeout(inspectTimer.current)
    inspectTimer.current = undefined
    inspectSample.current = undefined
    inspectLastSample.current = -Infinity
    inspectSelectionPending.current = false
    inspectSequence.current++
    selectionLockedRef.current = false
    setInspected(undefined)
    setHovered(undefined)
    setArea(undefined)
    setDragArea(undefined)
    dragStart.current = undefined
    setAnnotation('')
  }
  const inspectAt = (clientX: number, clientY: number, select: boolean) => {
    const point = pointAt(clientX, clientY, false)
    const pageID = activePageRef.current
    if (!point || !pageID) return
    if (!source.inspectPoint) {
      if (select) setNotice('当前数据源不支持元素选择')
      return
    }
    const inspect = () => {
      const sequence = ++inspectSequence.current
      if (!select) inspectHoverPending.current = true
      const inspectWithRetry = async (attempt: number): Promise<BrowserInspectedElement> => {
        try {
          return await source.inspectPoint!(sessionID, pageID, point.x, point.y, viewerID)
        } catch (error) {
          const message = error instanceof Error ? error.message : String(error)
          if (
            attempt >= 3 ||
            !transientInspectionError(message) ||
            sequence !== inspectSequence.current ||
            pageID !== activePageRef.current ||
            !inspectingRef.current
          )
            throw error
          await new Promise((resolve) => setTimeout(resolve, 120 * (attempt + 1)))
          if (sequence !== inspectSequence.current || pageID !== activePageRef.current || !inspectingRef.current)
            throw error
          return inspectWithRetry(attempt + 1)
        }
      }
      void inspectWithRetry(0)
        .then((element) => {
          if (sequence !== inspectSequence.current || pageID !== activePageRef.current || !inspectingRef.current) return
          inspectSelectionPending.current = false
          if (select) {
            selectionLockedRef.current = true
            setInspected(element)
            setHovered(undefined)
          } else if (!dragStart.current) setHovered(element)
        })
        .catch((error: Error) => {
          if (sequence !== inspectSequence.current || pageID !== activePageRef.current || !inspectingRef.current) return
          inspectSelectionPending.current = false
          if (select) {
            selectionLockedRef.current = false
            setNotice(error.message)
          }
        })
        .finally(() => {
          if (!select) inspectHoverPending.current = false
        })
    }
    if (select) {
      clearTimeout(inspectTimer.current)
      inspectTimer.current = undefined
      inspectSample.current = undefined
      inspectSelectionPending.current = true
      selectionLockedRef.current = true
      inspect()
      return
    }
    // 首次移动立即检查，其后按 10Hz 采样最新坐标；慢响应期间不积压悬停请求。
    inspectSample.current = inspect
    const sample = () => {
      inspectTimer.current = undefined
      if (!inspectingRef.current || selectionLockedRef.current || dragStart.current) {
        inspectSample.current = undefined
        return
      }
      if (inspectHoverPending.current) {
        inspectTimer.current = setTimeout(sample, inspectSampleIntervalMS)
        return
      }
      const next = inspectSample.current
      inspectSample.current = undefined
      inspectLastSample.current = performance.now()
      next?.()
    }
    if (inspectTimer.current === undefined) {
      const delay = Math.max(0, inspectSampleIntervalMS - (performance.now() - inspectLastSample.current))
      if (delay === 0) sample()
      else inspectTimer.current = setTimeout(sample, delay)
    }
  }

  const finishArea = (clientX: number, clientY: number) => {
    const start = dragStart.current
    dragStart.current = undefined
    const end = pointAt(clientX, clientY, true)
    if (!start || !end || !viewport) return false
    const bounds = {
      x: Math.floor(Math.min(start.x, end.x)),
      y: Math.floor(Math.min(start.y, end.y)),
      width: Math.ceil(Math.abs(start.x - end.x)),
      height: Math.ceil(Math.abs(start.y - end.y)),
    }
    if (bounds.width < 5 || bounds.height < 5) {
      setDragArea(undefined)
      return false
    }
    const sourceCanvas = canvasRef.current
    if (!sourceCanvas) return true
    const sequence = ++inspectSequence.current
    const pageID = activePageRef.current
    const crop = document.createElement('canvas')
    crop.width = bounds.width
    crop.height = bounds.height
    crop
      .getContext('2d')
      ?.drawImage(sourceCanvas, bounds.x, bounds.y, bounds.width, bounds.height, 0, 0, bounds.width, bounds.height)
    crop.toBlob((image) => {
      if (!image || sequence !== inspectSequence.current || pageID !== activePageRef.current || !inspectingRef.current)
        return
      selectionLockedRef.current = true
      setArea({ ...bounds, image })
      setInspected(undefined)
      setHovered(undefined)
      setDragArea(undefined)
    }, 'image/png')
    return true
  }
  useEffect(() => {
    stopInspecting()
  }, [activePageID])

  useEffect(() => {
    inputRef.current?.blur()
    setMobileInputActive(false)
    clearKeyboardState()
  }, [keyboardEnabled, manualKeyboard])

  useEffect(() => {
    if (!visible('keyboard') && mobileInputActive) {
      inputRef.current?.blur()
      setMobileInputActive(false)
    }
  }, [controls === false ? false : controls?.keyboard])

  // React 委托的触摸监听可能为 passive；使用原生 non-passive 监听，
  // 阻止本地滚动和兼容鼠标事件，手势只沿触摸通道传递一次。
  const pageTouchHandler = useRef(onPageTouch)
  pageTouchHandler.current = onPageTouch
  useEffect(() => {
    const frame = frameRef.current
    if (!frame) return
    const start = (event: TouchEvent) => pageTouchHandler.current('start', event)
    const move = (event: TouchEvent) => pageTouchHandler.current('move', event)
    const end = (event: TouchEvent) => pageTouchHandler.current('end', event)
    const cancel = (event: TouchEvent) => pageTouchHandler.current('cancel', event)
    frame.addEventListener('touchstart', start, { passive: false })
    frame.addEventListener('touchmove', move, { passive: false })
    frame.addEventListener('touchend', end, { passive: false })
    frame.addEventListener('touchcancel', cancel, { passive: false })
    return () => {
      frame.removeEventListener('touchstart', start)
      frame.removeEventListener('touchmove', move)
      frame.removeEventListener('touchend', end)
      frame.removeEventListener('touchcancel', cancel)
    }
  }, [])

  // React 在部分 Chromium 版本会把委托的 wheel listener 注册为 passive；
  // 直播画面需要阻止本地页面滚动，因此在实际画面节点上安装 non-passive listener。
  useEffect(() => {
    const frame = frameRef.current
    if (!frame || !viewport || !wheelEnabled || !liveViewEnabled) return
    const onWheel = (event: WheelEvent) => {
      if (inspectingRef.current && selectionLockedRef.current) {
        event.preventDefault()
        return
      }
      if (!browserControllerRef.current) return
      const point = pointAt(event.clientX, event.clientY, false)
      if (!point) return
      const unit =
        event.deltaMode === WheelEvent.DOM_DELTA_LINE
          ? 16
          : event.deltaMode === WheelEvent.DOM_DELTA_PAGE
            ? frame.clientHeight
            : 1
      sendInput({
        kind: 'mouse',
        action: 'wheel',
        x: point.x,
        y: point.y,
        delta_x: event.deltaX * unit * point.scaleX,
        delta_y: event.deltaY * unit * point.scaleY,
        buttons: event.buttons,
        modifiers: modifiers(event),
      })
      event.preventDefault()
    }
    frame.addEventListener('wheel', onWheel, { passive: false })
    return () => frame.removeEventListener('wheel', onWheel)
  }, [hasFrame, viewport, wheelEnabled, liveViewEnabled])

  useEffect(() => {
    // 禁用滚轮后丢弃已经排队的采样，避免下一次显示帧仍发送一次滚轮。
    if (!wheelEnabled) pendingWheelRef.current = undefined
  }, [wheelEnabled])

  // 操作响应已经包含管理器投影；优先直接采用，省去额外的 state 往返。
  const applyActionState = (state: BrowserViewState, fallbackPageID?: string) => {
    if (!acceptState(state)) return false
    const pageID = state.active_page_id || fallbackPageID
    if (pageID) activePageRef.current = pageID
    if (state.tabs) setTabs(state.tabs)
    if (pageID) setActivePageID(pageID)
    if (state.url !== undefined) setPageURL(state.url)
    if (state.viewport?.width && state.viewport.height) {
      const expected = expectedFrameRef.current
      if (
        !expected ||
        expected.pageID !== pageID ||
        (expected.width === state.viewport.width && expected.height === state.viewport.height)
      )
        setViewport(state.viewport)
    }
    return true
  }

  const selectTab = async (pageID: string, force = false) => {
    if (!browserControllerRef.current || !liveViewEnabled || !sessionID || (!force && pageID === activePageID)) {
      // 恢复可见等路径会先写入 pendingTabRef；这里提前返回时必须撤销，
      // 否则后续状态帧会一直被当成“旧活动页”而无法更新地址。
      if (pendingTabRef.current === pageID) pendingTabRef.current = undefined
      return
    }
    const selection = ++tabSelectionRef.current
    const previousPageID = activePageRef.current
    const target = tabs.find((tab) => tab.page_id === pageID)
    // 先乐观切换本地投影：Tab 高亮、地址和缓存帧不等待服务端往返。
    // 切换提交前到达的旧活动页状态由 pendingTabRef 拦截。
    pendingTabRef.current = pageID
    activePageRef.current = pageID
    setActivePageID(pageID)
    if (target?.url) setPageURL(target.url)
    setWaitingForFrame(true)
    setNotice(undefined)
    const local = frameCacheRef.current.get(pageID)
    if (local) {
      void drawFrame(local, pageID)
    } else {
      // 目标 Tab 没有本地帧时清空画布，不能继续显示上一个 Tab 的画面。
      clearFrame()
      setWaitingForFrame(true)
    }
    try {
      let state = await source.activate(sessionID, pageID, viewerID)
      if (selection !== tabSelectionRef.current) return
      if (!state) {
        state = await source.state(sessionID)
        if (selection !== tabSelectionRef.current) return
      }
      if (pendingTabRef.current === pageID) pendingTabRef.current = undefined
      serverActivePageRef.current = state.active_page_id || pageID
      resumeSelectionRef.current = false
      if (!applyActionState(state, pageID)) return
      // 服务端缓存帧可能比本地帧新；后台覆盖，不阻塞切换完成。
      const active = state.active_page_id || pageID
      void source
        .cachedFrame(sessionID)
        .then((cached) => drawFrame(cached, active, false))
        .catch(() => undefined)
    } catch (error) {
      if (selection !== tabSelectionRef.current) return
      pendingTabRef.current = undefined
      resumeSelectionRef.current = false
      if (previousPageID) {
        activePageRef.current = previousPageID
        setActivePageID(previousPageID)
        const previous = tabs.find((tab) => tab.page_id === previousPageID)
        if (previous?.url) setPageURL(previous.url)
        const cached = frameCacheRef.current.get(previousPageID)
        if (cached) {
          void drawFrame(cached, previousPageID)
        } else {
          // 不能把目标页或空白画面当成上一页；清空后等待上一页的缓存/直播帧。
          clearFrame()
          void source
            .cachedFrame(sessionID)
            .then((value) => {
              if (activePageRef.current === previousPageID) void drawFrame(value, previousPageID, false)
            })
            .catch(() => undefined)
        }
      } else {
        setWaitingForFrame(false)
      }
      setNotice(error instanceof Error ? `切换浏览器 Tab 失败：${error.message}` : '切换浏览器 Tab 失败')
      void source
        .state(sessionID)
        .then((state) => {
          if (selection === tabSelectionRef.current) applyActionState(state)
        })
        .catch(() => undefined)
    }
  }

  const appliedSelectionRef = useRef<{ sessionID: string; pageID: string } | undefined>(undefined)
  useEffect(() => {
    if (!liveViewEnabled) {
      // 隐藏期间其他标签可能切走了活动页；恢复可见后必须重新应用宿主的页面选择。
      appliedSelectionRef.current = undefined
      resumeSelectionRef.current = true
      return
    }
    // 先保护隐藏期间保留的最后一帧，等激活完成后再接受新的活动页。
    if (
      resumeSelectionRef.current &&
      selectedPageID &&
      serverActivePageRef.current &&
      serverActivePageRef.current !== selectedPageID
    ) {
      pendingTabRef.current = selectedPageID
    }
  }, [liveViewEnabled, selectedPageID])

  useEffect(() => {
    if (liveViewEnabled) return
    // 隐藏后直播与输入连接都会关闭，操作权也可能在重连宽限期后释放；
    // 恢复可见时必须等待新连接重新确认控制权，不能沿用旧状态直接激活。
    loseBrowserControl()
    tabSelectionRef.current += 1
    pendingTabRef.current = undefined
    browserControlRevisionRef.current = 0
    setBrowserControl(undefined)
    setControlRetryAfterMS(0)
    setTakingControl(false)
  }, [liveViewEnabled])

  useEffect(() => {
    if (!liveViewEnabled || !sessionID || !selectedPageID) return
    // Agent 正在操作时不要为了固定页面抢回活动页；结束后再恢复，
    // 否则本标签的鼠标输入会因服务端活动页不是它而被丢弃。
    if (!followActivePage && busy) return
    const serverPageID = serverActivePageRef.current
    // 隐藏期间或 Agent 操作其他页面后，服务端活动页可能已经变化；
    // 恢复可见时即使本地画面仍是该页也要重新激活。
    const needsActivation = Boolean(serverPageID && serverPageID !== selectedPageID)
    // 后打开的窗口、宿主标签切换和重连只能恢复视图；接管必须点击按钮。
    if (!browserControl?.controller) return
    const applied = appliedSelectionRef.current
    if (applied?.sessionID === sessionID && applied.pageID === selectedPageID && !needsActivation) return
    appliedSelectionRef.current = { sessionID, pageID: selectedPageID }
    if (needsActivation || selectedPageID !== activePageID) void selectTab(selectedPageID, needsActivation)
  }, [followActivePage, busy, liveViewEnabled, sessionID, selectedPageID, browserControl?.controller])

  const syncAfterNavigation = async (pageID: string, state?: BrowserViewState) => {
    if (!sessionID) return
    const next = state ?? (await source.state(sessionID))
    if (!applyActionState(next, pageID)) return
    serverActivePageRef.current = next.active_page_id || pageID
    // 导航返回前可能已经绘制最终帧；状态同步不能重新开启等待。
    void source
      .cachedFrame(sessionID)
      .then((cached) => drawFrame(cached, next.active_page_id || pageID, false))
      .catch(() => undefined)
  }

  const navigateAddress = async () => {
    const address = addressValue.trim()
    const pageID = activePageRef.current || activePageID
    if (!browserControllerRef.current || !liveViewEnabled || !sessionID || !pageID || !address) return
    const selection = tabSelectionRef.current
    try {
      addressEditingRef.current = false
      setWaitingForFrame(true)
      const state = await source.navigate(sessionID, pageID, address, viewerID)
      if (selection !== tabSelectionRef.current || activePageRef.current !== pageID) return
      await syncAfterNavigation(pageID, state || undefined)
      setNotice(undefined)
    } catch (error) {
      if (selection !== tabSelectionRef.current) return
      setWaitingForFrame(false)
      setNotice(error instanceof Error ? `前往页面失败：${error.message}` : '前往页面失败')
    }
  }

  const reloadActivePage = async () => {
    const pageID = activePageRef.current || activePageID
    if (!browserControllerRef.current || !liveViewEnabled || !sessionID || !pageID) return
    const selection = tabSelectionRef.current
    try {
      setWaitingForFrame(true)
      const state = await source.reload(sessionID, pageID, viewerID)
      if (selection !== tabSelectionRef.current || activePageRef.current !== pageID) return
      await syncAfterNavigation(pageID, state || undefined)
      setNotice(undefined)
    } catch (error) {
      if (selection !== tabSelectionRef.current) return
      setWaitingForFrame(false)
      setNotice(error instanceof Error ? `刷新页面失败：${error.message}` : '刷新页面失败')
    }
  }

  const navigateHistory = async (direction: 'back' | 'forward') => {
    const pageID = activePageRef.current || activePageID
    if (!browserControllerRef.current || !liveViewEnabled || !sessionID || !pageID || !source.history) return
    const selection = tabSelectionRef.current
    try {
      setWaitingForFrame(true)
      const state = await source.history(sessionID, pageID, direction, viewerID)
      if (selection !== tabSelectionRef.current || activePageRef.current !== pageID) return
      await syncAfterNavigation(pageID, state || undefined)
      setNotice(undefined)
    } catch (error) {
      if (selection !== tabSelectionRef.current) return
      setWaitingForFrame(false)
      const label = direction === 'back' ? '后退' : '前进'
      setNotice(error instanceof Error ? `浏览器${label}失败：${error.message}` : `浏览器${label}失败`)
    }
  }

  const removeTab = async (pageID: string) => {
    if (!browserControllerRef.current || !liveViewEnabled || !sessionID) return
    try {
      await source.closeTab(sessionID, pageID, viewerID)
      frameCacheRef.current.delete(pageID)
      viewportAppliedRef.current.delete(pageID)
      const state = await source.state(sessionID)
      if (!acceptState(state)) return
      serverActivePageRef.current = state.active_page_id || undefined
      if (state.active_page_id) activePageRef.current = state.active_page_id
      setTabs(state.tabs ?? [])
      setActivePageID(state.active_page_id)
      setPageURL(state.url)
      if (state.viewport?.width && state.viewport?.height) {
        const expected = expectedFrameRef.current
        if (
          !expected ||
          expected.pageID !== state.active_page_id ||
          (expected.width === state.viewport.width && expected.height === state.viewport.height)
        )
          setViewport(state.viewport)
      }
      if (state.active_page_id) {
        const local = frameCacheRef.current.get(state.active_page_id)
        if (local) void drawFrame(local, state.active_page_id)
        void source
          .cachedFrame(sessionID)
          .then((cached) => drawFrame(cached, state.active_page_id, false))
          .catch(() => undefined)
      } else {
        frameCacheRef.current.clear()
        activePageRef.current = undefined
        setPageURL(undefined)
        setViewport(undefined)
        clearFrame()
        setWaitingForFrame(false)
      }
    } catch (error) {
      setNotice(error instanceof Error ? `关闭浏览器 Tab 失败：${error.message}` : '关闭浏览器 Tab 失败')
    }
  }

  const claimBrowserControl = async () => {
    if (!sessionID || browserControllerRef.current || takingControl || controlRetryAfterMS > 0) return
    const feedback = new Promise((resolve) => window.setTimeout(resolve, 400))
    setTakingControl(true)
    try {
      const state = await source.takeControl(sessionID, viewerID)
      applyBrowserControl(state)
      setNotice(undefined)
    } catch (error) {
      setNotice(error instanceof Error ? `获取浏览器操作权失败：${error.message}` : '获取浏览器操作权失败')
    } finally {
      // 快速响应也保留短暂反馈；慢请求沿用实际等待时间。
      await feedback
      setTakingControl(false)
    }
  }

  const close = async () => {
    if (readOnly || !browserControl?.controller || closingRef.current) return
    closingRef.current = true
    setClosing(true)
    loseBrowserControl()
    clearFrame()
    frameCacheRef.current.clear()
    viewportAppliedRef.current.clear()
    onClose?.()
    try {
      await source.closeBrowser(sessionID, viewerID)
      setNotice('浏览器已关闭')
    } catch (error) {
      setNotice(error instanceof Error ? `关闭浏览器失败：${error.message}` : '关闭浏览器失败')
    } finally {
      closingRef.current = false
      setClosing(false)
    }
  }

  const toggleRecording = async () => {
    const pageID = activePageRef.current || activePageID
    if (!sessionID || !pageID || !source.startRecording || !source.finishRecording) return
    if (recording) {
      setRecordingDialog(true)
      return
    }
    setRecordingPending(true)
    try {
      await source.startRecording(sessionID, pageID, viewerID)
      setRecording(true)
      setRecordedSteps(1)
      setNotice('已开始记录浏览器操作')
    } catch (error) {
      setNotice(error instanceof Error ? `开启录制失败：${error.message}` : '开启录制失败')
    } finally {
      setRecordingPending(false)
    }
  }

  const saveRecording = async () => {
    if (!sessionID || !source.finishRecording) return
    const outputPath = recordingSaveMode === 'path' ? recordingPath.trim() : ''
    if (recordingSaveMode === 'path' && !outputPath) return
    setRecordingPending(true)
    try {
      const result = await source.finishRecording(sessionID, outputPath, viewerID)
      setRecording(false)
      setRecordingDialog(false)
      setRecordedSteps(0)
      setNotice(`操作序列已保存到 ${result.path}`)
    } catch (error) {
      setNotice(error instanceof Error ? `保存录制失败：${error.message}` : '保存录制失败')
    } finally {
      setRecordingPending(false)
    }
  }

  const activeTab = tabs.find((tab) => tab.page_id === activePageID)
  const controller = !readOnly && !interactionBusy && !closing && browserControl?.controller === true
  const inspectionLocked = inspecting && Boolean(inspected || area)
  const showAddress = visible('toolbar') && visible('addressBar')
  const showHistory = visible('toolbar') && liveViewEnabled && !readOnly && Boolean(source.history)
  const showBack = showHistory && visible('back')
  const showForward = showHistory && visible('forward')
  const showReload = visible('toolbar') && visible('reload')
  const addressPending = Boolean(addressValue.trim()) && addressValue.trim() !== (pageURL || '').trim()
  const showNavigate = showAddress && visible('navigate') && addressPending
  const showClose = visible('toolbar') && visible('closeBrowser') && liveViewEnabled && !readOnly
  const showDevtools = visible('toolbar') && visible('devtools') && devtoolsEnabled
  const showRecording =
    visible('toolbar') &&
    visible('recording') &&
    Boolean(source.startRecording && source.finishRecording) &&
    liveViewEnabled &&
    !readOnly
  const showInspect = visible('toolbar') && Boolean(onElementSelect || onAreaSelect)
  const showToolbar =
    showAddress || showBack || showForward || showReload || showClose || showDevtools || showInspect || showRecording
  const takeoverSeconds = Math.ceil(controlRetryAfterMS / 1000)

  const content = (
    <section
      ref={setPanelHost}
      data-part="root"
      className={`browserkit ${className} browser-live-window browser-live-window-side-panel ${devtoolsEnabled ? 'browser-live-window-devtools-enabled' : ''} ${devtoolsPanelOpen ? 'browser-live-window-devtools-open' : ''} ${devtoolsOptions?.resizable === false ? 'browser-live-window-devtools-fixed' : ''}   ${syncing ? 'browser-live-window-syncing' : ''} `}
      style={style}
      aria-busy={interactionBusy || closing || undefined}
      aria-label={liveViewEnabled ? (syncing ? '浏览器页面直播（同步中）' : '浏览器页面直播') : '浏览器开发诊断'}
      onKeyDownCapture={(event) => {
        if (inspecting) return
        if (devtoolsEnabled && devtoolsOptions?.shortcut !== false && event.key === 'F12') {
          event.preventDefault()
          event.stopPropagation()
          if (devtoolsPanelOpen) closeDevtoolsPanel()
          else openDevtoolsPanel()
        }
      }}
    >
      {interactionBusy && (
        <div className="browser-live-busy-overlay" role="status">
          Agent 正在操作浏览器…
        </div>
      )}
      {visible('tabs') && (
        <div
          data-part="tabs"
          className={`browser-live-tab-list ${classNames?.tabs || ''}`}
          role="tablist"
          aria-label="浏览器 Tab 列表"
          inert={interactionBusy || closing || inspectionLocked || undefined}
        >
          {tabs.length === 0 && <span className="browser-live-tab-empty">没有可用页面</span>}
          {tabs.map((tab) => (
            <div
              key={tab.page_id}
              data-part="tab"
              className={`browser-live-tab-row ${classNames?.tab || ''} ${tab.page_id === activePageID ? 'browser-live-tab-row-active' : ''} ${tab.status === 'loading' ? 'browser-live-tab-row-loading' : ''}`}
            >
              <button
                type="button"
                role="tab"
                disabled={!liveViewEnabled || !controller}
                aria-selected={tab.page_id === activePageID}
                aria-busy={tab.status === 'loading' || undefined}
                onClick={() => void selectTab(tab.page_id)}
                title={tab.url || tab.title || tab.page_id}
              >
                <i aria-hidden="true" />
                <span>
                  {tab.status === 'loading'
                    ? `${tab.title || tab.url || '新页面'}（加载中）`
                    : tab.title || tab.url || '未命名页面'}
                </span>
              </button>
              {liveViewEnabled && visible('closeTab') && (
                <button
                  type="button"
                  className="browser-live-tab-close"
                  disabled={!controller}
                  aria-label={`关闭 ${tab.title || '浏览器 Tab'}`}
                  onClick={() => void removeTab(tab.page_id)}
                >
                  <X size={12} />
                </button>
              )}
            </div>
          ))}
          {!readOnly && liveViewEnabled && visible('newTab') && (
            <button
              type="button"
              className="browser-new-tab"
              aria-label="新建浏览器 Tab"
              disabled={!controller}
              onClick={() => void source.newTab(sessionID, viewerID).catch((error: Error) => setNotice(error.message))}
            >
              <Plus size={14} />
            </button>
          )}
        </div>
      )}
      {showToolbar && (
        <header data-part="toolbar" className={`browser-live-window-header ${classNames?.toolbar || ''}`}>
          <div
            className="browser-live-address-row"
            inert={interactionBusy || closing || undefined}
            onPointerDown={(event) => {
              event.stopPropagation()
            }}
          >
            {showBack && (
              <button
                type="button"
                className="browser-toolbar-button"
                aria-label="后退"
                disabled={!liveViewEnabled || !controller || !activePageID || inspectionLocked}
                onClick={() => void navigateHistory('back')}
              >
                <ArrowLeft size={15} />
              </button>
            )}
            {showForward && (
              <button
                type="button"
                className="browser-toolbar-button"
                aria-label="前进"
                disabled={!liveViewEnabled || !controller || !activePageID || inspectionLocked}
                onClick={() => void navigateHistory('forward')}
              >
                <ArrowRight size={15} />
              </button>
            )}
            {showReload && (
              <button
                type="button"
                className="browser-toolbar-button"
                aria-label="刷新当前页面"
                disabled={!liveViewEnabled || !controller || !activePageID || inspectionLocked}
                onClick={() => void reloadActivePage()}
              >
                <RefreshCw size={15} />
              </button>
            )}
            {showAddress && (
              <input
                className="browser-address-input"
                aria-label="浏览器页面地址"
                disabled={!liveViewEnabled || !controller || inspectionLocked}
                value={addressValue}
                placeholder="输入网址或搜索"
                spellCheck={false}
                autoCapitalize="off"
                autoCorrect="off"
                onFocus={(event) => {
                  addressEditingRef.current = true
                  // 地址栏聚焦即全选，手机和桌面都能直接输入替换整段地址。
                  event.currentTarget.select()
                }}
                onChange={(event) => setAddressValue(event.target.value)}
                onKeyDown={(event) => {
                  if (event.key === 'Enter') {
                    event.preventDefault()
                    void navigateAddress()
                  }
                }}
                onBlur={() => {
                  addressEditingRef.current = false
                  if (!addressValue.trim()) setAddressValue(pageURL || '')
                }}
                title={pageURL || '浏览器直播'}
              />
            )}
            {showNavigate && (
              <button
                type="button"
                className="browser-toolbar-button"
                aria-label="前往页面"
                disabled={!liveViewEnabled || !controller || !activePageID || inspectionLocked}
                onClick={() => void navigateAddress()}
              >
                <CornerDownLeft size={15} />
              </button>
            )}
          </div>
          <div className="browser-toolbar-actions">
            {showRecording && (
              <button
                type="button"
                className={recording ? 'browser-sequence-recording-active' : undefined}
                aria-label={recording ? '停止并保存操作录制' : '开始操作录制'}
                aria-pressed={recording}
                title={recording ? `正在录制，已记录 ${recordedSteps} 步` : '录制用户浏览器操作'}
                disabled={!controller || !activePageID || recordingPending || inspectionLocked}
                onClick={() => void toggleRecording()}
              >
                {recording ? <Square size={12} /> : <Circle size={14} />}
              </button>
            )}
            {showClose && (
              <button
                type="button"
                className="browser-toolbar-button"
                aria-label="关闭浏览器"
                disabled={!browserControl?.controller || closing || inspectionLocked}
                onClick={() => void close()}
              >
                <X size={15} />
              </button>
            )}
            {showDevtools && (
              <button
                type="button"
                className="browser-toolbar-button"
                aria-label="切换开发诊断 F12"
                aria-pressed={devtoolsPanelOpen}
                onClick={() => {
                  if (devtoolsPanelOpen) closeDevtoolsPanel()
                  else openDevtoolsPanel()
                }}
              >
                <Code2 size={15} />
              </button>
            )}
            {showInspect && (
              <button
                type="button"
                className="browser-toolbar-button"
                aria-label="选择页面元素"
                aria-pressed={inspecting}
                disabled={!controller || !hasFrame || interactionBusy}
                onClick={() => {
                  if (inspecting) stopInspecting()
                  else {
                    clearKeyboardState()
                    inputRef.current?.blur()
                    inspectingRef.current = true
                    setInspecting(true)
                  }
                }}
              >
                <MousePointer2 size={15} />
              </button>
            )}
          </div>
        </header>
      )}
      {
        <div
          className={`browser-live-panel-body browser-live-panel-${mobilePanel}`}
          inert={interactionBusy || closing || undefined}
          style={devtoolsHeight ? ({ '--browser-devtools-height': devtoolsHeight } as CSSProperties) : undefined}
        >
          {devtoolsEnabled && devtoolsPanelOpen && (
            <div className="browser-live-mobile-switch" role="tablist" aria-label="浏览器面板视图">
              <button
                type="button"
                role="tab"
                aria-selected={mobilePanel === 'page'}
                onClick={() => setMobilePanel('page')}
              >
                页面
              </button>
              <button
                type="button"
                role="tab"
                aria-selected={mobilePanel === 'devtools'}
                onClick={() => setMobilePanel('devtools')}
              >
                开发诊断
              </button>
            </div>
          )}
          <div
            ref={frameRef}
            data-part="viewport"
            className={`browser-live-window-frame ${classNames?.viewport || ''} ${mobileInputActive ? 'browser-live-window-frame-input-active' : ''}`}
            style={{ touchAction: touchEnabled && controller ? 'none' : 'auto' }}
            role="application"
            aria-label="可交互浏览器页面"
            aria-disabled={!liveViewEnabled || !controller || undefined}
            tabIndex={liveViewEnabled && controller ? 0 : -1}
            onPointerDown={onPagePointerDown}
            onPointerMove={onPagePointerMove}
            onPointerUp={(event) => finishPagePointer('up', event)}
            onPointerCancel={(event) => finishPagePointer('cancel', event)}
            onKeyDown={(event) => sendKey('down', event)}
            onKeyUp={(event) => sendKey('up', event)}
            onBeforeInput={onBeforeInput}
            onInput={onLocalInput}
            onCompositionStart={onCompositionStart}
            onCompositionUpdate={onCompositionUpdate}
            onCompositionEnd={onCompositionEnd}
            onPaste={(event) => {
              if (keyboardEnabled && clipboardEnabled) sendText(event.clipboardData.getData('text'))
              event.preventDefault()
            }}
            onContextMenu={(event) => event.preventDefault()}
          >
            {keyboardEnabled && (
              <textarea
                ref={inputRef}
                className="browser-live-window-ime"
                aria-label="浏览器直播键盘输入"
                tabIndex={-1}
                autoCapitalize="off"
                autoCorrect="off"
                spellCheck={false}
                onBlur={onInputBlur}
                inputMode={manualKeyboard && !mobileInputActive ? 'none' : 'text'}
              />
            )}
            <canvas
              ref={canvasRef}
              className={hasFrame ? 'browser-live-window-canvas-ready' : ''}
              aria-label="浏览器页面当前画面"
            />
            {inspecting && (
              <div
                className={`browser-element-inspector ${inspected || area ? 'browser-element-inspector-locked' : ''}`}
                role="presentation"
                onPointerMove={(event) => {
                  if (dragStart.current) {
                    const end = pointAt(event.clientX, event.clientY, true)
                    if (end)
                      setDragArea({
                        x: Math.min(dragStart.current.x, end.x),
                        y: Math.min(dragStart.current.y, end.y),
                        width: Math.abs(dragStart.current.x - end.x),
                        height: Math.abs(dragStart.current.y - end.y),
                      })
                  } else if (onElementSelect && !selectionLockedRef.current)
                    inspectAt(event.clientX, event.clientY, false)
                }}
                onPointerDown={(event) => {
                  event.preventDefault()
                  event.stopPropagation()
                  if (selectionLockedRef.current) return
                  clearTimeout(inspectTimer.current)
                  inspectTimer.current = undefined
                  inspectSample.current = undefined
                  const point = pointAt(event.clientX, event.clientY, false)
                  if (point) {
                    inspectSequence.current++
                    setHovered(undefined)
                    dragStart.current = { x: point.x, y: point.y }
                    try {
                      event.currentTarget.setPointerCapture?.(event.pointerId)
                    } catch {
                      /* 继续处理当前指针。 */
                    }
                  }
                }}
                onPointerUp={(event) => {
                  event.stopPropagation()
                  if (dragStart.current) {
                    ignoreInspectClick.current = true
                    try {
                      event.currentTarget.releasePointerCapture?.(event.pointerId)
                    } catch {
                      /* 捕获未建立。 */
                    }
                    if (!finishArea(event.clientX, event.clientY) && onElementSelect)
                      inspectAt(event.clientX, event.clientY, true)
                  }
                }}
                onClick={(event) => {
                  event.stopPropagation()
                  if (ignoreInspectClick.current) {
                    ignoreInspectClick.current = false
                    return
                  }
                  if (inspected || area) clearInspection()
                }}
                onWheel={(event) => {
                  if (inspected || area) {
                    event.preventDefault()
                    event.stopPropagation()
                  }
                }}
                onTouchStart={(event) => {
                  if (inspected || area) event.preventDefault()
                  event.stopPropagation()
                }}
                onKeyDown={(event) => {
                  if (event.key === 'Escape') stopInspecting()
                  event.stopPropagation()
                }}
              >
                {(dragArea || area) && viewport && (
                  <div
                    className="browser-element-inspector-outline browser-element-inspector-area"
                    style={{
                      left: `${((dragArea || area)!.x / viewport.width) * 100}%`,
                      top: `${((dragArea || area)!.y / viewport.height) * 100}%`,
                      width: `${((dragArea || area)!.width / viewport.width) * 100}%`,
                      height: `${((dragArea || area)!.height / viewport.height) * 100}%`,
                    }}
                  />
                )}
                {!dragArea &&
                  !dragStart.current &&
                  !area &&
                  (inspected || hovered) &&
                  viewport &&
                  (() => {
                    const bounds = (inspected || hovered)!.bounds
                    return (
                      <div
                        className="browser-element-inspector-outline"
                        style={{
                          left: `${(bounds.x / viewport.width) * 100}%`,
                          top: `${(bounds.y / viewport.height) * 100}%`,
                          width: `${(bounds.width / viewport.width) * 100}%`,
                          height: `${(bounds.height / viewport.height) * 100}%`,
                        }}
                      />
                    )
                  })()}
                {inspected || area ? (
                  <div
                    className="browser-element-inspector-card"
                    role="dialog"
                    aria-label="选中页面内容"
                    onInput={(event) => event.stopPropagation()}
                    onBeforeInput={(event) => event.stopPropagation()}
                    onPaste={(event) => event.stopPropagation()}
                    onCompositionStart={(event) => event.stopPropagation()}
                    onCompositionUpdate={(event) => event.stopPropagation()}
                    onCompositionEnd={(event) => event.stopPropagation()}
                    onClick={(event) => event.stopPropagation()}
                    onPointerDown={(event) => event.stopPropagation()}
                    onKeyDown={(event) => {
                      if (event.key === 'Escape') stopInspecting()
                      event.stopPropagation()
                    }}
                    onKeyUp={(event) => event.stopPropagation()}
                  >
                    <strong>{area ? '已选区域' : inspected!.name || inspected!.role}</strong>
                    <small>
                      {area ? `${area.width} × ${area.height} 像素` : `${inspected!.role} · ${inspected!.ref}`}
                    </small>
                    <label>
                      批注
                      <textarea
                        value={annotation}
                        onChange={(event) => setAnnotation(event.target.value)}
                        placeholder="描述要对这个元素做什么"
                      />
                    </label>
                    <div className="browser-element-inspector-actions">
                      <button type="button" onClick={clearInspection}>
                        取消
                      </button>
                      <button
                        type="button"
                        onClick={() => {
                          if (area && viewport)
                            onAreaSelect?.({
                              pageID: activePageID!,
                              url: pageURL || '',
                              bounds: { x: area.x, y: area.y, width: area.width, height: area.height },
                              viewport,
                              image: area.image,
                              annotation,
                            })
                          else if (inspected)
                            onElementSelect?.({ pageID: activePageID!, element: inspected, annotation })
                          clearInspection()
                        }}
                      >
                        确认
                      </button>
                    </div>
                  </div>
                ) : (
                  <span className="browser-element-inspector-hint">
                    {onElementSelect ? '点击元素或拖动选择区域' : '拖动选择区域'} · Esc 退出
                  </span>
                )}
              </div>
            )}
            {visible('keyboard') && keyboardEnabled && liveViewEnabled && hasFrame && controller && (
              <button
                type="button"
                className="browser-live-window-mobile-input"
                aria-label={mobileInputActive ? '关闭页面输入' : '开启页面输入'}
                aria-pressed={mobileInputActive}
                title={mobileInputActive ? '关闭输入模式' : '开启输入模式'}
                onPointerDown={(event) => {
                  mobileInputToggleRef.current = true
                  event.stopPropagation()
                }}
                onPointerMove={(event) => event.stopPropagation()}
                onPointerUp={(event) => {
                  mobileInputToggleRef.current = false
                  event.stopPropagation()
                }}
                onPointerCancel={(event) => {
                  mobileInputToggleRef.current = false
                  event.stopPropagation()
                }}
                onKeyDown={(event) => event.stopPropagation()}
                onKeyUp={(event) => event.stopPropagation()}
                onClick={toggleMobileInput}
              >
                <Keyboard size={13} />
                <span>{mobileInputActive ? '完成' : '输入'}</span>
              </button>
            )}
            {(visible('controlStatus') || (visible('takeover') && !readOnly)) &&
              liveViewEnabled &&
              (!controller || takingControl) &&
              !interactionBusy &&
              !closing && (
                <div className="browser-live-window-control-overlay" role="status" aria-label="浏览器观察模式">
                  <div className="browser-live-window-control-copy">
                    {visible('controlStatus') && (
                      <span>
                        {takingControl
                          ? '正在切换浏览器操作权…'
                          : readOnly
                            ? '只读浏览器视图'
                            : browserControl
                              ? '当前浏览器正在由其他窗口控制'
                              : '正在确认浏览器操作权…'}
                      </span>
                    )}
                    {visible('takeover') && browserControl && !readOnly && (
                      <button
                        type="button"
                        disabled={takingControl || controlRetryAfterMS > 0}
                        aria-busy={takingControl || undefined}
                        onPointerDown={(event) => event.stopPropagation()}
                        onPointerUp={(event) => event.stopPropagation()}
                        onClick={() => void claimBrowserControl()}
                      >
                        {takingControl && <i className="browser-control-spinner" aria-hidden="true" />}
                        {takingControl
                          ? '正在切换…'
                          : takeoverSeconds > 0
                            ? `${takeoverSeconds} 秒后可获取`
                            : '获取操作权'}
                      </button>
                    )}
                  </div>
                </div>
              )}
            {!liveViewEnabled ? (
              <span className="browser-live-window-loading">页面直播未启用</span>
            ) : (
              (!hasFrame || waitingForFrame || activeTab?.status === 'loading') && (
                <span
                  className={`browser-live-window-loading ${waitingForFrame || activeTab?.status === 'loading' ? 'browser-live-window-loading-active' : ''}`}
                >
                  <i aria-hidden="true" />
                  {activeTab?.status === 'loading' || waitingForFrame ? '页面加载中…' : '没有可用页面'}
                </span>
              )
            )}
          </div>
          {visible('devtoolsFloating') && devtoolsEnabled && !devtoolsPanelOpen && (
            <button
              type="button"
              className="browser-devtools-panel-trigger"
              aria-label="打开开发诊断面板"
              onClick={openDevtoolsPanel}
            >
              <span aria-hidden="true" />
              开发诊断
            </button>
          )}
          {devtoolsEnabled && devtoolsPanelOpen && devtoolsOptions?.resizable !== false && (
            <div
              className="browser-devtools-resizer"
              role="separator"
              aria-label="调整开发诊断高度"
              aria-orientation="horizontal"
              onPointerDown={startDevtoolsResize}
              onPointerMove={moveDevtoolsResize}
              onPointerUp={endDevtoolsResize}
              onPointerCancel={endDevtoolsResize}
            />
          )}
          {devtoolsEnabled && sessionID && (
            <BrowserDevtoolsPanel
              source={source.diagnostics}
              className={classNames?.devtools}
              sessionID={sessionID}
              pageID={activePageID}
              viewerID={viewerID}
              controlEnabled={controller}
              hidden={!devtoolsPanelOpen || !liveViewEnabled}
              panels={devtoolsOptions?.panels}
              captureControl={devtoolsOptions?.captureControl}
              onClose={devtoolsOptions?.closeButton === false ? undefined : closeDevtoolsPanel}
            />
          )}
        </div>
      }
      {notice && (
        <div role="alert" className="browser-live-window-notice">
          {notice}
        </div>
      )}
      {recordingDialog && (
        <div
          className="browser-sequence-dialog-backdrop"
          role="presentation"
          onPointerDown={(event) => event.stopPropagation()}
        >
          <div
            className="browser-sequence-dialog"
            role="dialog"
            aria-modal="true"
            aria-labelledby="browser-sequence-dialog-title"
            onKeyDown={(event) => {
              if (event.key === 'Escape' && !recordingPending) setRecordingDialog(false)
            }}
          >
            <h3 id="browser-sequence-dialog-title">保存操作录制</h3>
            <p>已记录 {recordedSteps} 个候选步骤。选择保存位置后结束录制。</p>
            <label>
              <input
                type="radio"
                name="browser-sequence-save-mode"
                checked={recordingSaveMode === 'temporary'}
                onChange={() => setRecordingSaveMode('temporary')}
              />
              保存到临时文件
            </label>
            <label>
              <input
                type="radio"
                name="browser-sequence-save-mode"
                checked={recordingSaveMode === 'path'}
                onChange={() => setRecordingSaveMode('path')}
              />
              保存到指定文件
            </label>
            {recordingSaveMode === 'path' && (
              <input
                autoFocus
                aria-label="操作序列保存路径"
                value={recordingPath}
                onChange={(event) => setRecordingPath(event.target.value)}
                placeholder="/absolute/path/sequence.json"
              />
            )}
            <div>
              <button type="button" disabled={recordingPending} onClick={() => setRecordingDialog(false)}>
                继续录制
              </button>
              <button
                type="button"
                disabled={recordingPending || (recordingSaveMode === 'path' && !recordingPath.trim())}
                onClick={() => void saveRecording()}
              >
                {recordingPending ? '保存中…' : '停止并保存'}
              </button>
            </div>
          </div>
        </div>
      )}
    </section>
  )
  return content
}
