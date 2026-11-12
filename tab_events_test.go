package browserkit

import (
	"context"
	"testing"
	"time"
)

// fakeTabChangeSession 模拟支持 target 事件推送的浏览器。
type fakeTabChangeSession struct {
	*fakeViewportSession
	changes chan struct{}
}

func (session *fakeTabChangeSession) TabChanges() <-chan struct{} { return session.changes }

func TestTabChangeEventsPushStateWithoutPolling(t *testing.T) {
	browser := &fakeTabChangeSession{
		fakeViewportSession: &fakeViewportSession{
			fakeSession: &fakeSession{},
			tabs:        []TabInfo{{TargetID: "target-1", URL: "https://example.com", Title: "First", Type: "page"}},
		},
		changes: make(chan struct{}, 1),
	}
	manager, err := NewManager(Config{
		AllowPrivateNetwork: true,
		Factory: func(context.Context, SessionConfig) (Session, error) {
			return browser, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	if _, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"}); err != nil {
		t.Fatal(err)
	}
	subscribeCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates, err := manager.SubscribeState(subscribeCtx)
	if err != nil {
		t.Fatal(err)
	}
	drain := func() {
		for {
			select {
			case <-updates:
			default:
				return
			}
		}
	}
	waitUpdate := func(stage string) {
		select {
		case <-updates:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s 没有收到状态推送", stage)
		}
	}
	eventually := func(stage string, match func(BrowserViewState) bool) {
		deadline := time.Now().Add(2 * time.Second)
		for {
			view, viewErr := manager.View(context.Background())
			if viewErr == nil && match(view) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s 后状态仍未更新: view=%+v err=%v", stage, view, viewErr)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	drain()
	// 网站新开 Tab 只产生 target 事件，没有工具结果可以携带状态变化。
	browser.tabs = append(browser.tabs, TabInfo{TargetID: "target-2", URL: "https://example.org", Title: "Second", Type: "page"})
	browser.changes <- struct{}{}
	waitUpdate("新开 Tab")
	eventually("新开 Tab", func(view BrowserViewState) bool {
		return len(view.Tabs) == 2 && view.Tabs[1].Title == "Second"
	})

	drain()
	// 标题变化不伴随 URL 变化，仍必须推送新的状态投影。
	browser.tabs[1].Title = "Renamed"
	browser.changes <- struct{}{}
	waitUpdate("标题变化")
	eventually("标题变化", func(view BrowserViewState) bool {
		return len(view.Tabs) == 2 && view.Tabs[1].Title == "Renamed"
	})

	drain()
	// 网站或用户关闭 Tab 同样只产生 target 事件，订阅方必须收到删除后的投影。
	browser.tabs = browser.tabs[:1]
	browser.changes <- struct{}{}
	waitUpdate("关闭 Tab")
	eventually("关闭 Tab", func(view BrowserViewState) bool {
		return len(view.Tabs) == 1
	})
}
