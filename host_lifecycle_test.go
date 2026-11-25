package browserkit

import (
	"context"
	"testing"
	"time"
)

func TestHostManagedSessionLifetime(t *testing.T) {
	manager, browser := fakeManagerWithTiming(t, Config{AllowPrivateNetwork: true, DisableIdleReclaim: true, IdleTimeout: time.Millisecond}, adjustedTiming(func(value *timing) { value.sessionIdleCheck = time.Millisecond }))
	opened, err := manager.Open(context.Background(), OpenRequest{})
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	current := manager.sessions[opened.SessionID]
	manager.mu.Unlock()
	current.mu.Lock()
	current.lastUsed = time.Now().Add(-time.Hour)
	current.mu.Unlock()
	time.Sleep(15 * time.Millisecond)
	if _, _, err := manager.get(context.Background(), opened.SessionID, opened.PageID); err != nil {
		t.Fatalf("宿主管理会话仍被回收: %v", err)
	}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !browser.Closed() {
		t.Fatal("宿主关闭没有关闭浏览器")
	}
}
