package browserkit

import (
	"context"
	"errors"
	"math"
	"time"
)

// Act 在一个调用内按顺序执行一组动作，只在最后返回一次页面快照。
// 任一步失败会保留已完成步骤并停止，避免把后续动作建立在错误页面上。
func (manager *Manager) Act(ctx context.Context, request ActRequest) (ActResponse, error) {
	if err := contextErr(ctx); err != nil {
		return ActResponse{}, err
	}
	if err := requirePage(request.SessionID, request.PageID, "browserAct"); err != nil {
		return ActResponse{}, err
	}
	if len(request.Steps) == 0 {
		return ActResponse{}, errors.New("browserAct 至少需要一个步骤")
	}
	if len(request.Steps) > 20 {
		return ActResponse{}, errors.New("browserAct 最多支持 20 个步骤")
	}
	needsRevision := false
	for _, step := range request.Steps {
		if actStepNeedsRevision(step) {
			needsRevision = true
			break
		}
	}
	if needsRevision && request.Revision == 0 {
		return ActResponse{}, errors.New("browserAct 使用快照 ref 时需要 revision")
	}
	// 在执行任何有副作用的步骤前完成整批预检，避免后面的非法步骤
	// 让前面的点击或输入已经发生。
	for index, step := range request.Steps {
		err := validateActStep(step)
		if err == nil && step.Op == "evaluate" && !manager.config.AllowEvaluate {
			err = newError("javascript_blocked", "宿主未启用 Browser JavaScript 能力", "在 Browser 配置中显式设置 allow_evaluate", true)
		}
		if err != nil {
			result := ActStepResult{Index: index, Op: step.Op}
			fillStepError(&result, err, ctx.Err() != nil)
			return ActResponse{SessionID: request.SessionID, PageID: request.PageID, FailedStep: index + 1, Steps: []ActStepResult{result}}, nil
		}
	}
	if index, err := manager.preflightActRefs(ctx, request); err != nil {
		if index < 0 {
			return ActResponse{}, err
		}
		result := ActStepResult{Index: index, Op: request.Steps[index].Op}
		fillStepError(&result, err, ctx.Err() != nil)
		return ActResponse{SessionID: request.SessionID, PageID: request.PageID, FailedStep: index + 1, Steps: []ActStepResult{result}}, nil
	}
	if index, err := manager.preflightActPoints(ctx, request); err != nil {
		if index < 0 {
			return ActResponse{}, err
		}
		result := ActStepResult{Index: index, Op: request.Steps[index].Op}
		fillStepError(&result, err, ctx.Err() != nil)
		return ActResponse{SessionID: request.SessionID, PageID: request.PageID, FailedStep: index + 1, Steps: []ActStepResult{result}}, nil
	}
	initial, _, err := manager.get(ctx, request.SessionID, request.PageID)
	if err != nil {
		return ActResponse{}, err
	}
	initial.mu.Lock()
	beforeActive := initial.activeTab
	initial.mu.Unlock()
	response := ActResponse{SessionID: request.SessionID, PageID: request.PageID, Steps: make([]ActStepResult, 0, len(request.Steps))}
	for index, step := range request.Steps {
		result := ActStepResult{Index: index, Op: step.Op}
		dialogs, err := manager.runActStep(withoutActionSnapshot(ctx), request, step, &result)
		response.Dialogs = append(response.Dialogs, dialogs...)
		if err == nil && len(dialogs) > 0 {
			err = newError("dialog_open", "页面弹窗等待处理", "使用 browserSnapshot 返回的 dialog id 调用 browserHandleDialog，然后再继续后续动作", true)
		}
		result.Success = err == nil
		if err != nil {
			fillStepError(&result, err, ctx.Err() != nil)
			if step.Op == "click" && result.Kind == "step_timeout" {
				// 派发阶段超时时无法确认点击是否已经发生。
				result.RecommendedNextStep = "点击可能已经派发；先 browserSnapshot 或 browserWait 检查页面，不要直接重放点击"
			}
			response.Steps = append(response.Steps, result)
			response.FailedStep = index + 1
			if ctx.Err() != nil {
				response.Cancelled = true
				return response, ctx.Err()
			}
			break
		}
		response.Completed++
		response.Steps = append(response.Steps, result)
	}
	if ctx.Err() != nil {
		response.Cancelled = true
		return response, ctx.Err()
	}
	current, currentPage, err := manager.get(ctx, request.SessionID, request.PageID)
	if err != nil {
		response.SnapshotError = err.Error()
		if ctx.Err() != nil {
			response.Cancelled = true
			return response, ctx.Err()
		}
		return response, nil
	}
	if tabs, tabsErr := manager.ListTabs(ctx, TabsRequest{SessionID: request.SessionID}); tabsErr == nil {
		response.ActivePageID = tabs.ActivePageID
		response.TabsRevision = tabs.TabsRevision
		response.Tabs = tabs.Tabs
	} else if ctx.Err() != nil {
		response.Cancelled = true
		return response, ctx.Err()
	}
	snapshotPageID := request.PageID
	if response.ActivePageID != "" && response.ActivePageID != beforeActive && response.ActivePageID != request.PageID {
		snapshotPageID = response.ActivePageID
	}
	snapshotCurrent, snapshotPage := current, currentPage
	if snapshotPageID != request.PageID {
		snapshotCurrent, snapshotPage, err = manager.get(ctx, request.SessionID, snapshotPageID)
		if err != nil {
			response.SnapshotError = err.Error()
			if ctx.Err() != nil {
				response.Cancelled = true
				return response, ctx.Err()
			}
		}
	}
	if snapshotPage != nil && snapshotPage.page != nil {
		response.Snapshot, response.SnapshotError = manager.interactionSnapshot(ctx, snapshotCurrent, snapshotPage)
		if response.Snapshot != nil {
			if len(response.Snapshot.Dialogs) > 0 {
				response.Dialogs = append([]Dialog(nil), response.Snapshot.Dialogs...)
			}
		}
	}
	if ctx.Err() != nil {
		response.Cancelled = true
		return response, ctx.Err()
	}
	if request.IncludeScreenshot {
		pageID := request.PageID
		if response.ActivePageID != "" && response.ActivePageID != beforeActive && response.ActivePageID != request.PageID {
			pageID = response.ActivePageID
		}
		shot, shotErr := manager.Screenshot(ctx, ScreenshotRequest{SessionID: request.SessionID, PageID: pageID})
		if shotErr != nil {
			response.ScreenshotError = shotErr.Error()
		} else {
			response.Screenshot = &shot
		}
	}
	return response, nil
}

func actStepNeedsRevision(step ActStep) bool {
	switch step.Op {
	case "click":
		return step.Ref != ""
	case "hover", "type", "select", "check":
		return true
	case "press":
		return step.Ref != ""
	case "drag":
		return step.SourceRef != ""
	case "wait":
		return (step.Condition == "selector_visible" || step.Condition == "selector_hidden") && isSnapshotRef(step.RefOrSelector)
	}
	return false
}

func validateActStep(step ActStep) error {
	if step.TimeoutMS < 0 || step.WaitAfterMS < 0 {
		return newError("invalid_step", "超时和等待时间不能为负数", "省略或填写非负毫秒数", true)
	}
	needRef := func() error {
		if step.Ref == "" {
			return newError("invalid_step", step.Op+" 需要 ref", "使用同一次 browserSnapshot 返回的 ref", true)
		}
		return nil
	}
	switch step.Op {
	case "click":
		if step.Ref == "" {
			if step.X == nil || step.Y == nil {
				return newError("invalid_step", "click 需要 ref 或 x/y", "没有 ref 时填写视口 CSS 坐标", true)
			}
		} else if step.X != nil || step.Y != nil {
			return newError("invalid_step", "click 的 ref 与 x/y 只能二选一", "只保留 ref 或坐标", true)
		}
		if step.Button != "" && step.Button != "left" && step.Button != "right" && step.Button != "middle" {
			return newError("invalid_step", "click 的 button 无效", "使用 left、right 或 middle", true)
		}
		if step.ClickCount < 0 || step.ClickCount > 2 {
			return newError("invalid_step", "click_count 必须是 1 或 2", "省略或填写 1、2", true)
		}
	case "hover", "type":
		return needRef()
	case "select":
		if err := needRef(); err != nil {
			return err
		}
		if step.Value == "" && step.Label == "" {
			return newError("invalid_step", "select 需要 value 或 label", "二选一填写选项值或可见标签", true)
		}
		if step.Value != "" && step.Label != "" {
			return newError("invalid_step", "select 的 value 和 label 只能二选一", "只保留其中一个条件", true)
		}
	case "check":
		if err := needRef(); err != nil {
			return err
		}
		if step.Checked == nil {
			return newError("invalid_step", "check 需要 checked", "显式设置 true 或 false", true)
		}
	case "press":
		if step.Key == "" {
			return newError("invalid_step", "press 需要 key", "填写按键名称", true)
		}
		seen := map[string]bool{}
		for _, modifier := range step.Modifiers {
			if modifier != "Control" && modifier != "Meta" && modifier != "Alt" && modifier != "Shift" {
				return newError("invalid_step", "press 的 modifier 无效", "使用 Control、Meta、Alt 或 Shift", true)
			}
			if seen[modifier] {
				return newError("invalid_step", "press 的 modifier 重复", "每个修饰键最多出现一次", true)
			}
			seen[modifier] = true
		}
	case "scroll":
		if math.IsNaN(step.DeltaX) || math.IsNaN(step.DeltaY) || math.IsInf(step.DeltaX, 0) || math.IsInf(step.DeltaY, 0) || math.Abs(step.DeltaX) > 10000 || math.Abs(step.DeltaY) > 10000 {
			return newError("invalid_step", "scroll 距离超出范围", "绝对值不超过 10000", true)
		}
		if step.DeltaX == 0 && step.DeltaY == 0 {
			return newError("invalid_step", "scroll 需要非零 delta_x 或 delta_y", "填写滚动距离", true)
		}
	case "wait":
		switch step.Condition {
		case "selector_visible", "selector_hidden", "url_contains", "title_contains", "text_contains", "load_complete":
		default:
			return newError("invalid_step", "wait condition 无效", "使用 selector_visible、selector_hidden、url_contains、title_contains、text_contains 或 load_complete", true)
		}
		if step.Condition != "load_complete" && step.RefOrSelector == "" {
			return newError("invalid_step", "wait 需要 ref_or_selector", "填写 CSS selector、ref 或匹配文本", true)
		}
	case "drag":
		sourcePoint := step.X != nil && step.Y != nil
		if (step.X == nil) != (step.Y == nil) {
			return newError("invalid_step", "drag 的 x/y 必须同时填写", "填写完整的视口 CSS 坐标", true)
		}
		if step.SourceRef != "" && sourcePoint {
			return newError("invalid_step", "drag 的 source_ref 与 x/y 只能二选一", "源点二选一", true)
		}
		if step.SourceRef == "" && !sourcePoint {
			return newError("invalid_step", "drag 需要 source_ref 或 x/y", "填写源点", true)
		}
		targetPoint := step.DeltaX != 0 || step.DeltaY != 0
		if step.TargetRef != "" && targetPoint {
			return newError("invalid_step", "drag 的 target_ref 与 delta_x/delta_y 只能二选一", "目标二选一", true)
		}
		if step.TargetRef == "" && !targetPoint {
			return newError("invalid_step", "drag 需要 target_ref 或 delta_x/delta_y", "填写目标", true)
		}
		if targetPoint {
			if math.IsNaN(step.DeltaX) || math.IsNaN(step.DeltaY) || math.IsInf(step.DeltaX, 0) || math.IsInf(step.DeltaY, 0) || math.Abs(step.DeltaX) > 10000 || math.Abs(step.DeltaY) > 10000 {
				return newError("invalid_step", "drag 位移超出范围", "绝对值不超过 10000", true)
			}
		}
		if sourcePoint && step.TargetRef != "" {
			return newError("invalid_step", "drag 坐标起点暂不支持 target_ref", "改用 delta_x/delta_y，或提供 source_ref", true)
		}
		if step.SourceRef != "" && step.TargetRef != "" && step.SourceRef == step.TargetRef {
			return newError("invalid_step", "drag 的源和目标不能相同", "选择两个不同的元素", true)
		}
	case "evaluate":
		if step.Code == "" {
			return newError("invalid_step", "evaluate 需要 code", "填写要执行的 JavaScript；需要宿主启用 allow_evaluate", true)
		}
	default:
		return newError("invalid_step", "未知的 browserAct 动作", "使用 click、hover、type、select、check、press、scroll、wait、drag 或 evaluate", true)
	}
	return nil
}

func fillStepError(result *ActStepResult, err error, parentDone bool) {
	if result == nil || err == nil {
		return
	}
	result.Error = err.Error()
	// 步骤直接返回结构化错误；解释类型不能交给原生 errors.As 校验。
	if browserErr, ok := err.(*Error); ok {
		result.Kind = browserErr.Kind
		result.Recoverable = browserErr.Recoverable
		result.RecommendedNextStep = browserErr.RecommendedNextStep
		return
	}
	if !parentDone && errors.Is(err, context.DeadlineExceeded) {
		result.Kind = "step_timeout"
		result.Recoverable = true
		result.RecommendedNextStep = "检查页面状态；需要更长等待时增大 timeout_ms 或浏览器 OperationTimeout"
	}
}

func dialogsOf(snapshot *SnapshotResponse) []Dialog {
	if snapshot == nil {
		return nil
	}
	return snapshot.Dialogs
}

func (manager *Manager) runActStep(ctx context.Context, request ActRequest, step ActStep, result *ActStepResult) ([]Dialog, error) {
	parentCtx := ctx
	if step.TimeoutMS > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(step.TimeoutMS)*time.Millisecond)
		defer cancel()
	}
	needRef := func() error {
		if step.Ref == "" {
			return newError("invalid_step", step.Op+" 需要 ref", "使用同一次 browserSnapshot 返回的 ref", true)
		}
		return nil
	}
	switch step.Op {
	case "click":
		clickRevision := request.Revision
		if step.Ref == "" {
			clickRevision = 0
		}
		response, err := manager.Click(ctx, ClickRequest{SessionID: request.SessionID, PageID: request.PageID, Ref: step.Ref, Revision: clickRevision, X: step.X, Y: step.Y, Button: step.Button, ClickCount: step.ClickCount, WaitAfterMS: step.WaitAfterMS, TimeoutMS: step.TimeoutMS})
		result.Changed = response.Changed
		if response.SnapshotError != "" {
			result.Warning = "click 已执行，wait_after_ms 未完成（" + response.SnapshotError + "）；页面可能仍在加载，后续步骤可能运行在未稳定页面上，不要重放点击，可继续 browserWait 或 browserSnapshot 检查"
		}
		return dialogsOf(response.Snapshot), err
	case "hover":
		if err := needRef(); err != nil {
			return nil, err
		}
		response, err := manager.Hover(ctx, HoverRequest{SessionID: request.SessionID, PageID: request.PageID, Ref: step.Ref, Revision: request.Revision})
		return dialogsOf(response.Snapshot), err
	case "type":
		if err := needRef(); err != nil {
			return nil, err
		}
		response, err := manager.Type(ctx, TypeRequest{SessionID: request.SessionID, PageID: request.PageID, Ref: step.Ref, Revision: request.Revision, Text: step.Text, Append: step.Append, Submit: step.Submit})
		result.TextLength = response.TextLength
		return dialogsOf(response.Snapshot), err
	case "select":
		if err := needRef(); err != nil {
			return nil, err
		}
		response, err := manager.SelectOption(ctx, SelectOptionRequest{SessionID: request.SessionID, PageID: request.PageID, Ref: step.Ref, Revision: request.Revision, Value: step.Value, Label: step.Label})
		result.Value = response.Value
		return dialogsOf(response.Snapshot), err
	case "check":
		if err := needRef(); err != nil {
			return nil, err
		}
		response, err := manager.SetChecked(ctx, SetCheckedRequest{SessionID: request.SessionID, PageID: request.PageID, Ref: step.Ref, Revision: request.Revision, Checked: *step.Checked})
		result.Changed = response.Changed
		return dialogsOf(response.Snapshot), err
	case "press":
		pageRevision := request.Revision
		if step.Ref == "" {
			pageRevision = 0
		}
		response, err := manager.Press(ctx, PressRequest{SessionID: request.SessionID, PageID: request.PageID, Key: step.Key, Modifiers: step.Modifiers, Ref: step.Ref, Revision: pageRevision})
		return dialogsOf(response.Snapshot), err
	case "scroll":
		response, err := manager.Scroll(ctx, ScrollRequest{SessionID: request.SessionID, PageID: request.PageID, DeltaX: step.DeltaX, DeltaY: step.DeltaY, X: step.X, Y: step.Y})
		return dialogsOf(response.Snapshot), err
	case "wait":
		// wait 的 ref_or_selector 既可能是 ref 也可能是 CSS selector；
		// 只有形如快照 ref 的值才携带 revision 做过期校验。
		revision := uint64(0)
		if (step.Condition == "selector_visible" || step.Condition == "selector_hidden") && isSnapshotRef(step.RefOrSelector) {
			revision = request.Revision
		}
		_, err := manager.Wait(ctx, WaitRequest{SessionID: request.SessionID, PageID: request.PageID, Condition: step.Condition, RefOrSelector: step.RefOrSelector, Revision: revision, TimeoutMS: step.TimeoutMS})
		if err != nil && step.TimeoutMS > 0 && parentCtx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return nil, waitTimeoutError(step.Condition, step.RefOrSelector)
		}
		return nil, err
	case "drag":
		response, err := manager.Drag(ctx, DragRequest{SessionID: request.SessionID, PageID: request.PageID, SourceRef: step.SourceRef, FromX: step.X, FromY: step.Y, TargetRef: step.TargetRef, DeltaX: step.DeltaX, DeltaY: step.DeltaY, Revision: request.Revision})
		return dialogsOf(response.Snapshot), err
	case "evaluate":
		response, err := manager.Evaluate(ctx, EvaluateRequest{SessionID: request.SessionID, PageID: request.PageID, Code: step.Code, TimeoutMS: step.TimeoutMS, AwaitPromise: step.AwaitPromise})
		result.Result = response.Value
		result.Type = response.Type
		return response.Dialogs, err
	default:
		return nil, newError("invalid_step", "未知的 browserAct 动作", "使用 click、hover、type、select、check、press、scroll、wait、drag 或 evaluate", true)
	}
}
func (manager *Manager) preflightActPoints(ctx context.Context, request ActRequest) (int, error) {
	current, currentPage, err := manager.get(ctx, request.SessionID, request.PageID)
	if err != nil {
		return -1, err
	}
	current.mu.Lock()
	viewport := currentPage.viewport
	current.mu.Unlock()
	for index, step := range request.Steps {
		switch step.Op {
		case "click":
			if step.Ref == "" && step.X != nil && step.Y != nil {
				if err := validateViewportPoint(viewport, *step.X, *step.Y); err != nil {
					return index, err
				}
			}
		case "drag":
			if step.X == nil || step.Y == nil {
				continue
			}
			if err := validateViewportPoint(viewport, *step.X, *step.Y); err != nil {
				return index, err
			}
			if step.DeltaX != 0 || step.DeltaY != 0 {
				if err := validateViewportPoint(viewport, *step.X+step.DeltaX, *step.Y+step.DeltaY); err != nil {
					return index, err
				}
			}
		}
	}
	return 0, nil
}

func (manager *Manager) preflightActRefs(ctx context.Context, request ActRequest) (int, error) {
	current, currentPage, err := manager.get(ctx, request.SessionID, request.PageID)
	if err != nil {
		return -1, err
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	for index, step := range request.Steps {
		for _, ref := range actStepRefs(step, currentPage) {
			if err := validatePageRef(currentPage, ref, request.Revision); err != nil {
				return index, err
			}
		}
	}
	return 0, nil
}

func actStepRefs(step ActStep, currentPage *page) []string {
	refs := []string{}
	switch step.Op {
	case "click":
		if step.Ref != "" {
			refs = append(refs, step.Ref)
		}
	case "hover", "type", "select", "check":
		refs = append(refs, step.Ref)
	case "press":
		if step.Ref != "" {
			refs = append(refs, step.Ref)
		}
	case "wait":
		if (step.Condition == "selector_visible" || step.Condition == "selector_hidden") && isSnapshotRef(step.RefOrSelector) {
			refs = append(refs, step.RefOrSelector)
		}
	case "drag":
		if step.SourceRef != "" {
			refs = append(refs, step.SourceRef)
		}
		_, targetKnown := currentPage.refs[step.TargetRef]
		if step.TargetRef != "" && (targetKnown || isSnapshotRef(step.TargetRef)) {
			refs = append(refs, step.TargetRef)
		}
	}
	return refs
}
