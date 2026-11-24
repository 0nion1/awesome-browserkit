//go:build browser_smoke

package browserkit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestRealBrowserDialogPriorityPages 用多个真实页面覆盖弹窗与加载、连续脚本和
// 多 Tab 的交界，防止辅助 CDP 请求再次把显式弹窗处理锁在普通操作超时里。
func TestRealBrowserDialogPriorityPages(t *testing.T) {
	t.Run("加载中的延迟弹窗优先返回", func(t *testing.T) {
		release := make(chan struct{})
		var releaseOnce sync.Once
		defer releaseOnce.Do(func() { close(release) })
		mux := http.NewServeMux()
		mux.HandleFunc("/ready", func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write([]byte(`<!doctype html><title>Ready</title>`))
		})
		mux.HandleFunc("/", func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write([]byte(`<!doctype html><title>Slow Dialog</title>
<h1>弹窗前页面已经可读</h1><img src="/slow-resource">
<script>setTimeout(() => alert('load pending'), 100)</script>`))
		})
		mux.HandleFunc("/slow-resource", func(writer http.ResponseWriter, request *http.Request) {
			select {
			case <-release:
			case <-request.Context().Done():
			}
		})
		server := httptest.NewServer(mux)
		defer server.Close()
		manager, err := NewManager(Config{AllowPrivateNetwork: true})
		if err != nil {
			t.Fatal(err)
		}
		defer manager.Close(context.Background())

		// 冷启动 Chrome 不属于页面加载的等待窗口；先建立可复用 session，
		// 再计时验证弹窗确实打断了加载等待，而不是测本机启动速度。
		ready, err := manager.Open(context.Background(), OpenRequest{URL: server.URL + "/ready"})
		if err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		opened, err := manager.Open(context.Background(), OpenRequest{SessionID: ready.SessionID, URL: server.URL})
		if err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(started); elapsed > 2*time.Second {
			t.Fatalf("加载弹窗没有抢占普通加载等待: %s", elapsed)
		}
		if opened.Snapshot == nil || len(opened.Snapshot.Dialogs) != 1 || opened.Snapshot.Dialogs[0].Message != "load pending" {
			t.Fatalf("browserOpen 没有返回加载中的弹窗状态: %+v", opened)
		}
		dialog := opened.Snapshot.Dialogs[0]
		handled, err := manager.HandleDialog(context.Background(), HandleDialogRequest{
			SessionID: opened.SessionID, PageID: opened.PageID, DialogID: dialog.ID, Action: "accept",
		})
		if err != nil || handled.Snapshot == nil || !strings.Contains(handled.Snapshot.Text, "弹窗前页面已经可读") {
			t.Fatalf("处理加载弹窗后没有恢复当前页面: response=%+v err=%v", handled, err)
		}
		releaseOnce.Do(func() { close(release) })
	})

	t.Run("连续弹窗逐个暴露", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write([]byte(`<!doctype html><title>Chained Dialogs</title>
<button aria-label="Chain" onclick="alert('first'); this.textContent = confirm('second') ? 'accepted' : 'dismissed'">Chain</button>`))
		}))
		defer server.Close()
		manager, err := NewManager(Config{AllowPrivateNetwork: true})
		if err != nil {
			t.Fatal(err)
		}
		defer manager.Close(context.Background())
		ctx := context.Background()
		opened, err := manager.Open(ctx, OpenRequest{URL: server.URL})
		if err != nil {
			t.Fatal(err)
		}
		initial := opened.Snapshot
		if initial == nil {
			initialValue, snapshotErr := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
			if snapshotErr != nil {
				t.Fatal(snapshotErr)
			}
			initial = &initialValue
		}
		clicked, err := manager.Click(ctx, ClickRequest{
			SessionID: opened.SessionID, PageID: opened.PageID,
			Ref: smokeElementRef(t, *initial, "button", "Chain"), Revision: initial.Revision,
		})
		if err != nil || clicked.Snapshot == nil || len(clicked.Snapshot.Dialogs) != 1 {
			t.Fatalf("第一个弹窗没有进入点击结果: response=%+v err=%v", clicked, err)
		}
		first := clicked.Snapshot.Dialogs[0]
		if first.Type != "alert" || first.Message != "first" {
			t.Fatalf("第一个弹窗=%+v", first)
		}
		accepted, err := manager.HandleDialog(ctx, HandleDialogRequest{
			SessionID: opened.SessionID, PageID: opened.PageID, DialogID: first.ID, Action: "accept",
		})
		if err != nil || accepted.Snapshot == nil || len(accepted.Snapshot.Dialogs) != 1 {
			t.Fatalf("接受第一个弹窗后没有暴露第二个: response=%+v err=%v", accepted, err)
		}
		second := accepted.Snapshot.Dialogs[0]
		if second.Type != "confirm" || second.Message != "second" || second.ID <= first.ID {
			t.Fatalf("第二个弹窗=%+v，第一个=%+v", second, first)
		}
		dismissed, err := manager.HandleDialog(ctx, HandleDialogRequest{
			SessionID: opened.SessionID, PageID: opened.PageID, DialogID: second.ID, Action: "dismiss",
		})
		if err != nil || dismissed.Snapshot == nil || !strings.Contains(dismissed.Snapshot.Text, "dismissed") {
			t.Fatalf("取消第二个弹窗后页面没有继续执行: response=%+v err=%v", dismissed, err)
		}
	})

	t.Run("prompt 取消保持 null 语义", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write([]byte(`<!doctype html><title>Prompt Cancel</title>
<button aria-label="Default Prompt" onclick="const value = prompt('default prompt', 'preset'); this.textContent = value === null ? 'default:null' : 'default:' + value">Default Prompt</button>
<button aria-label="Empty Prompt" onclick="const value = prompt('empty prompt', ''); this.textContent = value === null ? 'empty:null' : 'empty:' + value">Empty Prompt</button>`))
		}))
		defer server.Close()
		manager, err := NewManager(Config{AllowPrivateNetwork: true})
		if err != nil {
			t.Fatal(err)
		}
		defer manager.Close(context.Background())
		ctx := context.Background()
		opened, err := manager.Open(ctx, OpenRequest{URL: server.URL})
		if err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct {
			name          string
			message       string
			defaultValue  string
			cancelledText string
		}{
			{name: "Default Prompt", message: "default prompt", defaultValue: "preset", cancelledText: "default:null"},
			{name: "Empty Prompt", message: "empty prompt", cancelledText: "empty:null"},
		} {
			snapshot, snapshotErr := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
			if snapshotErr != nil {
				t.Fatal(snapshotErr)
			}
			clicked, clickErr := manager.Click(ctx, ClickRequest{
				SessionID: opened.SessionID, PageID: opened.PageID,
				Ref: smokeElementRef(t, snapshot, "button", test.name), Revision: snapshot.Revision,
			})
			if clickErr != nil || clicked.Snapshot == nil || len(clicked.Snapshot.Dialogs) != 1 {
				t.Fatalf("%s 没有返回 prompt: response=%+v err=%v", test.name, clicked, clickErr)
			}
			dialog := clicked.Snapshot.Dialogs[0]
			if dialog.Type != "prompt" || dialog.Message != test.message || dialog.PromptText != test.defaultValue || dialog.Handled {
				t.Fatalf("%s prompt 状态=%+v", test.name, dialog)
			}
			handled, handleErr := manager.HandleDialog(ctx, HandleDialogRequest{
				SessionID: opened.SessionID, PageID: opened.PageID, DialogID: dialog.ID, Action: "dismiss",
			})
			if handleErr != nil || handled.Snapshot == nil || handled.Dialog.Accepted || !handled.Dialog.Handled {
				t.Fatalf("%s 取消失败: response=%+v err=%v", test.name, handled, handleErr)
			}
			if !strings.Contains(handled.Snapshot.Text, test.cancelledText) {
				t.Fatalf("%s 取消后没有保留 null 语义: %q", test.name, handled.Snapshot.Text)
			}
		}
	})

	t.Run("一个 Tab 的弹窗不阻塞其他 Tab", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/dialog", func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write([]byte(`<!doctype html><title>Dialog Tab</title><button aria-label="Open" onclick="alert('tab one')">Open</button>`))
		})
		mux.HandleFunc("/plain", func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write([]byte(`<!doctype html><title>Plain Tab</title><h1>second tab ready</h1>`))
		})
		server := httptest.NewServer(mux)
		defer server.Close()
		manager, err := NewManager(Config{AllowPrivateNetwork: true})
		if err != nil {
			t.Fatal(err)
		}
		defer manager.Close(context.Background())
		ctx := context.Background()
		first, err := manager.Open(ctx, OpenRequest{URL: server.URL + "/dialog"})
		if err != nil {
			t.Fatal(err)
		}
		firstSnapshot := first.Snapshot
		if firstSnapshot == nil {
			value, snapshotErr := manager.Snapshot(ctx, SnapshotRequest{SessionID: first.SessionID, PageID: first.PageID})
			if snapshotErr != nil {
				t.Fatal(snapshotErr)
			}
			firstSnapshot = &value
		}
		clicked, err := manager.Click(ctx, ClickRequest{
			SessionID: first.SessionID, PageID: first.PageID,
			Ref: smokeElementRef(t, *firstSnapshot, "button", "Open"), Revision: firstSnapshot.Revision,
		})
		if err != nil || clicked.Snapshot == nil || len(clicked.Snapshot.Dialogs) != 1 {
			t.Fatalf("第一个 Tab 没有进入弹窗状态: response=%+v err=%v", clicked, err)
		}
		second, err := manager.NewTab(ctx, OpenRequest{SessionID: first.SessionID, URL: server.URL + "/plain"})
		if err != nil {
			t.Fatalf("第一个 Tab 的弹窗阻塞了新 Tab: %v", err)
		}
		secondSnapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: first.SessionID, PageID: second.PageID})
		if err != nil || !strings.Contains(secondSnapshot.Text, "second tab ready") || len(secondSnapshot.Dialogs) != 0 {
			t.Fatalf("第二个 Tab 状态异常: snapshot=%+v err=%v", secondSnapshot, err)
		}
		firstAgain, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: first.SessionID, PageID: first.PageID})
		if err != nil || len(firstAgain.Dialogs) != 1 || firstAgain.Dialogs[0].Message != "tab one" {
			t.Fatalf("切换其他 Tab 后丢失原弹窗: snapshot=%+v err=%v", firstAgain, err)
		}
		if _, err := manager.HandleDialog(ctx, HandleDialogRequest{
			SessionID: first.SessionID, PageID: first.PageID,
			DialogID: firstAgain.Dialogs[0].ID, Action: "accept",
		}); err != nil {
			t.Fatal(err)
		}
	})
}
