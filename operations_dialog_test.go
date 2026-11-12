package browserkit

import (
	"context"
	"errors"
	"testing"
)

type fakeDialogPage struct {
	*fakePage
	pending *Dialog
	changes chan struct{}
	handled DialogHandleOptions
}

func (page *fakeDialogPage) PendingDialog() *Dialog {
	if page.pending == nil {
		return nil
	}
	value := *page.pending
	value.Options = append([]string(nil), value.Options...)
	return &value
}

func (page *fakeDialogPage) DialogChanges() <-chan struct{} { return page.changes }

func (page *fakeDialogPage) HandleDialog(_ context.Context, options DialogHandleOptions) (Dialog, error) {
	if page.pending == nil || options.ID != page.pending.ID {
		return Dialog{}, errors.New("dialog stale")
	}
	page.handled = options
	value := *page.pending
	value.Handled = true
	value.Accepted = options.Action == "accept"
	value.PromptText = options.PromptText
	page.pending = nil
	return value, nil
}

func TestSnapshotKeepsPendingDialogUntilExplicitHandling(t *testing.T) {
	manager, browser := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	initial, err := manager.Snapshot(context.Background(), SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	page := &fakeDialogPage{
		fakePage: browser.page,
		pending:  &Dialog{ID: 7, Type: "confirm", Message: "继续吗？", Options: []string{"accept", "dismiss"}},
		changes:  make(chan struct{}),
	}
	current := manager.sessions[opened.SessionID]
	current.mu.Lock()
	current.tabs[opened.PageID].page = page
	current.mu.Unlock()

	for range 2 {
		state, snapshotErr := manager.Snapshot(context.Background(), SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
		if snapshotErr != nil {
			t.Fatal(snapshotErr)
		}
		if state.Revision != initial.Revision || state.Text != initial.Text || len(state.Dialogs) != 1 {
			t.Fatalf("待处理弹窗没有叠加到最近页面状态: %+v", state)
		}
		dialog := state.Dialogs[0]
		if dialog.ID != 7 || dialog.Type != "confirm" || dialog.Message != "继续吗？" || dialog.Handled || dialog.Accepted {
			t.Fatalf("待处理弹窗状态错误: %+v", dialog)
		}
	}
	if browser.page.snapshotCalls != 2 {
		t.Fatalf("弹窗阻塞时仍读取了 renderer，snapshot 调用=%d", browser.page.snapshotCalls)
	}

	if _, err := manager.HandleDialog(context.Background(), HandleDialogRequest{
		SessionID: opened.SessionID, PageID: opened.PageID, DialogID: 6, Action: "accept",
	}); err == nil {
		t.Fatal("browserHandleDialog 接受了过期 dialog_id")
	}
	response, err := manager.HandleDialog(context.Background(), HandleDialogRequest{
		SessionID: opened.SessionID, PageID: opened.PageID, DialogID: 7, Action: "dismiss",
	})
	if err != nil {
		t.Fatal(err)
	}
	if page.handled.ID != 7 || page.handled.Action != "dismiss" || !response.Dialog.Handled || response.Dialog.Accepted {
		t.Fatalf("弹窗没有按明确选择处理: request=%+v response=%+v", page.handled, response)
	}
	if response.Snapshot == nil || len(response.Snapshot.Dialogs) != 0 {
		t.Fatalf("处理后没有恢复真实页面快照: %+v", response)
	}
}
