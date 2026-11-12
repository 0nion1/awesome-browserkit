package browserkit

import (
	"context"
	"errors"
)

// skipActionSnapshotKey 只用于 browserAct 内部：批量步骤不需要每一步
// 都扫描一次 DOM，批次结束时会统一读取一次最终快照。
type skipActionSnapshotKey struct{}

func withoutActionSnapshot(ctx context.Context) context.Context {
	return context.WithValue(ctx, skipActionSnapshotKey{}, true)
}

func (manager *Manager) actionSnapshot(ctx context.Context, current *session, currentPage *page) (*SnapshotResponse, string) {
	if skip, _ := ctx.Value(skipActionSnapshotKey{}).(bool); skip {
		if snapshot, ok := manager.pendingDialogSnapshot(current, currentPage); ok {
			return &snapshot, ""
		}
		return nil, ""
	}
	return manager.interactionSnapshot(ctx, current, currentPage)
}

// interactionSnapshot 只观察已完成的操作，读取失败不会把外部副作用变成可重放失败。
// 调用前须释放 session 交互锁并同步 Tab，避免与切换页面、画面流形成锁反转。
func (manager *Manager) interactionSnapshot(ctx context.Context, current *session, currentPage *page) (*SnapshotResponse, string) {
	if snapshot, ok := manager.pendingDialogSnapshot(current, currentPage); ok {
		return &snapshot, ""
	}
	readCtx, cancel, err := contextWithTimeout(ctx, manager.config.Session.OperationTimeout)
	if err != nil {
		return nil, err.Error()
	}
	defer cancel()
	changes := dialogChanges(currentPage.page)
	if snapshot, ok := manager.pendingDialogSnapshot(current, currentPage); ok {
		return &snapshot, ""
	}
	type evaluation struct{ err error }
	evaluated := make(chan evaluation, 1)
	go func() {
		_, evaluateErr := currentPage.page.Evaluate(readCtx, `new Promise(resolve => {
		if (document.visibilityState === 'hidden') { resolve(null); return; }
		requestAnimationFrame(() => resolve(null));
	})`, EvaluateOptions{AwaitPromise: true})
		evaluated <- evaluation{err: evaluateErr}
	}()
	select {
	case result := <-evaluated:
		err = result.err
	case <-changes:
		cancel()
		if snapshot, ok := manager.pendingDialogSnapshot(current, currentPage); ok {
			return &snapshot, ""
		}
		err = errors.New("页面弹窗状态发生变化")
	case <-readCtx.Done():
		err = readCtx.Err()
	}
	if err != nil {
		if snapshot, ok := manager.pendingDialogSnapshot(current, currentPage); ok {
			return &snapshot, ""
		}
		return nil, err.Error()
	}
	snapshot, err := manager.snapshotPage(readCtx, current, currentPage, SnapshotOptions{})
	if err != nil {
		return nil, err.Error()
	}
	compactInteractionSnapshot(&snapshot)
	return &snapshot, ""
}

func pendingPageDialog(value Page) *Dialog {
	if reporter, ok := value.(DialogPage); ok {
		return reporter.PendingDialog()
	}
	return nil
}

func pendingPageDialogs(value Page) []Dialog {
	dialog := pendingPageDialog(value)
	if dialog == nil {
		return nil
	}
	return []Dialog{*dialog}
}

func dialogChanges(value Page) <-chan struct{} {
	if reporter, ok := value.(DialogPage); ok {
		return reporter.DialogChanges()
	}
	return nil
}

// interactionTextLimit 限制动作后自动快照的正文长度。动作结果会进入
// 后续每一轮上下文；完整正文应由显式 browserSnapshot 获取。
const interactionTextLimit = 2000

func compactInteractionSnapshot(snapshot *SnapshotResponse) {
	if snapshot == nil {
		return
	}
	runes := []rune(snapshot.Text)
	if len(runes) > interactionTextLimit {
		// 长页面通常把结果追加在末尾；同时保留开头和结尾，避免模型
		// 为了看结果再调用一次完整快照。
		head := interactionTextLimit * 7 / 10
		tail := interactionTextLimit - head
		snapshot.Text = string(runes[:head]) + "…" + string(runes[len(runes)-tail:])
		snapshot.TextTruncated = true
		snapshot.Truncated = true
	}
	// Tab 列表通常没有变化；重复返回会让每个动作结果都携带同一份投影。
	if !snapshot.TabsChanged {
		snapshot.Tabs = nil
	}
}

// SetChecked 把快照中的可勾选控件设置为目标状态，已满足时不点击。
func (manager *Manager) SetChecked(ctx context.Context, request SetCheckedRequest) (SetCheckedResponse, error) {
	if err := contextErr(ctx); err != nil {
		return SetCheckedResponse{}, err
	}
	if err := requirePage(request.SessionID, request.PageID, "browserSetChecked"); err != nil {
		return SetCheckedResponse{}, err
	}
	if request.Ref == "" || request.Revision == 0 {
		return SetCheckedResponse{}, errors.New("browserSetChecked 需要 ref 和 revision")
	}
	current, currentPage, err := manager.get(ctx, request.SessionID, request.PageID)
	if err != nil {
		return SetCheckedResponse{}, err
	}
	unlock, err := lockSessionInteraction(ctx, current)
	if err != nil {
		return SetCheckedResponse{}, err
	}
	defer unlock()
	current.mu.Lock()
	before := tabsRevisionLocked(current)
	refErr := validatePageRef(currentPage, request.Ref, request.Revision)
	current.mu.Unlock()
	if refErr != nil {
		return SetCheckedResponse{}, refErr
	}
	changed, err := currentPage.page.SetChecked(ctx, ElementRef{Ref: request.Ref, Revision: request.Revision}, request.Checked)
	if err != nil {
		return SetCheckedResponse{}, err
	}
	manager.emit(ctx, Event{Kind: "interaction_finished", SessionID: current.id, PageID: currentPage.id})
	unlock()
	active, revision, tabsChanged := manager.tabChange(ctx, current, before)
	response := SetCheckedResponse{SessionID: current.id, PageID: currentPage.id, Ref: request.Ref, Checked: request.Checked, Changed: changed, ActivePageID: active, TabsRevision: revision, TabsChanged: tabsChanged}
	response.Snapshot, response.SnapshotError = manager.actionSnapshot(ctx, current, currentPage)
	return response, nil
}

// Drag 在同一页面内执行元素拖放或按视口位移拖拽。
func (manager *Manager) Drag(ctx context.Context, request DragRequest) (DragResponse, error) {
	if err := contextErr(ctx); err != nil {
		return DragResponse{}, err
	}
	if err := requirePage(request.SessionID, request.PageID, "browserDrag"); err != nil {
		return DragResponse{}, err
	}
	sourceRef := request.SourceRef != ""
	sourcePoint := request.FromX != nil && request.FromY != nil
	if (request.FromX == nil) != (request.FromY == nil) {
		return DragResponse{}, errors.New("browserDrag 的 from_x 和 from_y 必须同时填写")
	}
	if sourceRef && sourcePoint {
		return DragResponse{}, errors.New("browserDrag 的 source_ref 与 from_x/from_y 只能二选一")
	}
	if !sourceRef && !sourcePoint {
		return DragResponse{}, errors.New("browserDrag 需要 source_ref 或 from_x/from_y")
	}
	if sourceRef && request.Revision == 0 {
		return DragResponse{}, errors.New("browserDrag 使用 source_ref 时需要 revision")
	}
	targetRef := request.TargetRef != ""
	delta := request.DeltaX != 0 || request.DeltaY != 0
	if targetRef && delta {
		return DragResponse{}, errors.New("browserDrag 的 target_ref 与 delta_x/delta_y 只能二选一")
	}
	if !targetRef && !delta {
		return DragResponse{}, errors.New("browserDrag 需要 target_ref 或非零 delta_x/delta_y")
	}
	if delta {
		if err := validateDragDelta(request.DeltaX, request.DeltaY); err != nil {
			return DragResponse{}, err
		}
	}
	if sourcePoint && targetRef {
		return DragResponse{}, browserError("drag_point_target_unsupported", "坐标起点暂不支持元素目标", "同时提供 source_ref 和 target_ref，或改用 delta_x/delta_y", true)
	}
	if sourceRef && targetRef && request.SourceRef == request.TargetRef {
		return DragResponse{}, errors.New("browserDrag 的源和目标不能相同")
	}

	current, currentPage, err := manager.get(ctx, request.SessionID, request.PageID)
	if err != nil {
		return DragResponse{}, err
	}
	if sourcePoint {
		current.mu.Lock()
		viewport := currentPage.viewport
		current.mu.Unlock()
		if err := validateViewportPoint(viewport, *request.FromX, *request.FromY); err != nil {
			return DragResponse{}, err
		}
		if err := validateViewportPoint(viewport, *request.FromX+request.DeltaX, *request.FromY+request.DeltaY); err != nil {
			return DragResponse{}, err
		}
	}
	unlock, err := lockSessionInteraction(ctx, current)
	if err != nil {
		return DragResponse{}, err
	}
	defer unlock()
	current.mu.Lock()
	before := tabsRevisionLocked(current)
	refErr := error(nil)
	if sourceRef {
		refErr = validatePageRef(currentPage, request.SourceRef, request.Revision)
	}
	targetKnown := false
	if targetRef {
		_, targetKnown = currentPage.refs[request.TargetRef]
	}
	current.mu.Unlock()
	if refErr != nil {
		return DragResponse{}, refErr
	}
	if delta {
		if sourceRef {
			dragger, ok := currentPage.page.(DragByPage)
			if !ok {
				return DragResponse{}, browserError("drag_by_unsupported", "当前浏览器页面不支持按位移拖拽", "使用 target_ref 或实现 DragByPage", true)
			}
			if err := dragger.DragBy(ctx, ElementRef{Ref: request.SourceRef, Revision: request.Revision}, request.DeltaX, request.DeltaY); err != nil {
				return DragResponse{}, err
			}
		} else {
			dragger, ok := currentPage.page.(DragPointPage)
			if !ok {
				return DragResponse{}, browserError("drag_point_unsupported", "当前浏览器页面不支持按坐标拖拽", "使用 source_ref 或实现 DragPointPage", true)
			}
			if err := dragger.DragPoint(ctx, *request.FromX, *request.FromY, request.DeltaX, request.DeltaY); err != nil {
				return DragResponse{}, err
			}
		}
	} else {
		targetIsRef := targetKnown || isSnapshotRef(request.TargetRef)
		if targetIsRef {
			current.mu.Lock()
			refErr = validatePageRef(currentPage, request.TargetRef, request.Revision)
			current.mu.Unlock()
			if refErr != nil {
				return DragResponse{}, refErr
			}
			if err := currentPage.page.Drag(ctx, ElementRef{Ref: request.SourceRef, Revision: request.Revision}, ElementRef{Ref: request.TargetRef, Revision: request.Revision}); err != nil {
				return DragResponse{}, err
			}
		} else {
			dragger, ok := currentPage.page.(DragTargetPage)
			if !ok {
				return DragResponse{}, browserError("drag_selector_unsupported", "当前浏览器页面不支持用 selector 指定拖放目标", "使用快照中的 target_ref，或实现 DragTargetPage", true)
			}
			if err := dragger.DragTo(ctx, ElementRef{Ref: request.SourceRef, Revision: request.Revision}, request.TargetRef); err != nil {
				return DragResponse{}, err
			}
		}
	}
	manager.emit(ctx, Event{Kind: "interaction_finished", SessionID: current.id, PageID: currentPage.id})
	unlock()
	active, revision, tabsChanged := manager.tabChange(ctx, current, before)
	response := DragResponse{SessionID: current.id, PageID: currentPage.id, SourceRef: request.SourceRef, TargetRef: request.TargetRef, DeltaX: request.DeltaX, DeltaY: request.DeltaY, ActivePageID: active, TabsRevision: revision, TabsChanged: tabsChanged}
	response.Snapshot, response.SnapshotError = manager.actionSnapshot(ctx, current, currentPage)
	if request.IncludeScreenshot {
		shot, shotErr := manager.Screenshot(ctx, ScreenshotRequest{SessionID: request.SessionID, PageID: currentPage.id})
		if shotErr != nil {
			response.ScreenshotError = shotErr.Error()
		} else {
			response.Screenshot = &shot
		}
	}
	return response, nil
}
