package browserkit

import (
	"context"
	"path/filepath"
	"testing"
)

func TestDownloadDirNormalizesAndMergesSessionConfig(t *testing.T) {
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(Config{Session: SessionConfig{DownloadDir: dir}})
	if err != nil {
		t.Fatal(err)
	}
	if manager.config.DownloadDir != resolved || manager.config.Session.DownloadDir != resolved {
		t.Fatalf("DownloadDir 未合并或未绝对化: %+v", manager.config)
	}
	found := false
	for _, function := range manager.Functions() {
		if function.Name == "browserDownload" {
			found = true
		}
	}
	if !found {
		t.Fatal("Session.DownloadDir 没有暴露 browserDownload")
	}
	_ = manager.Close(context.Background())

	if _, err := NewManager(Config{DownloadDir: dir, Session: SessionConfig{DownloadDir: t.TempDir()}}); err == nil {
		t.Fatal("两个不同的 DownloadDir 应被拒绝")
	}
}

func TestDownloadDirRejectsRemoteBrowser(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewManager(Config{DownloadDir: dir, Session: SessionConfig{RemoteAddress: "ws://127.0.0.1:9222"}}); err == nil {
		t.Fatal("远程浏览器不应接受 DownloadDir")
	}
	if _, err := NewManager(Config{DownloadDir: dir, Session: SessionConfig{ExecPath: "http://127.0.0.1:9222"}}); err == nil {
		t.Fatal("ExecPath 中的远程地址不应接受 DownloadDir")
	}
	if _, err := NewSession(context.Background(), SessionConfig{RemoteAddress: "ws://127.0.0.1:9222", DownloadDir: dir}); err == nil {
		t.Fatal("NewSession 不应接受远程浏览器下载目录")
	}
}
