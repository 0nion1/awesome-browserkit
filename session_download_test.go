package browserkit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/cdp"
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

func TestDownloadRequiresEnabledSession(t *testing.T) {
	session := &rodSession{}
	if _, err := session.Downloads(context.Background()); err == nil {
		t.Fatal("未启用下载的 session 应拒绝查询")
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

func TestDownloadWaitTimeoutPreservesIdentityAndProgress(t *testing.T) {
	session := &rodSession{downloadGuids: map[string]*downloadRecord{}}
	session.downloadWillBegin(&proto.BrowserDownloadWillBegin{GUID: "one", FrameID: "frame-a", URL: "https://example.test/file"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := session.WaitDownload(ctx, "one"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
	session.operationTimeout = time.Millisecond
	partial, err := session.WaitDownload(context.Background(), "one")
	if !errors.Is(err, context.DeadlineExceeded) || partial.ID != "one" || partial.State != DownloadInProgress {
		t.Fatalf("timeout lost download: info=%+v err=%v", partial, err)
	}
	session.downloadProgress(&proto.BrowserDownloadProgress{GUID: "one", State: proto.BrowserDownloadProgressStateInProgress, ReceivedBytes: 13})
	info, err := session.DownloadStatus(context.Background(), "one")
	if err != nil || info.ID != "one" || info.State != DownloadInProgress || info.Bytes != 13 || info.Path != "" {
		t.Fatalf("info=%+v err=%v", info, err)
	}
	session.closeDownloads()
	info, err = session.WaitDownload(context.Background(), "one")
	if err != nil || info.State != DownloadSessionClosed || info.Path != "" {
		t.Fatalf("closed info=%+v err=%v", info, err)
	}
}

func TestDownloadBudgetChecksProgressAndActualFile(t *testing.T) {
	dir := t.TempDir()
	session := &rodSession{downloadTempDir: dir, downloadDir: dir, downloadMaxBytes: 10, downloadGuids: map[string]*downloadRecord{}}
	session.downloadWillBegin(&proto.BrowserDownloadWillBegin{GUID: "progress", FrameID: "one"})
	session.downloadProgress(&proto.BrowserDownloadProgress{GUID: "progress", State: proto.BrowserDownloadProgressStateInProgress, ReceivedBytes: 11})
	session.downloadProgress(&proto.BrowserDownloadProgress{GUID: "progress", State: proto.BrowserDownloadProgressStateCanceled, ReceivedBytes: 11})
	info, err := session.DownloadStatus(context.Background(), "progress")
	if err != nil || info.State != DownloadOverLimit || info.Path != "" {
		t.Fatalf("progress=%+v err=%v", info, err)
	}
	session.downloadWillBegin(&proto.BrowserDownloadWillBegin{GUID: "actual", FrameID: "two", SuggestedFilename: "actual.txt"})
	if err := os.WriteFile(filepath.Join(dir, "actual"), make([]byte, 11), 0600); err != nil {
		t.Fatal(err)
	}
	session.downloadProgress(&proto.BrowserDownloadProgress{GUID: "actual", State: proto.BrowserDownloadProgressStateCompleted, ReceivedBytes: 9})
	info, err = session.DownloadStatus(context.Background(), "actual")
	if err != nil || info.State != DownloadOverLimit || info.Path != "" {
		t.Fatalf("actual=%+v err=%v", info, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "actual")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial not removed: %v", err)
	}
}

func TestDownloadEventsPreserveTheirOwnFrameIdentity(t *testing.T) {
	session := &rodSession{downloadGuids: map[string]*downloadRecord{}}
	session.downloadWillBegin(&proto.BrowserDownloadWillBegin{GUID: "one", FrameID: "frame-a", URL: "https://example.test/one"})
	session.downloadWillBegin(&proto.BrowserDownloadWillBegin{GUID: "two", FrameID: "frame-b", URL: "https://example.test/two"})
	downloads, err := session.Downloads(context.Background())
	if err != nil || len(downloads) != 2 {
		t.Fatalf("downloads=%+v err=%v", downloads, err)
	}
	for _, info := range downloads {
		if info.Triggered || info.TargetID != "" {
			t.Fatalf("event invented action or target: %+v", info)
		}
		if info.ID == "one" && info.FrameID != "frame-a" || info.ID == "two" && info.FrameID != "frame-b" {
			t.Fatalf("wrong frame: %+v", info)
		}
	}
}

func TestDownloadBudgetFailedCancelRetriesAndCanceledWaitKeepsInfo(t *testing.T) {
	client := &downloadCancelClient{attempts: make(chan struct{}, 3), firstFailed: make(chan struct{}), releaseFirst: make(chan struct{})}
	browser := rod.New().Client(client)
	session := &rodSession{browser: browser, downloadMaxBytes: 10, downloadGuids: map[string]*downloadRecord{}}
	session.downloadWillBegin(&proto.BrowserDownloadWillBegin{GUID: "retry", FrameID: "frame"})
	session.downloadProgress(&proto.BrowserDownloadProgress{GUID: "retry", State: proto.BrowserDownloadProgressStateInProgress, ReceivedBytes: 11})
	<-client.attempts
	session.downloadProgress(&proto.BrowserDownloadProgress{GUID: "retry", State: proto.BrowserDownloadProgressStateInProgress, ReceivedBytes: 12})
	select {
	case <-client.attempts:
		t.Fatal("concurrent duplicate cancel")
	default:
	}
	close(client.releaseFirst)
	<-client.firstFailed
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		session.downloadMu.Lock()
		pending := session.downloadGuids["retry"].cancelPending
		session.downloadMu.Unlock()
		if !pending {
			break
		}
		time.Sleep(time.Millisecond)
	}
	session.downloadProgress(&proto.BrowserDownloadProgress{GUID: "retry", State: proto.BrowserDownloadProgressStateInProgress, ReceivedBytes: 13})
	select {
	case <-client.attempts:
	case <-time.After(time.Second):
		t.Fatal("failed budget cancellation did not retry")
	}
	info, err := session.DownloadStatus(context.Background(), "retry")
	if err != nil || info.State != DownloadInProgress || info.Error == "" {
		t.Fatalf("info=%+v err=%v", info, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	info, err = session.WaitDownload(ctx, "retry")
	if !errors.Is(err, context.Canceled) || info.ID != "retry" || info.Bytes != 13 {
		t.Fatalf("canceled wait=%+v err=%v", info, err)
	}
	info, err = session.CancelDownload(ctx, "retry")
	if !errors.Is(err, context.Canceled) || info.ID != "retry" {
		t.Fatalf("canceled cancel=%+v err=%v", info, err)
	}
}

type downloadCancelClient struct {
	attempts     chan struct{}
	firstFailed  chan struct{}
	releaseFirst chan struct{}
	calls        atomic.Int64
}

func (client *downloadCancelClient) Call(ctx context.Context, sessionID, method string, params any) ([]byte, error) {
	if method == "Browser.cancelDownload" {
		count := client.calls.Add(1)
		client.attempts <- struct{}{}
		if count == 1 {
			<-client.releaseFirst
			close(client.firstFailed)
			return nil, errors.New("temporary transport failure")
		}
	}
	return []byte(`{}`), nil
}

func (client *downloadCancelClient) Event() <-chan *cdp.Event { return nil }
