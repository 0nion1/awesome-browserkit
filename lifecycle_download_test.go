package browserkit

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-rod/rod/lib/proto"
)

type downloadCloseSession struct {
	*rodSession
	page Page
}

func (session *downloadCloseSession) NewPage(context.Context, PageOptions) (Page, error) {
	return session.page, nil
}

type downloadClosePage struct {
	*fakePage
	downloads        *rodSession
	stateDuringClose string
}

func (page *downloadClosePage) Close(ctx context.Context) error {
	// Model a native cancellation delivered while Manager closes the active Tab.
	page.downloads.downloadProgress(&proto.BrowserDownloadProgress{GUID: "active", State: proto.BrowserDownloadProgressStateCanceled})
	info, _ := page.downloads.DownloadStatus(context.Background(), "active")
	page.stateDuringClose = info.State
	return page.fakePage.Close(ctx)
}

func TestManagerCloseMarksDownloadsBeforeClosingPage(t *testing.T) {
	dir := t.TempDir()
	temp, err := os.MkdirTemp(dir, ".browserkit-")
	if err != nil {
		t.Fatal(err)
	}
	raw := &rodSession{downloadDir: dir, downloadTempDir: temp, downloadGuids: map[string]*downloadRecord{}, connectionDone: make(chan struct{})}
	for _, id := range []string{"active", "complete", "failed"} {
		raw.downloadWillBegin(&proto.BrowserDownloadWillBegin{GUID: id, FrameID: "frame", SuggestedFilename: id + ".txt"})
	}
	if err := os.WriteFile(filepath.Join(temp, "complete"), []byte("saved"), 0600); err != nil {
		t.Fatal(err)
	}
	raw.downloadProgress(&proto.BrowserDownloadProgress{GUID: "complete", State: proto.BrowserDownloadProgressStateCompleted, ReceivedBytes: 5})
	raw.downloadProgress(&proto.BrowserDownloadProgress{GUID: "failed", State: proto.BrowserDownloadProgressStateCanceled})
	completed, _ := raw.DownloadStatus(context.Background(), "complete")
	failed, _ := raw.DownloadStatus(context.Background(), "failed")
	page := &downloadClosePage{fakePage: &fakePage{info: PageInfo{URL: "about:blank"}}, downloads: raw}
	adapter := &downloadCloseSession{rodSession: raw, page: page}
	manager, err := NewManager(Config{Factory: func(context.Context, SessionConfig) (Session, error) { return adapter, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	if _, err := manager.Open(context.Background(), OpenRequest{}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	active, err := raw.DownloadStatus(context.Background(), "active")
	if err != nil || active.State != DownloadSessionClosed || page.stateDuringClose != DownloadSessionClosed {
		t.Fatalf("Manager.Close cancellation misclassified: active=%+v state_during_page_close=%s err=%v", active, page.stateDuringClose, err)
	}
	afterComplete, _ := raw.DownloadStatus(context.Background(), "complete")
	afterFailed, _ := raw.DownloadStatus(context.Background(), "failed")
	if afterComplete != completed || afterFailed != failed {
		t.Fatalf("terminal download changed: complete=%+v failed=%+v", afterComplete, afterFailed)
	}
	data, err := os.ReadFile(completed.Path)
	if err != nil || string(data) != "saved" {
		t.Fatalf("completed file lost: %q %v", data, err)
	}
	if _, err := os.Stat(temp); !os.IsNotExist(err) {
		t.Fatalf("temporary download directory remains: %v", err)
	}
}
