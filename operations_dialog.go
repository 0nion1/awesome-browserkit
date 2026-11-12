package browserkit

import (
	"context"
	"errors"
)

// HandleDialog 明确接受或取消当前页面弹窗，并在页面恢复后返回新快照。
func (manager *Manager) HandleDialog(ctx context.Context, request HandleDialogRequest) (HandleDialogResponse, error) {
	if err := contextErr(ctx); err != nil {
		return HandleDialogResponse{}, err
	}
	if err := requirePage(request.SessionID, request.PageID, "browserHandleDialog"); err != nil {
		return HandleDialogResponse{}, err
	}
	if request.DialogID == 0 {
		return HandleDialogResponse{}, errors.New("browserHandleDialog 需要 dialog_id")
	}
	if request.Action != "accept" && request.Action != "dismiss" {
		return HandleDialogResponse{}, errors.New("browserHandleDialog action 必须是 accept 或 dismiss")
	}
	current, currentPage, err := manager.get(ctx, request.SessionID, request.PageID)
	if err != nil {
		return HandleDialogResponse{}, err
	}
	handler, ok := currentPage.page.(DialogPage)
	if !ok {
		return HandleDialogResponse{}, browserError("dialog_unsupported", "当前浏览器页面不支持显式处理弹窗", "使用支持 DialogPage 的 Browser Factory", true)
	}
	unlock, err := lockSessionInteraction(ctx, current)
	if err != nil {
		return HandleDialogResponse{}, err
	}
	defer unlock()
	dialog, err := handler.HandleDialog(ctx, DialogHandleOptions{ID: request.DialogID, Action: request.Action, PromptText: request.PromptText})
	if err != nil {
		return HandleDialogResponse{}, err
	}
	manager.emit(ctx, Event{Kind: "dialog_handled", SessionID: current.id, PageID: currentPage.id})
	unlock()
	response := HandleDialogResponse{SessionID: current.id, PageID: currentPage.id, Dialog: dialog}
	response.Snapshot, response.SnapshotError = manager.interactionSnapshot(ctx, current, currentPage)
	return response, nil
}
