package browserkit

import (
	"context"
	"errors"
	"math"
	"time"
)

func (manager *Manager) activeSession() *session {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.sessionLocked("")
}

// activePageForOperation 返回当前页面；返回后不持有 session.mu。
func (manager *Manager) activePageForOperation(ctx context.Context) (*session, *page, error) {
	current := manager.activeSession()
	if current == nil {
		return nil, nil, browserError("page_not_found", "当前没有打开的浏览器页面", "调用 browserOpen 打开页面", true)
	}
	current.mu.Lock()
	closed := current.closed || current.browser.Closed()
	current.mu.Unlock()
	if closed {
		return nil, nil, browserError("session_closed", "浏览器 session 已关闭", "重新调用 browserOpen", true)
	}
	_ = manager.reconcileTabs(ctx, current)
	current.mu.Lock()
	active := manager.activePageLocked(current)
	if active == nil {
		current.mu.Unlock()
		return nil, nil, browserError("page_not_found", "当前没有打开的浏览器页面", "调用 browserOpen 打开页面", true)
	}
	current.lastUsed = time.Now()
	current.mu.Unlock()
	return current, active, nil
}

func (manager *Manager) stopLive() {
	manager.liveMu.Lock()
	hub := manager.live
	manager.live = nil
	manager.liveMu.Unlock()
	if hub != nil {
		hub.closeSubscribers()
		hub.stop()
	}
}

// stopLiveAndWait 停止直播并等待其 goroutine 退出。
// 只能在未持有 session.mu 时调用，避免直播循环正在等待同一把锁。
func (manager *Manager) stopLiveAndWait() {
	manager.liveMu.Lock()
	hub := manager.live
	manager.live = nil
	manager.liveMu.Unlock()
	if hub == nil {
		return
	}
	hub.stop()
	select {
	case <-hub.done:
	case <-time.After(2 * time.Second):
	}
}

// LiveState 是 Web 浮窗查询的当前页面状态，不包含截图数据。
type LiveState struct {
	URL      string
	Title    string
	Viewport Viewport
}

// LiveState 返回当前活动浏览器页面 的状态，不执行截图。
func (manager *Manager) LiveState(ctx context.Context) (LiveState, error) {
	if manager == nil {
		return LiveState{}, browserError("browser_unavailable", "Browser Manager 不存在", "调用 NewManager 创建浏览器运行时", true)
	}
	if err := contextErr(ctx); err != nil {
		return LiveState{}, err
	}
	_, activePage, err := manager.activePageForOperation(ctx)
	if err != nil {
		return LiveState{}, err
	}
	info, err := activePage.page.Info(ctx)
	if err != nil {
		return LiveState{}, err
	}
	return LiveState{URL: info.URL, Title: info.Title, Viewport: activePage.viewport}, nil
}

// DispatchLiveInput 把 Web 直播中的输入发送给当前真实浏览器页面。
func (manager *Manager) DispatchLiveInput(ctx context.Context, pageID string, event InputEvent) error {
	return manager.dispatchInput(ctx, "", pageID, event)
}

// DispatchViewerInput 只接收当前控制方的显式页面输入。
func (manager *Manager) DispatchViewerInput(ctx context.Context, viewerID, pageID string, event InputEvent) error {
	if viewerID == "" || pageID == "" {
		return errors.New("viewer_id 和 page_id 不能为空")
	}
	return manager.dispatchInputs(ctx, viewerID, pageID, []InputEvent{event}, false, 0)
}

// DispatchViewerInputs 按顺序投递同一页面的多条输入，并保留全部采样点。
// 批量轨迹不会与相邻采样合并，span 是这批采样覆盖的时间跨度，用于稳定回放。
func (manager *Manager) DispatchViewerInputs(ctx context.Context, viewerID, pageID string, events []InputEvent, span time.Duration) error {
	if viewerID == "" || pageID == "" {
		return errors.New("viewer_id 和 page_id 不能为空")
	}
	if len(events) == 0 {
		return nil
	}
	return manager.dispatchInputs(ctx, viewerID, pageID, events, true, span)
}

// WaitViewerInput 等待此前投递的输入真正应用到页面。
// 它只用于需要读取输入后状态的检查；普通输入只要求入队顺序，不必等待。
func (manager *Manager) WaitViewerInput(ctx context.Context, pageID string) error {
	if manager == nil {
		return browserError("browser_unavailable", "Browser Manager 不存在", "调用 NewManager 创建浏览器运行时", true)
	}
	if err := contextErr(ctx); err != nil {
		return err
	}
	current, activePage, err := manager.get(ctx, "", pageID)
	if err != nil {
		return err
	}
	if pageID == "" {
		pageID = activePage.id
	}
	current.mu.Lock()
	if current.activeTab != pageID {
		current.mu.Unlock()
		return nil
	}
	epoch := current.inputEpoch
	current.mu.Unlock()
	done := make(chan struct{})
	barrier := liveInput{page: activePage, epoch: epoch, done: done}
	select {
	case current.inputQueue <- barrier:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (manager *Manager) dispatchInput(ctx context.Context, viewerID, pageID string, event InputEvent) error {
	return manager.dispatchInputs(ctx, viewerID, pageID, []InputEvent{event}, false, 0)
}

// maxInputReplaySpan 限制一批采样的回放跨度；积压时压缩回放，避免延迟持续放大。
const maxInputReplaySpan = 100 * time.Millisecond

// liveInteractiveWindow 是用户输入后优先发布的持续时间。连续移动会不断续期，
// 因此交互期间可以按源帧率发布，停顿后回到档位帧率。
const liveInteractiveWindow = 300 * time.Millisecond

const (
	livePointerStep             = 16 * time.Millisecond
	livePointerCoalesceGap      = 4 * time.Millisecond
	livePointerImmediateGap     = 24 * time.Millisecond
	livePointerMaxInterpolation = 100 * time.Millisecond
)

type livePointerState struct {
	viewerID string
	page     *page
	epoch    uint64
	event    InputEvent
	at       time.Time
}

type livePointerTrajectory struct {
	target liveInput
	startX float64
	startY float64
	step   int
	steps  int
}

func (manager *Manager) markInteractive() {
	if manager == nil {
		return
	}
	manager.interactiveUntil.Store(time.Now().Add(liveInteractiveWindow).UnixNano())
	select {
	case manager.interactiveWake <- struct{}{}:
	default:
	}
}

func (manager *Manager) interactive() bool {
	return manager != nil && time.Now().UnixNano() < manager.interactiveUntil.Load()
}

// inputReplayOffsets 把一批采样均匀铺开到 span 内，最后一个点不晚于 span。
func inputReplayOffsets(span time.Duration, count int) []time.Duration {
	offsets := make([]time.Duration, count)
	if count <= 1 || span <= 0 {
		return offsets
	}
	if span > maxInputReplaySpan {
		span = maxInputReplaySpan
	}
	step := span / time.Duration(count)
	for index := 1; index < count; index++ {
		offsets[index] = step * time.Duration(index)
	}
	return offsets
}

func (manager *Manager) dispatchInputs(
	ctx context.Context,
	viewerID, pageID string,
	events []InputEvent,
	lossless bool,
	span time.Duration,
) error {
	if manager == nil {
		return browserError("browser_unavailable", "Browser Manager 不存在", "调用 NewManager 创建浏览器运行时", true)
	}
	if err := contextErr(ctx); err != nil {
		return err
	}
	if viewerID != "" && !manager.CanControl(viewerID) {
		return errors.New("当前窗口没有浏览器操作权")
	}
	current, activePage, err := manager.get(ctx, "", pageID)
	if err != nil {
		return err
	}
	if pageID == "" {
		// 未携带目标的输入在接收瞬间绑定当前 active page。它仍然只进入
		// 该 page/epoch 的队列，后续切 Tab 时会被正常丢弃。
		pageID = activePage.id
	}
	current.mu.Lock()
	if current.activeTab != pageID {
		current.mu.Unlock()
		return nil
	}
	viewport := activePage.viewport
	epoch := current.inputEpoch
	current.mu.Unlock()
	if _, ok := activePage.page.(InputPage); !ok {
		return browserError("input_unsupported", "当前浏览器页面不支持直播交互", "使用支持 CDP Input 域的 Browser Factory", true)
	}
	// 先完成整批校验，避免后半段非法导致前半段已投递的部分轨迹。
	for _, event := range events {
		if err := validateLiveInput(event, viewport); err != nil {
			return err
		}
	}
	manager.markInteractive()
	offsets := inputReplayOffsets(span, len(events))
	var pace *inputBatchPace
	if lossless && len(offsets) > 1 && offsets[len(offsets)-1] > 0 {
		pace = &inputBatchPace{}
	}
	for index, event := range events {
		queued := liveInput{
			viewerID: viewerID, page: activePage, event: event, epoch: epoch,
			lossless: lossless, received: time.Now(),
		}
		if pace != nil {
			queued.pace = pace
			queued.offset = offsets[index]
		}
		if lossless {
			// 批量轨迹必须完整入队；队列满时等待消费者腾出空间，
			// 不能用覆盖或丢弃把真实轨迹变成不完整的历史。
			select {
			case current.inputQueue <- queued:
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		}
		if droppableLiveInput(event) {
			// 连续采样由后续位置覆盖；队列满时不能为了保留它而淘汰快捷键、
			// key up 或触摸边界等不可重建的离散输入。
			select {
			case current.inputQueue <- queued:
			default:
			}
			continue
		}
		// WebSocket 接收循环不能在这里等待队列空位，否则 renderer 变慢时会
		// 停止读取整条连接，并把积压扩散到客户端发送缓冲。离散输入不能静默
		// 伪装成已接收；队列满时立即返回可恢复错误，由外围决定如何提示或重试。
		select {
		case current.inputQueue <- queued:
		default:
			return browserError("input_overloaded", "浏览器输入队列已满", "等待输入恢复后重试", true)
		}
	}
	return nil
}

func sameLivePointerScope(pointer *livePointerState, input liveInput) bool {
	return pointer != nil && pointer.viewerID == input.viewerID && pointer.page == input.page && pointer.epoch == input.epoch
}

func livePointerSteps(span time.Duration) int {
	// 与原前端插值规则保持一致：超密采样等到下一本地帧收敛，
	// 典型 60Hz 采样直接提交，较稀疏且未形成停顿的采样才补点。
	if span <= 0 || (span >= livePointerCoalesceGap && span <= livePointerImmediateGap) ||
		span > livePointerMaxInterpolation {
		return 0
	}
	steps := int(math.Ceil(float64(span) / float64(livePointerStep)))
	if steps < 1 {
		steps = 1
	}
	maximum := int(livePointerMaxInterpolation / livePointerStep)
	if steps > maximum {
		steps = maximum
	}
	return steps
}

// applyLiveInput 在再次校验页面、epoch 和操作权后向 Chrome 提交一项输入。
// 返回值只在事件已经成功交给页面时为 true，供本地指针轨迹更新当前位置。
func (manager *Manager) applyLiveInput(current *session, input liveInput) bool {
	if input.viewerID != "" && !manager.CanControl(input.viewerID) {
		return false
	}
	current.mu.Lock()
	valid := !current.closed && input.page != nil && current.activeTab == input.page.id &&
		current.tabs[input.page.id] == input.page && current.inputEpoch == input.epoch
	current.mu.Unlock()
	if !valid {
		return false
	}
	inputPage, ok := input.page.page.(InputPage)
	if !ok {
		return false
	}
	operationCtx, cancel := context.WithTimeout(current.ctx, liveInputDispatchTimeout)
	current.mu.Lock()
	valid = !current.closed && current.activeTab == input.page.id &&
		current.tabs[input.page.id] == input.page && current.inputEpoch == input.epoch
	if valid {
		current.inputCancel = cancel
	}
	current.mu.Unlock()
	if !valid {
		cancel()
		return false
	}
	succeeded := false
	if lockInteraction(operationCtx, &current.interactionMu) == nil {
		current.mu.Lock()
		valid = !current.closed && current.activeTab == input.page.id &&
			current.tabs[input.page.id] == input.page && current.inputEpoch == input.epoch
		current.mu.Unlock()
		if input.viewerID != "" {
			valid = valid && manager.CanControl(input.viewerID)
		}
		if valid {
			current.mu.Lock()
			recording := current.sequenceDraft != nil && current.sequenceDraft.manual
			current.mu.Unlock()
			var clicked Element
			if recording && input.event.Kind == "mouse" && input.event.Action == "up" && input.event.Button == "left" {
				clicked, _ = manager.manualSequenceElementAt(operationCtx, current, input.page, input.event.X, input.event.Y)
			}
			succeeded = inputPage.DispatchInput(operationCtx, input.event) == nil
			if succeeded && recording {
				manager.recordManualSequenceInput(operationCtx, current, input.page, input.event, clicked)
			}
		}
		current.interactionMu.Unlock()
	}
	cancel()
	current.mu.Lock()
	current.inputCancel = nil
	current.mu.Unlock()
	return succeeded
}

func (manager *Manager) dispatchLiveInputs(current *session) {
	var pending *liveInput
	var pointer *livePointerState
	var trajectory *livePointerTrajectory
	var pointerTimer *time.Timer
	var pointerTick <-chan time.Time
	stopPointerTimer := func() {
		if pointerTimer != nil && !pointerTimer.Stop() {
			select {
			case <-pointerTimer.C:
			default:
			}
		}
		pointerTick = nil
	}
	schedulePointer := func() {
		if pointerTick != nil {
			return
		}
		if pointerTimer == nil {
			pointerTimer = time.NewTimer(livePointerStep)
		} else {
			pointerTimer.Reset(livePointerStep)
		}
		pointerTick = pointerTimer.C
	}
	clearTrajectory := func() {
		stopPointerTimer()
		trajectory = nil
	}
	defer stopPointerTimer()
	flushTrajectory := func() {
		if trajectory == nil {
			return
		}
		target := trajectory.target
		clearTrajectory()
		if manager.applyLiveInput(current, target) {
			pointer = &livePointerState{
				viewerID: target.viewerID, page: target.page, epoch: target.epoch,
				event: target.event, at: time.Now(),
			}
		} else {
			pointer = nil
		}
	}
	for {
		if pending == nil {
			select {
			case <-current.ctx.Done():
				return
			case input := <-current.inputQueue:
				pending = &input
			case <-pointerTick:
				pointerTick = nil
				// 输入与定时器同时就绪时优先采用新目标，避免在已经收到
				// 最新位置后仍向 Chrome 补播旧轨迹中的一个点。
				select {
				case input := <-current.inputQueue:
					pending = &input
					continue
				default:
				}
				if trajectory == nil {
					continue
				}
				trajectory.step++
				ratio := float64(trajectory.step) / float64(trajectory.steps)
				stepInput := trajectory.target
				stepInput.event.X = trajectory.startX + (trajectory.target.event.X-trajectory.startX)*ratio
				stepInput.event.Y = trajectory.startY + (trajectory.target.event.Y-trajectory.startY)*ratio
				if !manager.applyLiveInput(current, stepInput) {
					clearTrajectory()
					pointer = nil
					continue
				}
				pointer = &livePointerState{
					viewerID: stepInput.viewerID, page: stepInput.page, epoch: stepInput.epoch,
					event: stepInput.event, at: time.Now(),
				}
				if trajectory.step >= trajectory.steps {
					clearTrajectory()
				} else {
					schedulePointer()
				}
				continue
			}
		}
		input := *pending
		input, pending = coalesceLiveInputs(input, current.inputQueue)
		// 顺序屏障必须等待此前最新目标真正到达页面。
		if input.done != nil {
			flushTrajectory()
			close(input.done)
			continue
		}
		if pointer != nil && !sameLivePointerScope(pointer, input) {
			clearTrajectory()
			pointer = nil
		}
		// 批量轨迹沿用调用方节奏；直播 mouse move 则由本地状态机平滑。
		if input.pace != nil {
			input.pace.once.Do(func() { input.pace.start = time.Now() })
			if wait := time.Until(input.pace.start.Add(input.offset)); wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-timer.C:
				case <-current.ctx.Done():
					timer.Stop()
					return
				}
			}
		}
		if input.event.Kind == "mouse" && input.event.Action == "move" && !input.lossless {
			targetAt := input.received
			if targetAt.IsZero() {
				targetAt = time.Now()
			}
			if pointer == nil {
				if manager.applyLiveInput(current, input) {
					pointer = &livePointerState{
						viewerID: input.viewerID, page: input.page, epoch: input.epoch,
						event: input.event, at: time.Now(),
					}
				}
				continue
			}
			steps := livePointerSteps(targetAt.Sub(pointer.at))
			if steps == 0 {
				clearTrajectory()
				if manager.applyLiveInput(current, input) {
					pointer = &livePointerState{
						viewerID: input.viewerID, page: input.page, epoch: input.epoch,
						event: input.event, at: time.Now(),
					}
				} else {
					pointer = nil
				}
				continue
			}
			trajectory = &livePointerTrajectory{
				target: input, startX: pointer.event.X, startY: pointer.event.Y,
				steps: steps,
			}
			schedulePointer()
			continue
		}
		// 离散事件是顺序屏障：先精确落到最新 move，再提交 down/up/wheel 等事件。
		flushTrajectory()
		if manager.applyLiveInput(current, input) && input.event.Kind == "mouse" {
			pointer = &livePointerState{
				viewerID: input.viewerID, page: input.page, epoch: input.epoch,
				event: input.event, at: time.Now(),
			}
		}
	}
}

func (manager *Manager) waitLiveInputBarrier(ctx context.Context, current *session) error {
	barrier := make(chan struct{})
	current.mu.Lock()
	epoch := current.inputEpoch
	current.mu.Unlock()
	select {
	case current.inputQueue <- liveInput{epoch: epoch, done: barrier}:
	case <-ctx.Done():
		return ctx.Err()
	case <-current.ctx.Done():
		return errors.New("浏览器 session 已关闭")
	}
	select {
	case <-barrier:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-current.ctx.Done():
		return errors.New("浏览器 session 已关闭")
	}
}

// coalesceLiveInputs 丢弃尚未投递的过时鼠标移动，只保留最新目标。
// 触摸、滚轮和其他离散事件都是顺序屏障，不允许被覆盖或跨越。
func coalesceLiveInputs(first liveInput, queue <-chan liveInput) (liveInput, *liveInput) {
	if first.done != nil || first.lossless || !droppableLiveInput(first.event) {
		return first, nil
	}
	result := first
	for {
		select {
		case next := <-queue:
			if next.done != nil || next.lossless ||
				next.viewerID != result.viewerID || next.page != result.page || next.epoch != result.epoch ||
				next.event.Kind != result.event.Kind || next.event.Action != result.event.Action {
				return result, &next
			}
			result = next
		default:
			return result, nil
		}
	}
}

func droppableLiveInput(event InputEvent) bool {
	return event.Kind == "mouse" && event.Action == "move"
}

func validateLiveInput(event InputEvent, viewport Viewport) error {
	if event.Modifiers < 0 || event.Modifiers > 15 {
		return errors.New("浏览器输入 modifiers 无效")
	}
	if event.Buttons < 0 || event.Buttons > 31 || event.ClickCount < 0 || event.ClickCount > 3 {
		return errors.New("浏览器输入鼠标状态无效")
	}
	if len(event.Text) > 64<<10 || len(event.Key) > 128 || len(event.Code) > 128 {
		return errors.New("浏览器输入文本过长")
	}
	if event.Kind == "key" && event.Modifiers&7 != 0 {
		return errors.New("浏览器不允许直接透传带修饰键的按键，请使用语义化快捷键")
	}
	if event.Kind == "shortcut" {
		if event.Modifiers != 0 && event.Modifiers != 2 && event.Modifiers != 4 {
			return errors.New("语义化快捷键只允许 Ctrl 或 Meta 修饰键")
		}
		switch event.Action {
		case "select_all", "copy", "paste":
		default:
			return errors.New("浏览器只支持全选、复制和粘贴快捷键")
		}
	}
	if event.Kind == "mouse" {
		if err := validateLivePoint(event.X, event.Y, viewport); err != nil {
			return err
		}
		if math.IsNaN(event.DeltaX) || math.IsInf(event.DeltaX, 0) || math.IsNaN(event.DeltaY) || math.IsInf(event.DeltaY, 0) {
			return errors.New("浏览器滚动距离无效")
		}
	}
	for _, point := range event.Touches {
		if err := validateLivePoint(point.X, point.Y, viewport); err != nil {
			return err
		}
		if point.ID < 0 || point.RadiusX < 0 || point.RadiusY < 0 || point.Force < 0 || point.Force > 1 {
			return errors.New("浏览器触点状态无效")
		}
	}
	return nil
}

func validateLivePoint(x, y float64, viewport Viewport) error {
	if math.IsNaN(x) || math.IsInf(x, 0) || math.IsNaN(y) || math.IsInf(y, 0) ||
		x < 0 || y < 0 || viewport.Width <= 0 || viewport.Height <= 0 || x >= float64(viewport.Width) || y >= float64(viewport.Height) {
		return errors.New("浏览器输入坐标超出 viewport")
	}
	return nil
}

// LiveFrame 返回当前活动浏览器页面 的最新画面。
func (manager *Manager) LiveFrame(ctx context.Context) (LiveFrame, error) {
	if manager == nil {
		return LiveFrame{}, browserError("browser_unavailable", "Browser Manager 不存在", "调用 NewManager 创建浏览器运行时", true)
	}
	if err := contextErr(ctx); err != nil {
		return LiveFrame{}, err
	}
	current, activePage, err := manager.activePageForOperation(ctx)
	if err != nil {
		return LiveFrame{}, err
	}
	current.mu.Lock()
	generation := current.inputEpoch
	current.mu.Unlock()
	info, err := activePage.page.Info(ctx)
	if err != nil {
		return LiveFrame{}, err
	}
	screenshot, err := activePage.page.Screenshot(ctx, ScreenshotOptions{})
	if err != nil {
		return LiveFrame{}, err
	}
	frame := LiveFrame{PageID: activePage.id, Generation: generation, Data: screenshot.Data, MIME: "image/" + screenshot.Format, URL: info.URL, Title: info.Title}
	cachePageLiveFrame(activePage, frame)
	return frame, nil
}
