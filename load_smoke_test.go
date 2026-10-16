//go:build browser_smoke

package browserkit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRealBrowserLoadingTimeoutReturnsCurrentContent(t *testing.T) {
	chrome := os.Getenv("BROWSERKIT_EXEC_PATH")
	if chrome == "" {
		chrome = "/Applications/Chromium.app/Contents/MacOS/Chromium"
		if _, err := os.Stat(chrome); err != nil {
			chrome = ""
		}
	}
	release := make(chan struct{})
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow-image" {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><title>加载中</title><h1>当前已可读</h1><button onclick="document.querySelector('h1').textContent='已继续操作'">继续</button><img src="/slow-image">`))
	}))
	defer site.Close()
	defer close(release)
	manager, err := NewManager(Config{Session: SessionConfig{ExecPath: chrome}, AllowPrivateNetwork: true, AllowedOrigins: []string{site.URL}})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	check := func(status OpenLoadStatus, warning string, snapshot *SnapshotResponse) {
		t.Helper()
		if status != OpenLoadTimeout || !strings.Contains(warning, "2 秒") || snapshot == nil || !strings.Contains(snapshot.Text, "当前已可读") {
			t.Fatalf("未返回超时提醒与当前内容：status=%s warning=%s snapshot=%+v", status, warning, snapshot)
		}
		for _, element := range snapshot.Elements {
			if element.Role != "button" || element.Name != "继续" {
				continue
			}
			if _, err := manager.Click(ctx, ClickRequest{SessionID: snapshot.SessionID, PageID: snapshot.PageID, Ref: element.Ref, Revision: snapshot.Revision}); err != nil {
				t.Fatal(err)
			}
			after, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: snapshot.SessionID, PageID: snapshot.PageID})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(after.Text, "已继续操作") {
				t.Fatalf("超时页面无法继续操作：%s", after.Text)
			}
			return
		}
		t.Fatal("超时快照缺少可操作按钮")
	}
	opened, err := manager.Open(ctx, OpenRequest{URL: site.URL})
	if err != nil {
		t.Fatal(err)
	}
	check(opened.LoadStatus, opened.Warning, opened.Snapshot)
	started := time.Now()
	navigated, err := manager.Navigate(ctx, NavigateRequest{SessionID: opened.SessionID, PageID: opened.PageID, URL: site.URL + "/next"})
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	if elapsed < 1800*time.Millisecond || elapsed > 3500*time.Millisecond {
		t.Fatalf("加载等待未限定为 2 秒：%s", elapsed)
	}
	check(navigated.LoadStatus, navigated.Warning, navigated.Snapshot)
	t.Logf("导航用时 %s，页面仍在加载时可继续操作", elapsed)
}
