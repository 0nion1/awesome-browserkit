import type React from 'react'
import type { BrowserInputConnection } from './source'

export type RemotePoint = { x: number; y: number; scaleX: number; scaleY: number }
export type PendingMove = { pageID: string | undefined; input: BrowserInput }
export type BrowserInput = {
  kind: 'mouse' | 'touch' | 'key' | 'shortcut' | 'composition' | 'text'
  action?: string
  x?: number
  y?: number
  delta_x?: number
  delta_y?: number
  button?: string
  buttons?: number
  click_count?: number
  pointer_type?: string
  touches?: Array<{ id: number; x: number; y: number; radius_x?: number; radius_y?: number; force?: number }>
  modifiers?: number
  key?: string
  code?: string
  key_code?: number
  location?: number
  auto_repeat?: boolean
  text?: string
  selection_start?: number
  selection_end?: number
}
type Ref<T> = { current: T }
type BrowserViewport = { width: number; height: number }

type BrowserInputContext = {
  liveViewEnabled: boolean
  browserControllerRef: Ref<boolean>
  inspectingRef: Ref<boolean>
  selectionLockedRef: Ref<boolean>
  wheelEnabled: boolean
  mouseEnabled: boolean
  touchEnabled: boolean
  keyboardEnabled: boolean
  clipboardEnabled: boolean
  activePageRef: Ref<string | undefined>
  hasFrame: boolean
  expectedFrameRef: Ref<(BrowserViewport & { pageID: string }) | undefined>
  socketRef: Ref<BrowserInputConnection | undefined>
  pendingWheelRef: Ref<PendingMove | undefined>
  mouseMoveFrameRef: Ref<number | undefined>
  heldKeysRef: Ref<Map<string, { pageID: string; input: BrowserInput }>>
  heldMouseRef: Ref<Map<string, { pageID: string; input: BrowserInput }>>
  touchesRef: Ref<Map<number, RemotePoint & { id: number; radiusX: number; radiusY: number; force: number }>>
  mouseButtonsRef: Ref<Map<number, number>>
  mouseButtonRef: Ref<Map<number, number>>
  clickRef: Ref<{ button: number; x: number; y: number; at: number; count: number } | undefined>
  pendingTouchMoveRef: Ref<PendingMove | undefined>
  touchMoveTimerRef: Ref<number | undefined>
  frameRef: Ref<HTMLDivElement | null>
  viewport: BrowserViewport | undefined
  canvasRef: Ref<HTMLCanvasElement | null>
  inputRef: Ref<HTMLTextAreaElement | null>
  mobileInputToggleRef: Ref<boolean>
  mobileInputActive: boolean
  setMobileInputActive: (active: boolean) => void
  clearKeyboardState: () => void
  manualKeyboard: boolean
  inspecting: boolean
  stopInspecting: () => void
  nextTouchIDRef: Ref<number>
  shortcutKeysRef: Ref<Set<string>>
  suppressedKeyRefs: Ref<Set<string>>
  composingRef: Ref<boolean>
  suppressInputRef: Ref<boolean>
}

const browserInputBackpressureBytes = 16 << 10
// 与设备刷新率解耦，限制可靠触摸输入的传递频率。
const touchMoveIntervalMS = 34

/** 页面输入的坐标、触摸、指针、键盘及输入法事件处理。 */
export function createBrowserInputHandlers(context: BrowserInputContext) {
  const {
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
    stopInspecting,
    nextTouchIDRef,
    shortcutKeysRef,
    suppressedKeyRefs,
    composingRef,
    suppressInputRef,
  } = context

  const sendToSocket = (pageID: string, input: BrowserInput) => {
    const socket = socketRef.current
    if (socket?.readyState !== 1) return false
    try {
      socket.send(JSON.stringify({ ...input, page_id: pageID }))
      if (input.kind === 'key') {
        const key = input.code || input.key || ''
        if (input.action === 'down') heldKeysRef.current.set(key, { pageID, input })
        else heldKeysRef.current.delete(key)
      }
      if (input.kind === 'mouse' && input.button) {
        if (input.action === 'down') heldMouseRef.current.set(input.button, { pageID, input })
        else if (input.action === 'up') heldMouseRef.current.delete(input.button)
      }
      return true
    } catch {
      return false
    }
  }

  const flushPointerInput = () => {
    if (mouseMoveFrameRef.current !== undefined) cancelAnimationFrame(mouseMoveFrameRef.current)
    mouseMoveFrameRef.current = undefined
    const pageID = activePageRef.current
    const wheel = pendingWheelRef.current
    pendingWheelRef.current = undefined
    if (pageID && wheel && wheel.pageID === pageID && wheelEnabled) sendToSocket(pageID, wheel.input)
  }

  const flushWheel = () => {
    mouseMoveFrameRef.current = undefined
    const pageID = activePageRef.current
    const wheel = pendingWheelRef.current
    pendingWheelRef.current = undefined
    if (pageID && wheel && wheel.pageID === pageID && wheelEnabled) sendToSocket(pageID, wheel.input)
  }

  const queueWheel = (input: BrowserInput) => {
    const pageID = activePageRef.current
    if (!pageID) return
    const pending = pendingWheelRef.current
    if (pending && pending.pageID === pageID) {
      pending.input = {
        ...input,
        delta_x: (pending.input.delta_x || 0) + (input.delta_x || 0),
        delta_y: (pending.input.delta_y || 0) + (input.delta_y || 0),
      }
    } else {
      pendingWheelRef.current = { pageID, input }
    }
    if (mouseMoveFrameRef.current === undefined) mouseMoveFrameRef.current = requestAnimationFrame(flushWheel)
  }

  const sendInput = (input: BrowserInput) => {
    // 图流仅用于显示；输入目标只取独立 state 通道确认过的
    // active page，并由 Manager 在入队和投递前再次校验。
    if (!liveViewEnabled || !browserControllerRef.current || (inspectingRef.current && selectionLockedRef.current))
      return
    if (input.kind === 'mouse' && (input.action === 'wheel' ? !wheelEnabled : !mouseEnabled)) return
    if (input.kind === 'touch' && !touchEnabled) return
    if (['key', 'shortcut', 'composition', 'text'].includes(input.kind) && !keyboardEnabled) return
    if (input.kind === 'shortcut' && ['copy', 'paste'].includes(input.action || '') && !clipboardEnabled) return
    const pageID = activePageRef.current
    if (!hasFrame || !pageID) return
    const expected = expectedFrameRef.current
    if (expected && expected.pageID === activePageRef.current) return
    const socket = socketRef.current
    if (socket?.readyState !== 1) return

    if (input.kind === 'mouse' && input.action === 'move') {
      // 滚轮是相对位移，不能被后来的绝对 move 越过。
      flushPointerInput()
      sendToSocket(pageID, input)
      return
    }
    if (input.kind === 'mouse' && input.action === 'wheel') {
      queueWheel(input)
      return
    }
    // 离散输入发送前先提交已缓存的滚轮位移；move 的有序覆盖由
    // Browser Source 完成，这里只传递浏览器真实采样。
    flushPointerInput()
    sendToSocket(pageID, input)
  }

  const flushPendingTouchMove = () => {
    if (touchMoveTimerRef.current !== undefined) window.clearTimeout(touchMoveTimerRef.current)
    touchMoveTimerRef.current = undefined
    const pending = pendingTouchMoveRef.current
    pendingTouchMoveRef.current = undefined
    if (pending && pending.pageID === activePageRef.current) sendInput(pending.input)
  }

  const pumpTouchMove = () => {
    touchMoveTimerRef.current = undefined
    const pending = pendingTouchMoveRef.current
    if (!pending) return
    if ((socketRef.current?.bufferedAmount || 0) < browserInputBackpressureBytes) {
      pendingTouchMoveRef.current = undefined
      if (pending.pageID === activePageRef.current) sendInput(pending.input)
    }
    // 保留完整冷却窗口；下一次采样不能因为前一批已发送就再次立即提交。
    // 拥塞时只保留最新触点，结束边界仍会可靠提交最终位置。
    touchMoveTimerRef.current = window.setTimeout(pumpTouchMove, touchMoveIntervalMS)
  }

  const scheduleTouchMove = (input: BrowserInput) => {
    pendingTouchMoveRef.current = { pageID: activePageRef.current, input }
    if (touchMoveTimerRef.current === undefined) pumpTouchMove()
  }

  const pointAt = (clientX: number, clientY: number, clamp: boolean): RemotePoint | undefined => {
    const frame = frameRef.current
    if (!frame || !viewport) return undefined
    const expected = expectedFrameRef.current
    if (expected && expected.pageID === activePageRef.current) return undefined
    const canvas = canvasRef.current
    if (!canvas || canvas.width !== viewport.width || canvas.height !== viewport.height) return undefined
    const rect = frame.getBoundingClientRect()
    if (rect.width <= 0 || rect.height <= 0) return undefined
    const scale = Math.min(rect.width / viewport.width, rect.height / viewport.height)
    const width = viewport.width * scale
    const height = viewport.height * scale
    const left = rect.left + (rect.width - width) / 2
    const top = rect.top + (rect.height - height) / 2
    if (!clamp && (clientX < left || clientX > left + width || clientY < top || clientY > top + height))
      return undefined
    const x = Math.min(viewport.width - 0.001, Math.max(0, ((clientX - left) / width) * viewport.width))
    const y = Math.min(viewport.height - 0.001, Math.max(0, ((clientY - top) / height) * viewport.height))
    return { x, y, scaleX: viewport.width / width, scaleY: viewport.height / height }
  }

  const modifiers = (event: { altKey: boolean; ctrlKey: boolean; metaKey: boolean; shiftKey: boolean }) =>
    (event.altKey ? 1 : 0) | (event.ctrlKey ? 2 : 0) | (event.metaKey ? 4 : 0) | (event.shiftKey ? 8 : 0)

  const mouseButton = (button: number) => ['left', 'middle', 'right', 'back', 'forward'][button] || 'none'

  const focusInput = () => {
    if (!keyboardEnabled || !browserControllerRef.current || !inputRef.current) return
    // 同一用户事件中先更新 inputMode 再聚焦，确保移动端可以弹出键盘。
    inputRef.current.inputMode = 'text'
    inputRef.current.focus({ preventScroll: true })
  }

  const toggleMobileInput = () => {
    mobileInputToggleRef.current = false
    if (!keyboardEnabled || !browserControllerRef.current) return
    if (mobileInputActive) {
      inputRef.current?.blur()
      clearKeyboardState()
      setMobileInputActive(false)
      return
    }
    setMobileInputActive(true)
    // 必须在用户点击的同一个事件中聚焦，iOS 才会可靠地打开软键盘。
    focusInput()
  }

  const onInputBlur = (event: React.FocusEvent<HTMLTextAreaElement>) => {
    // 点击角标本身会先让 textarea 失焦，再触发 button click；此时保留
    // 状态交给 toggleMobileInput 处理，避免一次点击被误判为系统收起键盘。
    const next = event.relatedTarget
    if (
      mobileInputToggleRef.current ||
      (next instanceof HTMLElement && next.closest('.browser-live-window-mobile-input'))
    )
      return
    clearKeyboardState()
    setMobileInputActive(false)
  }

  const touchInput = (action: 'start' | 'move' | 'end' | 'cancel', event: TouchEvent) => {
    const touchPoints = () =>
      Array.from(touchesRef.current.values(), (touch) => ({
        id: touch.id,
        x: touch.x,
        y: touch.y,
        radius_x: touch.radiusX,
        radius_y: touch.radiusY,
        force: touch.force,
      }))
    // touchend 可能携带比最后一个 touchmove 更新的位置；先提交抬起前的
    // 最终触点，再移除触点，快速拖动也能到达手指实际松开的位置。
    if (action === 'end') {
      let moved = false
      for (const touch of Array.from(event.changedTouches)) {
        const previous = touchesRef.current.get(touch.identifier)
        if (!previous) continue
        const point = pointAt(touch.clientX, touch.clientY, true)
        if (!point || (point.x === previous.x && point.y === previous.y)) continue
        touchesRef.current.set(touch.identifier, { ...previous, ...point })
        moved = true
      }
      if (moved)
        pendingTouchMoveRef.current = {
          pageID: activePageRef.current,
          input: { kind: 'touch', action: 'move', touches: touchPoints(), modifiers: modifiers(event) },
        }
    }
    if (action !== 'move') flushPendingTouchMove()
    if (action !== 'start' && touchesRef.current.size === 0) return
    const changed = new Set(Array.from(event.changedTouches, (touch) => touch.identifier))
    const active = new Set<number>()
    for (const touch of Array.from(event.touches)) {
      const contact = touch as Touch & { radiusX?: number; radiusY?: number; force?: number }
      const previous = touchesRef.current.get(touch.identifier)
      // 只接纳在本画布落下的触点；已接纳触点越界后继续夹取坐标，
      // 不能在多指抬起或追加手指时丢掉仍按住的手指。
      if (!previous && (action !== 'start' || !changed.has(touch.identifier))) continue
      const point = pointAt(touch.clientX, touch.clientY, Boolean(previous))
      if (!point) continue
      active.add(touch.identifier)
      touchesRef.current.set(touch.identifier, {
        ...point,
        id: previous?.id ?? nextTouchIDRef.current++,
        radiusX: Math.max(1, (contact.radiusX || 0.5) * point.scaleX),
        radiusY: Math.max(1, (contact.radiusY || 0.5) * point.scaleY),
        force: contact.force && contact.force > 0 ? Math.min(1, contact.force) : 1,
      })
    }
    for (const identifier of touchesRef.current.keys()) {
      if (!active.has(identifier)) touchesRef.current.delete(identifier)
    }
    const touches = action === 'cancel' ? [] : touchPoints()
    // CDP 的 touchEnd 只接受空触点；多指操作中单指离开时用 move 更新
    // 剩余触点，最后一个触点离开时再发送真正的 end。
    const inputAction = action === 'end' && touches.length > 0 ? 'move' : action
    if ((inputAction === 'start' || inputAction === 'move') && touches.length === 0) return
    const input: BrowserInput = { kind: 'touch', action: inputAction, touches, modifiers: modifiers(event) }
    if (action === 'move') scheduleTouchMove(input)
    else sendInput(input)
    if (action === 'cancel') touchesRef.current.clear()
    if ((action === 'end' || action === 'cancel') && touchesRef.current.size === 0) nextTouchIDRef.current = 0
  }

  const onPageTouch = (action: 'start' | 'move' | 'end' | 'cancel', event: TouchEvent) => {
    if ((event.target as Element).closest('button, .browser-element-inspector-card')) return
    if (inspecting) {
      event.preventDefault()
      return
    }
    if (
      !touchEnabled ||
      !browserControllerRef.current ||
      !liveViewEnabled ||
      !hasFrame ||
      socketRef.current?.readyState !== 1 ||
      Boolean(expectedFrameRef.current)
    )
      return
    event.preventDefault()
    touchInput(action, event)
  }

  const onPagePointerDown = (event: React.PointerEvent<HTMLDivElement>) => {
    if (inspecting) {
      event.preventDefault()
      return
    }

    // 触摸统一由 TouchEvent 处理。荣耀等 Android WebView 会同时派发
    // PointerEvent 与 TouchEvent，若两条路径并存会产生重复 touchStart。
    if (!mouseEnabled || !browserControllerRef.current || !liveViewEnabled || event.pointerType === 'touch') return
    const point = pointAt(event.clientX, event.clientY, false)
    if (!point) return
    if (manualKeyboard || !keyboardEnabled) frameRef.current?.focus({ preventScroll: true })
    else focusInput()
    event.preventDefault()
    // WebKit/WebView 偶尔会在 pointerdown 中对仍然有效的指针抛
    // NotFoundError。Pointer capture 只用于把越界 move/up 留在画面上，
    // 失败不能阻断当前鼠标或触控笔输入本身。
    try {
      event.currentTarget.setPointerCapture?.(event.pointerId)
    } catch {
      /* 继续传递当前输入。 */
    }
    const previous = clickRef.current
    const close =
      previous &&
      previous.button === event.button &&
      event.timeStamp - previous.at < 500 &&
      Math.hypot(previous.x - point.x, previous.y - point.y) < 5
    const count = close ? Math.min(3, previous.count + 1) : 1
    clickRef.current = { button: event.button, x: point.x, y: point.y, at: event.timeStamp, count }
    const pressedButtons = event.buttons || 1 << Math.max(0, event.button)
    mouseButtonsRef.current.set(event.pointerId, pressedButtons)
    mouseButtonRef.current.set(event.pointerId, event.button)
    sendInput({
      kind: 'mouse',
      action: 'down',
      x: point.x,
      y: point.y,
      button: mouseButton(event.button),
      buttons: pressedButtons,
      click_count: count,
      pointer_type: event.pointerType === 'pen' ? 'pen' : 'mouse',
      modifiers: modifiers(event),
    })
  }

  const onPagePointerMove = (event: React.PointerEvent<HTMLDivElement>) => {
    if (inspecting) return
    if (!mouseEnabled || !browserControllerRef.current || !liveViewEnabled || event.pointerType === 'touch') return
    const captured = event.currentTarget.hasPointerCapture?.(event.pointerId) || false
    const coalesced = event.nativeEvent.getCoalescedEvents?.()
    const samples = coalesced?.length ? coalesced : [event.nativeEvent]
    // 同一 PointerEvent 内只提交最后一个浏览器真实采样。平滑轨迹由远端
    // BrowserKit 在 CDP 本地生成，避免把插值点变成远程 RPC。
    let latest: BrowserInput | undefined
    for (const sample of samples) {
      const point = pointAt(sample.clientX, sample.clientY, captured)
      if (!point) continue
      const buttons = sample.buttons || mouseButtonsRef.current.get(sample.pointerId) || 0
      const button = mouseButtonRef.current.get(sample.pointerId)
      latest = {
        kind: 'mouse',
        action: 'move',
        x: point.x,
        y: point.y,
        buttons,
        button: button === undefined ? undefined : mouseButton(button),
        pointer_type: sample.pointerType === 'pen' ? 'pen' : 'mouse',
        modifiers: modifiers(sample),
      }
    }
    if (latest) sendInput(latest)
  }

  const finishPagePointer = (_action: 'up' | 'cancel', event: React.PointerEvent<HTMLDivElement>) => {
    if (inspecting) {
      event.preventDefault()
      return
    }

    if (!mouseEnabled || !browserControllerRef.current || !liveViewEnabled || event.pointerType === 'touch') return
    const point = pointAt(event.clientX, event.clientY, true)
    if (point) {
      const button = mouseButtonRef.current.get(event.pointerId) ?? event.button
      sendInput({
        kind: 'mouse',
        action: 'up',
        x: point.x,
        y: point.y,
        button: mouseButton(button),
        buttons: event.buttons,
        click_count: clickRef.current?.count || 1,
        pointer_type: event.pointerType === 'pen' ? 'pen' : 'mouse',
        modifiers: modifiers(event),
      })
    }
    mouseButtonsRef.current.delete(event.pointerId)
    mouseButtonRef.current.delete(event.pointerId)
    try {
      event.currentTarget.releasePointerCapture?.(event.pointerId)
    } catch {
      /* 捕获未建立时无需释放。 */
    }
    event.preventDefault()
  }

  const sendKey = (action: 'down' | 'up', event: React.KeyboardEvent<HTMLDivElement>) => {
    if (inspecting) {
      if (event.key === 'Escape' && action === 'down') stopInspecting()
      event.preventDefault()
      return
    }
    if (!keyboardEnabled || !browserControllerRef.current) return
    if (composingRef.current || event.key === 'Process' || event.keyCode === 229) return
    const keyTokens = Array.from(
      new Set([event.code?.toLowerCase(), event.key.toLowerCase()].filter(Boolean) as string[]),
    )
    const keyToken = keyTokens[0] || ''
    const shortcutActions: Record<string, string> = {
      keya: 'select_all',
      keyc: 'copy',
      keyv: 'paste',
      a: 'select_all',
      c: 'copy',
      v: 'paste',
    }
    const shortcutAction = shortcutActions[keyToken]
    if (action === 'down' && (event.ctrlKey || event.metaKey) && !event.altKey && !event.shiftKey && shortcutAction) {
      if (!event.repeat) {
        keyTokens.forEach((token) => shortcutKeysRef.current.add(token))
        sendInput({ kind: 'shortcut', action: shortcutAction, modifiers: event.metaKey ? 4 : 2 })
      }
      event.preventDefault()
      event.stopPropagation()
      return
    }
    if (
      action === 'up' &&
      (keyTokens.some((token) => shortcutKeysRef.current.delete(token)) ||
        keyTokens.some((token) => suppressedKeyRefs.current.delete(token)))
    ) {
      event.preventDefault()
      event.stopPropagation()
      return
    }
    // 不把 Ctrl/Cmd/Alt/Win 自身或未允许的组合键发送到远端，避免
    // 修饰键丢失 keyup 后在 Chrome 中形成粘滞状态。
    if (
      event.ctrlKey ||
      event.metaKey ||
      event.altKey ||
      ['Control', 'Meta', 'OS', 'Alt', 'AltGraph'].includes(event.key)
    ) {
      if (action === 'down' && !event.repeat) keyTokens.forEach((token) => suppressedKeyRefs.current.add(token))
      event.preventDefault()
      event.stopPropagation()
      return
    }
    const hasCommandModifier = event.altKey || event.ctrlKey || event.metaKey
    sendInput({
      kind: 'key',
      action,
      key: event.key,
      code: event.code,
      key_code: event.keyCode || event.which,
      location: event.location,
      auto_repeat: action === 'down' && event.repeat,
      modifiers: modifiers(event),
      text: action === 'down' && event.key.length === 1 && !hasCommandModifier ? event.key : undefined,
    })
    event.preventDefault()
    event.stopPropagation()
  }

  const sendText = (text: string) => {
    if (text) sendInput({ kind: 'text', text })
  }

  const sendVirtualKey = (key: string, code: string, keyCode: number) => {
    sendInput({ kind: 'key', action: 'down', key, code, key_code: keyCode })
    sendInput({ kind: 'key', action: 'up', key, code, key_code: keyCode })
  }

  const onBeforeInput = (event: React.FormEvent<HTMLDivElement>) => {
    if (!keyboardEnabled || !browserControllerRef.current || composingRef.current) return
    const native = event.nativeEvent as InputEvent
    if (!clipboardEnabled && native.inputType === 'insertFromPaste') {
      event.preventDefault()
      return
    }
    if (native.inputType === 'deleteContentBackward') {
      sendVirtualKey('Backspace', 'Backspace', 8)
      event.preventDefault()
    } else if (native.inputType === 'deleteContentForward') {
      sendVirtualKey('Delete', 'Delete', 46)
      event.preventDefault()
    } else if (native.data && native.inputType?.startsWith('insert')) {
      sendText(native.data)
      suppressInputRef.current = true
      event.preventDefault()
    }
  }

  const onLocalInput = (event: React.FormEvent<HTMLDivElement>) => {
    const target = event.target as HTMLTextAreaElement
    if (composingRef.current) return
    if (suppressInputRef.current) suppressInputRef.current = false
    else sendText(target.value)
    target.value = ''
  }

  const onCompositionStart = () => {
    composingRef.current = true
  }

  const onCompositionUpdate = () => {
    // 输入法候选态可能每个字都产生多次 update；只在本地记录，避免把
    // 未完成的候选文本排队发送到 CDP，快速输入时造成请求堆积。
  }

  const onCompositionEnd = (event: React.CompositionEvent<HTMLDivElement>) => {
    composingRef.current = false
    suppressInputRef.current = true
    // compositionend.data 才是输入法最终提交的文本；候选态被取消时为空，
    // 不能回退使用之前的 compositionupdate，否则会把未确认的拼音误提交。
    sendText(event.data)
    if (inputRef.current) inputRef.current.value = ''
  }

  return {
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
  }
}
