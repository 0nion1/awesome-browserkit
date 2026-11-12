package browserkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"math"
	"os"
	"strings"
	"time"
)

// Snapshot 执行浏览器页面操作，使用普通 context 和明确目标。
func (manager *Manager) Snapshot(ctx context.Context, request SnapshotRequest) (SnapshotResponse, error) {
	if err := contextErr(ctx); err != nil {
		return SnapshotResponse{}, err
	}
	if request.SessionID == "" {
		return SnapshotResponse{}, errors.New("browserSnapshot 需要 session_id")
	}
	// Snapshot 是 调用方读取 Tab 的同步点：先从浏览器枚举 target，再选择
	// active page。这样省略 page_id 时不会沿用上一次工具调用的页面。
	current, currentPage, err := manager.get(ctx, request.SessionID, request.PageID)
	if err != nil {
		return SnapshotResponse{}, err
	}
	if response, ok := manager.pendingDialogSnapshot(current, currentPage); ok {
		return response, nil
	}
	if err := manager.reconcileTabs(ctx, current); err != nil {
		if response, ok := manager.pendingDialogSnapshot(current, currentPage); ok {
			return response, nil
		}
		return SnapshotResponse{}, err
	}
	current, currentPage, err = manager.get(ctx, request.SessionID, request.PageID)
	if err != nil {
		return SnapshotResponse{}, err
	}
	if request.MaxChars < 0 || request.MaxElements < 0 || request.Offset < 0 || request.Offset > 1_000_000_000 {
		return SnapshotResponse{}, errors.New("browserSnapshot 的 max_chars、max_elements 和 offset 无效")
	}

	response, err := manager.snapshotPage(ctx, current, currentPage, SnapshotOptions{MaxChars: request.MaxChars, MaxElements: request.MaxElements, Offset: request.Offset})
	if err != nil {
		return SnapshotResponse{}, err
	}
	if request.IncludeScreenshot {
		if len(response.Dialogs) > 0 {
			response.ScreenshotError = "页面存在待处理弹窗；先调用 browserHandleDialog，再请求截图"
			return response, nil
		}
		shot, shotErr := manager.Screenshot(ctx, ScreenshotRequest{SessionID: request.SessionID, PageID: response.PageID, Grid: request.ScreenshotGrid})
		if shotErr != nil {
			response.ScreenshotError = shotErr.Error()
		} else {
			response.Screenshot = &shot
		}
	}
	return response, nil
}

func (manager *Manager) snapshotPage(ctx context.Context, current *session, currentPage *page, options SnapshotOptions) (SnapshotResponse, error) {
	if response, ok := manager.pendingDialogSnapshot(current, currentPage); ok {
		return response, nil
	}
	current.mu.Lock()
	generation := currentPage.generation
	current.mu.Unlock()
	type snapshotResult struct {
		snapshot Snapshot
		err      error
	}
	changes := dialogChanges(currentPage.page)
	var snapshot Snapshot
	var err error
	if changes == nil {
		snapshot, err = currentPage.page.Snapshot(ctx, options)
	} else {
		readCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		if response, ok := manager.pendingDialogSnapshot(current, currentPage); ok {
			return response, nil
		}
		result := make(chan snapshotResult, 1)
		go func() {
			value, snapshotErr := currentPage.page.Snapshot(readCtx, options)
			result <- snapshotResult{snapshot: value, err: snapshotErr}
		}()
		select {
		case value := <-result:
			snapshot, err = value.snapshot, value.err
		case <-changes:
			cancel()
			if response, ok := manager.pendingDialogSnapshot(current, currentPage); ok {
				return response, nil
			}
			err = errors.New("页面弹窗状态发生变化")
		case <-ctx.Done():
			err = ctx.Err()
		}
	}
	if err != nil {
		if response, ok := manager.pendingDialogSnapshot(current, currentPage); ok {
			return response, nil
		}
		return SnapshotResponse{}, err
	}
	current.mu.Lock()
	// generation 在导航、外部 Tab 更新时单调递增。快照读取期间页面已经
	// 变化时，只把结果返回给调用方，不能用旧 URL 和旧 refs 覆盖新投影。
	if currentPage.generation == generation {
		if snapshot.Revision >= currentPage.refRevision {
			currentPage.info = snapshot.Info
			currentPage.status = "ready"
			rememberSnapshotRefs(currentPage, snapshot)
			cached := snapshot
			cached.Elements = append([]Element(nil), snapshot.Elements...)
			currentPage.lastSnapshot = &cached
		}
	}
	tabsRevision := tabsRevisionLocked(current)
	tabsChanged := !current.snapshotInitialized || current.snapshotRevision != tabsRevision
	current.snapshotRevision = tabsRevision
	current.snapshotInitialized = true
	activePageID := current.activeTab
	tabs := tabViewsLocked(current)
	current.mu.Unlock()
	// 自定义 Page 可能尚未设置 PaginationKnown；满页时沿用旧的
	// next_offset 推断，避免第三方实现的分页静默停止。
	effectiveMaxElements := options.MaxElements
	if effectiveMaxElements <= 0 {
		effectiveMaxElements = 100
	} else if effectiveMaxElements > maxSnapshotElements {
		effectiveMaxElements = maxSnapshotElements
	}
	hasMore := snapshot.HasMore
	if !snapshot.PaginationKnown && len(snapshot.Elements) == effectiveMaxElements {
		hasMore = true
	}
	nextOffset := 0
	if hasMore {
		nextOffset = options.Offset + len(snapshot.Elements)
	}
	return SnapshotResponse{SessionID: current.id, PageID: currentPage.id, Revision: snapshot.Revision, URL: snapshot.Info.URL, Title: snapshot.Info.Title, Text: snapshot.Text, Elements: snapshot.Elements, Truncated: snapshot.Truncated, TextTruncated: snapshot.TextTruncated, NextOffset: nextOffset, ActivePageID: activePageID, TabsRevision: tabsRevision, TabsChanged: tabsChanged, Tabs: tabs}, nil
}

// pendingDialogSnapshot 在 renderer 被原生弹窗暂停时返回最近一次成功页面投影，
// 并把当前弹窗作为页面状态附加；它不读取 DOM，也不消费弹窗。
func (manager *Manager) pendingDialogSnapshot(current *session, currentPage *page) (SnapshotResponse, bool) {
	if current == nil || currentPage == nil {
		return SnapshotResponse{}, false
	}
	dialog := pendingPageDialog(currentPage.page)
	if dialog == nil {
		return SnapshotResponse{}, false
	}
	current.mu.Lock()
	response := SnapshotResponse{
		SessionID: current.id, PageID: currentPage.id,
		URL: currentPage.info.URL, Title: currentPage.info.Title,
		Revision: currentPage.refRevision,
		Elements: []Element{}, Dialogs: []Dialog{*dialog},
		ActivePageID: current.activeTab, TabsRevision: tabsRevisionLocked(current),
		Tabs: tabViewsLocked(current),
	}
	if currentPage.lastSnapshot != nil {
		cached := currentPage.lastSnapshot
		response.Revision = cached.Revision
		response.URL, response.Title = cached.Info.URL, cached.Info.Title
		response.Text = cached.Text
		response.Elements = append([]Element(nil), cached.Elements...)
		response.Truncated = cached.Truncated
		response.TextTruncated = cached.TextTruncated
	}
	current.mu.Unlock()
	return response, true
}

// Click 执行浏览器页面操作，使用普通 context 和明确目标。
func (manager *Manager) Click(ctx context.Context, request ClickRequest) (ClickResponse, error) {
	operationCtx, cancel, err := operationContext(ctx, request.TimeoutMS)
	if err != nil {
		return ClickResponse{}, err
	}
	defer cancel()
	if err := requirePage(request.SessionID, request.PageID, "browserClick"); err != nil {
		return ClickResponse{}, err
	}

	if _, _, err := clickParameters(ClickOptions{Button: request.Button, ClickCount: request.ClickCount}); err != nil {
		return ClickResponse{}, err
	}

	current, currentPage, err := manager.get(operationCtx, request.SessionID, request.PageID)
	if err != nil {
		return ClickResponse{}, err
	}
	pointMode := request.Ref == ""
	if pointMode {
		if request.X == nil || request.Y == nil {
			return ClickResponse{}, errors.New("browserClick 需要 ref 或 x/y")
		}
	} else if request.X != nil || request.Y != nil {
		return ClickResponse{}, errors.New("browserClick 的 ref 与 x/y 只能二选一")
	} else if request.Revision == 0 {
		return ClickResponse{}, errors.New("browserClick 需要 ref 和 revision")
	}
	if request.WaitAfterMS < 0 {
		return ClickResponse{}, errors.New("browserClick wait_after_ms 不能为负数")
	}
	element, _ := sequenceElement(current, currentPage, request.Ref)
	unlockInteraction, err := lockSessionInteraction(operationCtx, current)
	if err != nil {
		return ClickResponse{}, err
	}
	defer unlockInteraction()
	current.mu.Lock()
	beforeTabsRevision := tabsRevisionLocked(current)
	beforeActive := current.activeTab
	refErr := error(nil)
	if !pointMode {
		refErr = validatePageRef(currentPage, request.Ref, request.Revision)
	}
	current.mu.Unlock()
	if refErr != nil {
		return ClickResponse{}, refErr
	}
	if pointMode {
		current.mu.Lock()
		viewport := currentPage.viewport
		current.mu.Unlock()
		if err := validateViewportPoint(viewport, *request.X, *request.Y); err != nil {
			return ClickResponse{}, err
		}
		pointer, ok := currentPage.page.(PointClickPage)
		if !ok {
			return ClickResponse{}, browserError("click_point_unsupported", "当前浏览器页面不支持按坐标点击", "使用快照 ref 或实现 PointClickPage", true)
		}
		if err := pointer.ClickPoint(operationCtx, *request.X, *request.Y, ClickOptions{Button: request.Button, ClickCount: request.ClickCount}); err != nil {
			return ClickResponse{}, err
		}
	} else if err := currentPage.page.Click(operationCtx, ElementRef{Ref: request.Ref, Revision: request.Revision}, ClickOptions{Button: request.Button, ClickCount: request.ClickCount}); err != nil {
		return ClickResponse{}, err
	}
	var waitErr error
	if request.WaitAfterMS > 0 {
		waitErr = waitContext(operationCtx, time.Duration(request.WaitAfterMS)*time.Millisecond)
	}
	candidateID := ""
	// 既有序列格式只表达默认的元素单击；不能把坐标、右键或双击
	// 偷换成回放时的左键单击。
	if !pointMode && (request.Button == "" || request.Button == "left") && request.ClickCount <= 1 {
		candidateID = recordSequenceStep(current, currentPage, SequenceStep{Type: "click", Locator: locatorForSequenceElement(element), WaitAfterMS: request.WaitAfterMS}, "succeeded")
	}
	manager.emit(operationCtx, Event{Kind: "interaction_finished", SessionID: current.id, PageID: currentPage.id})
	unlockInteraction()
	activePageID, tabsRevision, tabsChanged := manager.tabChange(operationCtx, current, beforeTabsRevision)
	response := ClickResponse{SessionID: current.id, PageID: currentPage.id, Ref: request.Ref, Changed: true, ActivePageID: activePageID, TabsRevision: tabsRevision, TabsChanged: tabsChanged, CandidateID: candidateID}
	response.Snapshot, response.SnapshotError = manager.actionSnapshot(operationCtx, current, currentPage)
	if waitErr != nil {
		notice := "click 已执行，wait_after_ms 未完成（" + waitErr.Error() + "）；页面可能仍在加载，不要重放点击，可继续 browserWait 或 browserSnapshot 检查"
		if response.SnapshotError != "" {
			notice = response.SnapshotError + "；" + notice
		}
		response.SnapshotError = notice
	}
	if request.IncludeScreenshot {
		pageID := request.PageID
		if activePageID != "" && activePageID != beforeActive && activePageID != request.PageID {
			pageID = activePageID
		}
		shot, shotErr := manager.Screenshot(operationCtx, ScreenshotRequest{SessionID: request.SessionID, PageID: pageID})
		if shotErr != nil {
			response.ScreenshotError = shotErr.Error()
		} else {
			response.Screenshot = &shot
		}
	}
	return response, nil
}

// Hover 执行浏览器页面操作，使用普通 context 和明确目标。
func (manager *Manager) Hover(ctx context.Context, request HoverRequest) (HoverResponse, error) {
	operationCtx, cancel, err := operationContext(ctx, 0)
	if err != nil {
		return HoverResponse{}, err
	}
	defer cancel()
	if err := requirePage(request.SessionID, request.PageID, "browserHover"); err != nil {
		return HoverResponse{}, err
	}
	if request.Ref == "" || request.Revision == 0 {
		return HoverResponse{}, errors.New("browserHover 需要 ref 和 revision")
	}

	current, currentPage, err := manager.get(operationCtx, request.SessionID, request.PageID)
	if err != nil {
		return HoverResponse{}, err
	}
	hoverPage, ok := currentPage.page.(HoverPage)
	if !ok {
		return HoverResponse{}, browserError("hover_unsupported", "当前浏览器页面不支持真实指针悬停", "使用支持 BrowserKit HoverPage 的 Browser Factory", true)
	}
	unlockInteraction, err := lockSessionInteraction(operationCtx, current)
	if err != nil {
		return HoverResponse{}, err
	}
	defer unlockInteraction()
	current.mu.Lock()
	beforeTabsRevision := tabsRevisionLocked(current)
	if err := validatePageRef(currentPage, request.Ref, request.Revision); err != nil {
		current.mu.Unlock()
		return HoverResponse{}, err
	}
	current.mu.Unlock()
	if err := hoverPage.Hover(operationCtx, ElementRef{Ref: request.Ref, Revision: request.Revision}); err != nil {
		return HoverResponse{}, err
	}
	unlockInteraction()
	activePageID, tabsRevision, tabsChanged := manager.tabChange(operationCtx, current, beforeTabsRevision)
	response := HoverResponse{SessionID: current.id, PageID: currentPage.id, Ref: request.Ref, ActivePageID: activePageID, TabsRevision: tabsRevision, TabsChanged: tabsChanged}
	response.Snapshot, response.SnapshotError = manager.actionSnapshot(operationCtx, current, currentPage)
	return response, nil
}

// Type 执行浏览器页面操作，使用普通 context 和明确目标。
func (manager *Manager) Type(ctx context.Context, request TypeRequest) (TypeResponse, error) {
	return manager.typeElement(ctx, request, true)
}

func (manager *Manager) typeElement(ctx context.Context, request TypeRequest, record bool) (TypeResponse, error) {
	if err := contextErr(ctx); err != nil {
		return TypeResponse{}, err
	}
	if err := requirePage(request.SessionID, request.PageID, "browserType"); err != nil {
		return TypeResponse{}, err
	}

	current, currentPage, err := manager.get(ctx, request.SessionID, request.PageID)
	if err != nil {
		return TypeResponse{}, err
	}
	if request.Ref == "" || request.Revision == 0 {
		return TypeResponse{}, errors.New("browserType 需要 ref 和 revision")
	}
	if request.SequenceVariable != "" && !sequenceVariablePattern.MatchString(strings.TrimSpace(request.SequenceVariable)) {
		return TypeResponse{}, fmt.Errorf("序列变量名 %q 无效", request.SequenceVariable)
	}
	element, _ := sequenceElement(current, currentPage, request.Ref)
	unlockInteraction, err := lockSessionInteraction(ctx, current)
	if err != nil {
		return TypeResponse{}, err
	}
	defer unlockInteraction()
	current.mu.Lock()
	beforeTabsRevision := tabsRevisionLocked(current)
	refErr := validatePageRef(currentPage, request.Ref, request.Revision)
	current.mu.Unlock()
	if refErr != nil {
		return TypeResponse{}, refErr
	}
	if err := currentPage.page.Type(ctx, ElementRef{Ref: request.Ref, Revision: request.Revision}, request.Text, !request.Append); err != nil {
		return TypeResponse{}, err
	}
	if request.Submit {
		if err := currentPage.page.Press(ctx, PressOptions{Key: "Enter"}); err != nil {
			return TypeResponse{}, err
		}
	}
	candidateID := ""
	if record {
		candidateID = recordSequenceInput(current, currentPage, element, request)
	}
	unlockInteraction()
	activePageID, tabsRevision, tabsChanged := manager.tabChange(ctx, current, beforeTabsRevision)
	response := TypeResponse{SessionID: current.id, PageID: currentPage.id, Ref: request.Ref, TextLength: len([]rune(request.Text)), Submitted: request.Submit, ActivePageID: activePageID, TabsRevision: tabsRevision, TabsChanged: tabsChanged, CandidateID: candidateID}
	response.Snapshot, response.SnapshotError = manager.actionSnapshot(ctx, current, currentPage)
	return response, nil
}

// SelectOption 按快照中的值或标签选择单选原生下拉框选项。
func (manager *Manager) SelectOption(ctx context.Context, request SelectOptionRequest) (SelectOptionResponse, error) {
	if err := contextErr(ctx); err != nil {
		return SelectOptionResponse{}, err
	}
	if err := requirePage(request.SessionID, request.PageID, "browserSelectOption"); err != nil {
		return SelectOptionResponse{}, err
	}
	if request.Ref == "" || request.Revision == 0 || (request.Value == "" && request.Label == "") || (request.Value != "" && request.Label != "") {
		return SelectOptionResponse{}, errors.New("browserSelectOption 需要 ref、revision，以及 value 或 label 其中之一")
	}
	current, currentPage, err := manager.get(ctx, request.SessionID, request.PageID)
	if err != nil {
		return SelectOptionResponse{}, err
	}
	selectPage, ok := currentPage.page.(SelectOptionPage)
	if !ok {
		return SelectOptionResponse{}, browserError("select_unsupported", "当前浏览器页面不支持选择下拉选项", "使用支持 BrowserKit SelectOptionPage 的 Browser Factory", true)
	}
	unlockInteraction, err := lockSessionInteraction(ctx, current)
	if err != nil {
		return SelectOptionResponse{}, err
	}
	defer unlockInteraction()
	current.mu.Lock()
	beforeTabsRevision := tabsRevisionLocked(current)
	refErr := validatePageRef(currentPage, request.Ref, request.Revision)
	current.mu.Unlock()
	if refErr != nil {
		return SelectOptionResponse{}, refErr
	}
	value, err := selectPage.SelectOption(ctx, ElementRef{Ref: request.Ref, Revision: request.Revision}, request.Value, request.Label)
	if err != nil {
		return SelectOptionResponse{}, err
	}
	unlockInteraction()
	activePageID, tabsRevision, tabsChanged := manager.tabChange(ctx, current, beforeTabsRevision)
	response := SelectOptionResponse{SessionID: current.id, PageID: currentPage.id, Ref: request.Ref, Value: value, ActivePageID: activePageID, TabsRevision: tabsRevision, TabsChanged: tabsChanged}
	response.Snapshot, response.SnapshotError = manager.actionSnapshot(ctx, current, currentPage)
	return response, nil
}

// Press 执行浏览器页面操作，使用普通 context 和明确目标。
func (manager *Manager) Press(ctx context.Context, request PressRequest) (PressResponse, error) {
	if err := contextErr(ctx); err != nil {
		return PressResponse{}, err
	}
	if err := requirePage(request.SessionID, request.PageID, "browserPress"); err != nil {
		return PressResponse{}, err
	}

	if _, err := pressParameters(PressOptions{Key: request.Key, Modifiers: request.Modifiers}); err != nil {
		return PressResponse{}, err
	}
	if (request.Ref == "") != (request.Revision == 0) {
		return PressResponse{}, errors.New("browserPress 的 ref 和 revision 必须一起提供")
	}
	var targetRef *ElementRef
	if request.Ref != "" {
		targetRef = &ElementRef{Ref: request.Ref, Revision: request.Revision}
	}
	current, currentPage, err := manager.get(ctx, request.SessionID, request.PageID)
	if err != nil {
		return PressResponse{}, err
	}
	unlockInteraction, err := lockSessionInteraction(ctx, current)
	if err != nil {
		return PressResponse{}, err
	}
	defer unlockInteraction()
	current.mu.Lock()
	beforeTabsRevision := tabsRevisionLocked(current)
	var refErr error
	if targetRef != nil {
		refErr = validatePageRef(currentPage, targetRef.Ref, targetRef.Revision)
	}
	current.mu.Unlock()
	if refErr != nil {
		return PressResponse{}, refErr
	}
	if err := currentPage.page.Press(ctx, PressOptions{Key: request.Key, Modifiers: request.Modifiers, Ref: targetRef}); err != nil {
		return PressResponse{}, err
	}
	unlockInteraction()
	activePageID, tabsRevision, tabsChanged := manager.tabChange(ctx, current, beforeTabsRevision)
	response := PressResponse{SessionID: current.id, PageID: currentPage.id, Key: request.Key, ActivePageID: activePageID, TabsRevision: tabsRevision, TabsChanged: tabsChanged}
	response.Snapshot, response.SnapshotError = manager.actionSnapshot(ctx, current, currentPage)
	return response, nil
}

// Scroll 向页面视口或指定坐标下的滚动容器发送真实滚轮事件。
func (manager *Manager) Scroll(ctx context.Context, request ScrollRequest) (ScrollResponse, error) {
	if err := contextErr(ctx); err != nil {
		return ScrollResponse{}, err
	}
	if err := requirePage(request.SessionID, request.PageID, "browserScroll"); err != nil {
		return ScrollResponse{}, err
	}
	if math.IsNaN(request.DeltaX) || math.IsNaN(request.DeltaY) || math.IsInf(request.DeltaX, 0) || math.IsInf(request.DeltaY, 0) ||
		(request.DeltaX == 0 && request.DeltaY == 0) || math.Abs(request.DeltaX) > 10_000 || math.Abs(request.DeltaY) > 10_000 {
		return ScrollResponse{}, errors.New("browserScroll 需要非零且不超过 10000 CSS 像素的滚动距离")
	}
	current, currentPage, err := manager.get(ctx, request.SessionID, request.PageID)
	if err != nil {
		return ScrollResponse{}, err
	}
	inputPage, ok := currentPage.page.(InputPage)
	if !ok {
		return ScrollResponse{}, browserError("scroll_unsupported", "当前浏览器页面不支持滚轮输入", "使用支持 BrowserKit InputPage 的 Browser Factory", true)
	}
	viewportResult, err := currentPage.page.Evaluate(ctx, "({width: window.innerWidth, height: window.innerHeight})", EvaluateOptions{})
	if err != nil {
		return ScrollResponse{}, err
	}
	var viewport Viewport
	if err := json.Unmarshal(viewportResult.Value, &viewport); err != nil {
		return ScrollResponse{}, err
	}
	if err := viewport.Validate(); err != nil {
		return ScrollResponse{}, err
	}
	x, y := float64(viewport.Width)/2, float64(viewport.Height)/2
	if request.X != nil {
		x = *request.X
	}
	if request.Y != nil {
		y = *request.Y
	}
	if math.IsNaN(x) || math.IsNaN(y) || math.IsInf(x, 0) || math.IsInf(y, 0) || x < 0 || y < 0 || x >= float64(viewport.Width) || y >= float64(viewport.Height) {
		return ScrollResponse{}, errors.New("browserScroll 的 x/y 超出页面视口")
	}
	unlockInteraction, err := lockSessionInteraction(ctx, current)
	if err != nil {
		return ScrollResponse{}, err
	}
	defer unlockInteraction()
	if err := inputPage.DispatchInput(ctx, InputEvent{Kind: "mouse", Action: "wheel", X: x, Y: y, DeltaX: request.DeltaX, DeltaY: request.DeltaY}); err != nil {
		return ScrollResponse{}, err
	}
	manager.emit(ctx, Event{Kind: "interaction_finished", SessionID: current.id, PageID: currentPage.id})
	unlockInteraction()
	_ = manager.reconcileTabs(ctx, current)
	response := ScrollResponse{SessionID: current.id, PageID: currentPage.id, DeltaX: request.DeltaX, DeltaY: request.DeltaY, X: x, Y: y}
	response.Snapshot, response.SnapshotError = manager.actionSnapshot(ctx, current, currentPage)
	return response, nil
}

// tabChange 在交互返回前刷新一次真实 Tab 列表，并返回模型需要立即
// 看到的轻量状态。reconcile 失败时保留本地投影，避免把已经完成的点击
// 误报成工具失败；下一次 browserSnapshot 会再次强制同步。
func (manager *Manager) tabChange(ctx context.Context, current *session, before uint64) (string, uint64, bool) {
	if current == nil {
		return "", 0, false
	}
	_ = manager.reconcileTabs(ctx, current)
	current.mu.Lock()
	defer current.mu.Unlock()
	return tabChangeLocked(current, before)
}

// Wait 执行浏览器页面操作，使用普通 context 和明确目标。
func (manager *Manager) Wait(ctx context.Context, request WaitRequest) (WaitResponse, error) {
	waitCtx, cancel, err := operationContext(ctx, request.TimeoutMS)
	if err != nil {
		return WaitResponse{}, err
	}
	defer cancel()

	if err := requirePage(request.SessionID, request.PageID, "browserWait"); err != nil {
		return WaitResponse{}, err
	}
	current, currentPage, err := manager.get(waitCtx, request.SessionID, request.PageID)
	if err != nil {
		return WaitResponse{}, err
	}
	if request.Condition == "" {
		return WaitResponse{}, errors.New("browserWait 需要 condition")
	}
	if request.Condition != "load_complete" && request.RefOrSelector == "" {
		return WaitResponse{}, errors.New("browserWait 需要 ref_or_selector；load_complete 可省略")
	}
	selector := request.RefOrSelector
	current.mu.Lock()
	_, known := currentPage.refs[request.RefOrSelector]
	refRevision := currentPage.refRevision
	current.mu.Unlock()
	if request.Condition == "selector_visible" || request.Condition == "selector_hidden" {
		if known {
			if request.Revision != 0 && request.Revision != refRevision {
				return WaitResponse{}, browserError("stale_reference", "元素引用已经过期", "在目标 page_id 上重新调用 browserSnapshot", true)
			}
			selector = `[data-browserkit-ref="` + strings.ReplaceAll(strings.ReplaceAll(request.RefOrSelector, `\`, `\\`), `"`, `\"`) + `"]`
			if request.Condition == "selector_visible" {
				// 批量步骤可能已删除元素，但 manager 的 ref 投影直到批次结束
				// 才更新。先确认 ref 仍在 DOM 中，避免把一个明确的失效引用
				// 等成超时。自定义 Page 未实现时保持原有等待语义。
				if checker, ok := currentPage.page.(RefAwarePage); ok {
					// 这是一次可选的加速检查：读取失败时退回正常等待，
					// 避免把页面导航中的瞬时错误变成 wait 失败。检查只使用
					// 剩余预算的一小部分，防止大 DOM 预检吃掉全部等待时间。
					if budget, ok := refCheckBudget(waitCtx); ok {
						checkCtx, checkCancel := context.WithTimeout(waitCtx, budget)
						exists, existsErr := checker.HasRef(checkCtx, selector)
						checkCancel()
						if existsErr == nil && !exists {
							return WaitResponse{}, browserError("stale_reference", "元素已从页面移除", "重新调用 browserSnapshot；若元素会被异步删除，改用 CSS selector", true)
						}
					}
				}
			}
		} else if request.Revision != 0 || isSnapshotRef(request.RefOrSelector) {
			return WaitResponse{}, browserError("invalid_reference", "当前 Tab 没有该元素引用", "在目标 page_id 上重新调用 browserSnapshot", true)
		}
	}
	if err := currentPage.page.Wait(waitCtx, WaitOptions{Condition: request.Condition, Selector: selector}); err != nil {
		// 调用方主动取消或父 context 到期保持原始语义；请求期限和页面
		// OperationTimeout 才转成可重试的 wait_timeout。
		if ctx.Err() != nil {
			return WaitResponse{}, ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) || waitCtx.Err() == context.DeadlineExceeded {
			return WaitResponse{}, waitTimeoutError(request.Condition, request.RefOrSelector)
		}
		return WaitResponse{}, err
	}
	response := WaitResponse{SessionID: current.id, PageID: currentPage.id, Condition: request.Condition, Completed: true}
	return response, nil
}

func refCheckBudget(ctx context.Context) (time.Duration, bool) {
	const maxBudget = 50 * time.Millisecond
	deadline, ok := ctx.Deadline()
	if !ok {
		return maxBudget, true
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 0, false
	}
	budget := remaining / 4
	if budget > maxBudget {
		budget = maxBudget
	}
	// 预算太小时预检本身就可能吃掉整个等待窗口；直接跳过，
	// 让真正的条件检查使用完整剩余时间。
	if budget < 10*time.Millisecond {
		return 0, false
	}
	return budget, true
}

func waitTimeoutError(condition, target string) *Error {
	if target == "" {
		target = "页面加载"
	}
	advice := "确认目标内容是否出现"
	switch condition {
	case "selector_hidden":
		advice = "确认元素是否已经隐藏或从 DOM 移除"
	case "load_complete":
		advice = "确认页面是否仍在加载"
	}
	message := "等待条件 " + condition + " 超时：" + target + "。" + advice
	next := "必要时查看 browserSnapshot；需要更长等待时同时增大 timeout_ms 和浏览器 OperationTimeout"
	return &Error{Kind: "wait_timeout", Message: message, RecommendedNextStep: next, Recoverable: true}
}

// isSnapshotRef 只识别 BrowserKit 生成的 e<token>_<number> 形式。
// 只有这种形状且不在当前快照中的值才会被视为失效 ref；普通 CSS 选择器
// （即使以 e 开头）仍按 selector 处理。
func isSnapshotRef(value string) bool {
	separator := strings.LastIndexByte(value, '_')
	if len(value) < 3 || value[0] != 'e' || separator < 1 || separator == len(value)-1 {
		return false
	}
	for _, char := range value[1:separator] {
		if char < '0' || char > '9' && (char < 'a' || char > 'z') {
			return false
		}
	}
	for _, char := range value[separator+1:] {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

// Evaluate 执行浏览器页面操作，使用普通 context 和明确目标。
func (manager *Manager) Evaluate(ctx context.Context, request EvaluateRequest) (EvaluateResponse, error) {
	operationCtx, cancel, err := operationContext(ctx, request.TimeoutMS)
	if err != nil {
		return EvaluateResponse{}, err
	}
	defer cancel()

	if !manager.config.AllowEvaluate {
		return EvaluateResponse{}, browserError("javascript_blocked", "宿主未启用 Browser JavaScript 能力", "在 Browser 配置中显式设置 allow_evaluate", true)
	}
	if request.SessionID == "" || request.PageID == "" || strings.TrimSpace(request.Code) == "" {
		return EvaluateResponse{}, errors.New("browserEvaluate 需要 session_id、page_id 和 code")
	}
	if len(request.Code) > manager.config.MaxScriptBytes {
		return EvaluateResponse{}, browserError("script_too_large", "JavaScript 代码超过大小限制", "缩短 code 后重试", true)
	}
	current, currentPage, err := manager.get(operationCtx, request.SessionID, request.PageID)
	if err != nil {
		return EvaluateResponse{}, err
	}
	awaitPromise := true
	if request.AwaitPromise != nil {
		awaitPromise = *request.AwaitPromise
	}
	options := EvaluateOptions{AwaitPromise: awaitPromise}
	if request.TimeoutMS > 0 {
		options.Timeout = time.Duration(request.TimeoutMS) * time.Millisecond
	}
	result, err := currentPage.page.Evaluate(operationCtx, request.Code, options)
	if err != nil {
		return EvaluateResponse{}, err
	}
	if len(result.Value) > manager.config.MaxResultBytes {
		return EvaluateResponse{}, browserError("result_too_large", "JavaScript 返回值超过大小限制", "只返回必要的 JSON 摘要", true)
	}
	manager.emit(ctx, Event{Kind: "evaluation_finished", SessionID: current.id, PageID: currentPage.id})
	response := EvaluateResponse{SessionID: current.id, PageID: currentPage.id, Type: result.Type, Value: result.Value, Dialogs: pendingPageDialogs(currentPage.page)}
	return response, nil
}

// Screenshot 执行浏览器页面操作，使用普通 context 和明确目标。
func (manager *Manager) Screenshot(ctx context.Context, request ScreenshotRequest) (ScreenshotResponse, error) {
	if err := contextErr(ctx); err != nil {
		return ScreenshotResponse{}, err
	}
	if err := requirePage(request.SessionID, request.PageID, "browserScreenshot"); err != nil {
		return ScreenshotResponse{}, err
	}
	if request.Grid && request.FullPage {
		return ScreenshotResponse{}, errors.New("grid 截图不支持 full_page；请截取视口或 region")
	}

	current, currentPage, err := manager.get(ctx, request.SessionID, request.PageID)
	if err != nil {
		return ScreenshotResponse{}, err
	}
	screenshot, err := manager.captureScreenshot(ctx, current, currentPage, ScreenshotOptions{FullPage: request.FullPage})
	if err != nil {
		return ScreenshotResponse{}, err
	}
	if request.Grid {
		size := screenshotDimensions(screenshot.Data)
		if size.Width*size.Height > maxGridPixels {
			return ScreenshotResponse{}, errors.New("网格截图尺寸超过限制")
		}
	}
	if request.Region != nil {
		if request.FullPage {
			return ScreenshotResponse{}, errors.New("区域截图不能同时启用 full_page")
		}
		screenshot.Data, err = cropScreenshot(screenshot.Data, *request.Region)
		if err != nil {
			return ScreenshotResponse{}, err
		}
		screenshot.Format = "png"
	}
	path, err := manager.screenshotPath(current.id, currentPage.id, manager.sequence.Add(1))
	if err != nil {
		return ScreenshotResponse{}, err
	}
	size := screenshotDimensions(screenshot.Data)
	width := size.Width
	height := size.Height
	current.mu.Lock()
	viewport := currentPage.viewport
	current.mu.Unlock()
	if request.Grid {
		originX, originY := 0.0, 0.0
		regionWidth := float64(width)
		regionHeight := float64(height)
		if request.Region != nil {
			originX, originY = request.Region.X, request.Region.Y
			regionWidth, regionHeight = request.Region.Width, request.Region.Height
		} else if !request.FullPage && viewport.Width > 0 && viewport.Height > 0 {
			regionWidth = float64(viewport.Width)
			regionHeight = float64(viewport.Height)
		}
		annotated, gridErr := annotateGrid(screenshot.Data, originX, originY, regionWidth, regionHeight)
		if gridErr != nil {
			return ScreenshotResponse{}, gridErr
		}
		screenshot.Data = annotated
	}
	if err := os.WriteFile(path, screenshot.Data, 0o600); err != nil {
		return ScreenshotResponse{}, err
	}
	size = screenshotDimensions(screenshot.Data)
	width = size.Width
	height = size.Height
	manager.emit(ctx, Event{Kind: "screenshot_ready", SessionID: current.id, PageID: currentPage.id})
	return ScreenshotResponse{SessionID: current.id, PageID: currentPage.id, Path: path, Format: screenshot.Format, Bytes: len(screenshot.Data), Data: screenshot.Data, Width: width, Height: height, ViewportWidth: viewport.Width, ViewportHeight: viewport.Height}, nil
}

func validateViewportPoint(viewport Viewport, x, y float64) error {
	if err := validateInputPoint(x, y); err != nil {
		return err
	}
	if viewport.Width > 0 && x >= float64(viewport.Width) {
		return errors.New("坐标超出视口宽度")
	}
	if viewport.Height > 0 && y >= float64(viewport.Height) {
		return errors.New("坐标超出视口高度")
	}
	return nil
}

type imageDimensions struct {
	Width  int
	Height int
}

// screenshotDimensions 读取 PNG 头，避免为尺寸解码整张图片。
func screenshotDimensions(data []byte) imageDimensions {
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return imageDimensions{}
	}
	return imageDimensions{Width: config.Width, Height: config.Height}
}

func cropScreenshot(data []byte, region ScreenshotRegion) ([]byte, error) {
	values := []float64{region.X, region.Y, region.Width, region.Height, region.ViewportWidth, region.ViewportHeight}
	for _, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, errors.New("区域截图坐标无效")
		}
	}
	if region.X < 0 || region.Y < 0 || region.Width <= 0 || region.Height <= 0 || region.ViewportWidth <= 0 || region.ViewportHeight <= 0 || region.X+region.Width > region.ViewportWidth || region.Y+region.Height > region.ViewportHeight {
		return nil, errors.New("区域截图超出页面视口")
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	width, height := decoded.Bounds().Dx(), decoded.Bounds().Dy()
	left := int(math.Floor(region.X / region.ViewportWidth * float64(width)))
	top := int(math.Floor(region.Y / region.ViewportHeight * float64(height)))
	right := int(math.Ceil((region.X + region.Width) / region.ViewportWidth * float64(width)))
	bottom := int(math.Ceil((region.Y + region.Height) / region.ViewportHeight * float64(height)))
	if left < 0 || top < 0 || right > width || bottom > height || left >= right || top >= bottom {
		return nil, errors.New("区域截图像素范围无效")
	}
	part := image.NewRGBA(image.Rect(0, 0, right-left, bottom-top))
	for y := top; y < bottom; y++ {
		for x := left; x < right; x++ {
			part.Set(x-left, y-top, decoded.At(decoded.Bounds().Min.X+x, decoded.Bounds().Min.Y+y))
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, part); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

// captureScreenshot 保留 Manager 声明的 active Tab。Chrome 会在截图前
// 激活 target，后台 Tab 截图后必须恢复原 target，
// 否则后续直播与输入会和 Manager.activeTab 分叉。
func (manager *Manager) captureScreenshot(ctx context.Context, current *session, currentPage *page, options ScreenshotOptions) (Screenshot, error) {
	current.tabMu.Lock()
	defer current.tabMu.Unlock()
	current.mu.Lock()
	active := manager.activePageLocked(current)
	browser := current.browser
	current.mu.Unlock()
	screenshot, screenshotErr := currentPage.page.Screenshot(ctx, options)
	if active != currentPage {
		if err := restoreActiveTarget(current, browser, active); err != nil && screenshotErr == nil {
			return Screenshot{}, browserError("tab_restore_failed", "截图后恢复活动 Tab 失败: "+err.Error(), "重新获取 tablist 后重试", true)
		}
	}
	return screenshot, screenshotErr
}

// CloseSession 执行浏览器页面操作，使用普通 context 和明确目标。
func (manager *Manager) CloseSession(ctx context.Context, request CloseRequest) (CloseResponse, error) {
	if err := contextErr(ctx); err != nil {
		return CloseResponse{}, err
	}

	if request.SessionID == "" {
		return CloseResponse{}, errors.New("browserClose 需要 session_id")
	}
	manager.mu.Lock()
	current := manager.sessions[request.SessionID]
	if current == nil {
		_, closed := manager.closed[request.SessionID]
		manager.mu.Unlock()
		if closed {
			return CloseResponse{SessionID: request.SessionID, Closed: true}, nil
		}
		return CloseResponse{}, browserError("session_not_found", "浏览器 session 不存在", "确认 session_id 后重试", true)
	}
	delete(manager.sessions, request.SessionID)
	manager.closed[request.SessionID] = struct{}{}
	if manager.active == request.SessionID {
		manager.active = ""
	}
	manager.mu.Unlock()
	// 逻辑关闭已经完成；底层 Browser.Close 即使超时或失败，也不能让
	// 状态观察者继续显示已经失效的 Tab 和截图。
	manager.emit(ctx, Event{Kind: "session_closed", SessionID: current.id})
	if err := manager.closeSession(ctx, current); err != nil {
		return CloseResponse{}, err
	}
	return CloseResponse{SessionID: current.id, Closed: true}, nil
}

func waitContext(ctx context.Context, duration time.Duration) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	if duration <= 0 {
		return nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// requirePage 用于仍要求显式目标的页面工具。browserSnapshot 是例外：
// 它先 reconcile，再把省略 page_id 绑定到最新 active Tab。
func requirePage(sessionID, pageID, operation string) error {
	if sessionID == "" {
		return errors.New(operation + " 需要 session_id 和 page_id")
	}
	if pageID == "" {
		return errors.New(operation + " 需要 session_id 和 page_id；不能省略 page_id")
	}
	return nil
}

func rememberSnapshotRefs(value *page, snapshot Snapshot) {
	if value == nil {
		return
	}
	if value.refs == nil || value.refRevision != snapshot.Revision {
		value.refs = make(map[string]struct{})
		value.elements = make(map[string]Element)
		value.refRevision = snapshot.Revision
	}
	if snapshot.ActiveRefs != nil {
		active := make(map[string]struct{}, len(snapshot.ActiveRefs))
		for _, ref := range snapshot.ActiveRefs {
			active[ref] = struct{}{}
		}
		for ref := range value.refs {
			if _, ok := active[ref]; !ok {
				delete(value.refs, ref)
				delete(value.elements, ref)
			}
		}
	}
	for _, element := range snapshot.Elements {
		if element.Ref != "" {
			value.refs[element.Ref] = struct{}{}
			value.elements[element.Ref] = element
		}
	}
}

func clearPageRefs(value *page) {
	if value == nil {
		return
	}
	value.refs = make(map[string]struct{})
	value.elements = make(map[string]Element)
	value.refRevision = 0
}

func validatePageRef(value *page, ref string, revision uint64) error {
	if value == nil || value.refs == nil {
		return browserError("invalid_reference", "当前 Tab 没有该元素引用", "在目标 page_id 上重新调用 browserSnapshot", true)
	}
	_, ok := value.refs[ref]
	if !ok {
		return browserError("invalid_reference", "当前 Tab 没有该元素引用", "确认 ref 与 page_id 来自同一次 browserSnapshot", true)
	}
	if value.refRevision != revision {
		return browserError("stale_reference", "元素引用已经过期", "在目标 page_id 上重新调用 browserSnapshot", true)
	}
	return nil
}

// operationContext 是请求级 timeout 的唯一入口。BrowserKit 仍会在
// page/session 内叠加配置的 OperationTimeout，最终期限取调用方和两者中更短的一个。
func operationContext(ctx context.Context, timeoutMS int) (context.Context, context.CancelFunc, error) {
	if err := contextErr(ctx); err != nil {
		return nil, func() {}, err
	}
	if timeoutMS < 0 {
		return nil, func() {}, errors.New("timeout_ms 不能为负数")
	}
	if timeoutMS == 0 {
		return ctx, func() {}, nil
	}
	operationCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMS)*time.Millisecond)
	return operationCtx, cancel, nil
}
