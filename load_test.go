package browserkit

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLoadingTimeoutReturnsUsableSnapshot(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	session.page = &fakePage{info: PageInfo{URL: "about:blank", Title: "partial"}, idleErr: context.DeadlineExceeded}
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	check := func(status OpenLoadStatus, snapshot *SnapshotResponse, warning string) {
		t.Helper()
		if status != OpenLoadTimeout || !strings.Contains(warning, "总计 3 秒") || !strings.Contains(warning, "可能不完整") {
			t.Fatalf("加载超时未返回提醒：status=%s warning=%s", status, warning)
		}
		if snapshot == nil || snapshot.Text != "hello" || snapshot.SessionID != opened.SessionID || snapshot.PageID != opened.PageID || len(snapshot.Elements) == 0 {
			t.Fatalf("缺少当前页面内容：%+v", snapshot)
		}
		_, err := manager.Click(ctx, ClickRequest{SessionID: snapshot.SessionID, PageID: snapshot.PageID, Ref: snapshot.Elements[0].Ref, Revision: snapshot.Revision})
		if err != nil {
			t.Fatalf("超时快照的 ref 无法直接使用：%v", err)
		}
	}
	check(opened.LoadStatus, opened.Snapshot, opened.Warning)
	reused, err := manager.Open(ctx, OpenRequest{SessionID: opened.SessionID, URL: "/reuse"})
	if err != nil {
		t.Fatal(err)
	}
	check(reused.LoadStatus, reused.Snapshot, reused.Warning)
	navigated, err := manager.Navigate(ctx, NavigateRequest{SessionID: opened.SessionID, PageID: opened.PageID, URL: "/next"})
	if err != nil {
		t.Fatal(err)
	}
	check(navigated.LoadStatus, navigated.Snapshot, navigated.Warning)
	reloaded, err := manager.Reload(ctx, TabRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	check(reloaded.LoadStatus, reloaded.Snapshot, reloaded.Warning)
	if session.page.idleTimeout != 3*time.Second || session.page.waitOptions.Condition != "load_complete" {
		t.Fatalf("未在统一的 3 秒期限内等待完整加载及空闲：timeout=%s options=%+v", session.page.idleTimeout, session.page.waitOptions)
	}
}

func TestLoadingCompleteReturnsSnapshot(t *testing.T) {
	manager, _ := fakeManager(t, Config{})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if opened.LoadStatus != OpenLoadLoaded || opened.Snapshot == nil || opened.Snapshot.Text != "hello" || opened.Warning != "" {
		t.Fatalf("已加载页面应返回快照：%+v", opened)
	}
	navigated, err := manager.Navigate(context.Background(), NavigateRequest{SessionID: opened.SessionID, PageID: opened.PageID, URL: "/next"})
	if err != nil {
		t.Fatal(err)
	}
	if navigated.LoadStatus != OpenLoadLoaded || navigated.Snapshot == nil || navigated.Warning != "" {
		t.Fatalf("已加载导航应返回快照：%+v", navigated)
	}
}

func TestLoadingTimeoutSnapshotFailureKeepsPage(t *testing.T) {
	manager, session := fakeManager(t, Config{})
	defer manager.Close(context.Background())
	session.page = &fakePage{info: PageInfo{URL: "about:blank"}, idleErr: context.DeadlineExceeded, snapshotErr: errors.New("snapshot unavailable")}
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if opened.LoadStatus != OpenLoadTimeout || opened.Snapshot != nil || !strings.Contains(opened.Warning, "snapshot unavailable") {
		t.Fatalf("快照失败丢失了导航结果：%+v", opened)
	}
	if _, _, err := manager.get(context.Background(), opened.SessionID, opened.PageID); err != nil {
		t.Fatal(err)
	}
}

func TestLoadingWaitPreservesCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if timedOut, err := waitPageIdle(ctx, &fakePage{waitErr: context.Canceled}, defaultTiming().openIdle); timedOut || !errors.Is(err, context.Canceled) {
		t.Fatalf("调用方取消被当作可继续的加载超时：timeout=%v err=%v", timedOut, err)
	}
}
