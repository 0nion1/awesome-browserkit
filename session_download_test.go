package browserkit

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/go-rod/rod/lib/proto"
)

func TestFinalizeDownloadConcurrentSameName(t *testing.T) {
	dir := t.TempDir()
	const count = 16
	results := make([]DownloadInfo, count)
	errs := make([]error, count)
	var wait sync.WaitGroup
	for index := 0; index < count; index++ {
		guid := fmt.Sprintf("guid-%d", index)
		content := fmt.Sprintf("content-%d", index)
		if err := os.WriteFile(filepath.Join(dir, guid), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		wait.Add(1)
		go func(index int, guid string) {
			defer wait.Done()
			results[index], errs[index] = finalizeDownload(dir, &proto.BrowserDownloadWillBegin{
				GUID: guid, SuggestedFilename: "same.txt",
			})
		}(index, guid)
	}
	wait.Wait()
	names := map[string]bool{}
	for index := 0; index < count; index++ {
		if errs[index] != nil {
			t.Fatalf("第 %d 个下载失败: %v", index, errs[index])
		}
		if names[results[index].Path] {
			t.Fatalf("并发下载返回重复路径: %s", results[index].Path)
		}
		names[results[index].Path] = true
		if _, err := os.Stat(results[index].Path); err != nil {
			t.Fatalf("下载文件不存在: %v", err)
		}
	}
	if len(names) != count {
		t.Fatalf("唯一文件数=%d，期望=%d", len(names), count)
	}
}

func TestRegisterDownloadRequiresEnabledSession(t *testing.T) {
	session := &rodSession{}
	if _, err := session.registerDownload(map[string]bool{"frame": true}, t.TempDir(), "target"); err == nil {
		t.Fatal("未启用下载的 session 应拒绝注册")
	}
	if _, err := NewManager(Config{Session: SessionConfig{RemoteAddress: "ws://127.0.0.1:9222", DownloadDir: t.TempDir()}}); err == nil {
		t.Fatal("Session.DownloadDir 与远程浏览器应被拒绝")
	}
}

func TestSafeDownloadNameTruncatesBytes(t *testing.T) {
	name := safeDownloadName("你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你你.txt", "fallback")
	if len(name) > 240 {
		t.Fatalf("文件名超过字节上限: %d", len(name))
	}
	if filepath.Ext(name) != ".txt" {
		t.Fatalf("截断后丢失扩展名: %q", name)
	}
}
