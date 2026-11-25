//go:build browser_smoke

package browserkit

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func awaitSmokeDownload(t *testing.T, manager *Manager, sessionID string, count int) []DownloadInfo {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response, err := manager.Download(context.Background(), DownloadRequest{SessionID: sessionID, Action: "list"})
		if err != nil {
			t.Fatal(err)
		}
		if len(response.Downloads) >= count {
			return response.Downloads
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("download did not begin")
	return nil
}

// Only these fixtures have exactly one download. Production callers inspect the
// session list; they cannot infer causality from frame, URL or event order.
func triggerSingleFixtureDownload(t *testing.T, manager *Manager, request DownloadRequest) DownloadResponse {
	t.Helper()
	started, err := manager.Download(context.Background(), request)
	var browserErr *Error
	if !errors.As(err, &browserErr) || browserErr.Kind != "download_unconfirmed" || !started.Triggered || started.State != "unconfirmed" || started.DownloadID != "" || started.Path != "" || started.FileName != "" {
		t.Fatalf("start=%+v err=%v", started, err)
	}
	info := awaitSmokeDownload(t, manager, request.SessionID, 1)[0]
	response, err := manager.Download(context.Background(), DownloadRequest{SessionID: request.SessionID, Action: "wait", DownloadID: info.ID, TimeoutMS: request.TimeoutMS})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for response.TargetID == "" && time.Now().Before(deadline) {
		response, err = manager.Download(context.Background(), DownloadRequest{SessionID: request.SessionID, Action: "query", DownloadID: info.ID})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return response
}

func TestRealBrowserAutomaticDownloadWaitResumesWithoutClick(t *testing.T) {
	release := make(chan struct{})
	var requests atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<!doctype html><a href="/file">download</a>`))
	})
	mux.HandleFunc("/file", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Disposition", `attachment; filename="resumed.txt"`)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(bytes.Repeat([]byte("first"), 4096))
		w.(http.Flusher).Flush()
		select {
		case <-release:
			_, _ = w.Write([]byte("last"))
		case <-r.Context().Done():
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true, DownloadDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(context.Background(), SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.Click(context.Background(), ClickRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: smokeElementRef(t, snapshot, "link", "download"), Revision: snapshot.Revision})
	if err != nil {
		t.Fatal(err)
	}
	info := awaitSmokeDownload(t, manager, opened.SessionID, 1)[0]
	response, err := manager.Download(context.Background(), DownloadRequest{SessionID: opened.SessionID, Action: "wait", DownloadID: info.ID, TimeoutMS: 20})
	var browserErr *Error
	if !errors.As(err, &browserErr) || browserErr.Kind != "download_timeout" || response.DownloadID != info.ID || response.State != DownloadInProgress || response.Path != "" {
		t.Fatalf("wait=%+v err=%v", response, err)
	}
	close(release)
	response, err = manager.Download(context.Background(), DownloadRequest{SessionID: opened.SessionID, Action: "wait", DownloadID: info.ID, TimeoutMS: 5000})
	if err != nil || response.State != DownloadCompleted || response.DownloadID != info.ID {
		t.Fatalf("resumed=%+v err=%v", response, err)
	}
	data, err := os.ReadFile(response.Path)
	if err != nil || !bytes.Equal(data, append(bytes.Repeat([]byte("first"), 4096), []byte("last")...)) || requests.Load() != 1 {
		t.Fatalf("data=%q err=%v requests=%d", data, err, requests.Load())
	}
	// 来源在异步 frame tree 核对后必须等于当前真实 target。
	current, selected, err := manager.get(context.Background(), opened.SessionID, opened.PageID)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for response.TargetID == "" && time.Now().Before(deadline) {
		response, err = manager.Download(context.Background(), DownloadRequest{SessionID: opened.SessionID, Action: "query", DownloadID: info.ID})
		time.Sleep(10 * time.Millisecond)
	}
	if response.TargetID != selected.targetID {
		t.Fatalf("target=%s expected=%s", response.TargetID, selected.targetID)
	}
	raw := current.browser.(*rodSession)
	tempDir := raw.downloadTempDir
	if err := manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tempDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary download directory remained: %v", err)
	}
	if _, err := os.Stat(response.Path); err != nil {
		t.Fatalf("completed file removed: %v", err)
	}
}

func TestRealBrowserDownloadBudgetCancelAndSessionClose(t *testing.T) {
	for _, mode := range []string{"budget", "cancel", "close"} {
		t.Run(mode, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`<!doctype html><a href="/stream">stream</a>`))
			})
			mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Disposition", `attachment; filename="stream.bin"`)
				w.Header().Set("Content-Type", "application/octet-stream")
				ticker := time.NewTicker(40 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-r.Context().Done():
						return
					case <-ticker.C:
						if _, err := w.Write(make([]byte, 16384)); err != nil {
							return
						}
						w.(http.Flusher).Flush()
					}
				}
			})
			server := httptest.NewServer(mux)
			defer server.Close()
			max := int64(0)
			if mode == "budget" {
				max = 32768
			}
			dir := t.TempDir()
			manager, err := NewManager(Config{AllowPrivateNetwork: true, DownloadDir: dir, DownloadMaxBytes: max})
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Close(context.Background())
			opened, err := manager.Open(context.Background(), OpenRequest{URL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := manager.Snapshot(context.Background(), SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
			if err != nil {
				t.Fatal(err)
			}
			_, err = manager.Click(context.Background(), ClickRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: smokeElementRef(t, snapshot, "link", "stream"), Revision: snapshot.Revision})
			if err != nil {
				t.Fatal(err)
			}
			info := awaitSmokeDownload(t, manager, opened.SessionID, 1)[0]
			current, _, err := manager.get(context.Background(), opened.SessionID, opened.PageID)
			if err != nil {
				t.Fatal(err)
			}
			raw := current.browser.(*rodSession)
			waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			expected := DownloadOverLimit
			switch mode {
			case "budget":
				info, err = raw.WaitDownload(waitCtx, info.ID)
			case "cancel":
				expected = DownloadCanceled
				info, err = raw.CancelDownload(waitCtx, info.ID)
			case "close":
				expected = DownloadSessionClosed
				_ = manager.Close(context.Background())
				info, err = raw.WaitDownload(waitCtx, info.ID)
			}
			if err != nil || info.State != expected || info.Path != "" {
				t.Fatalf("info=%+v err=%v", info, err)
			}
			if mode == "budget" && info.Bytes <= max {
				t.Fatalf("budget not reached: %+v", info)
			}
			_ = manager.Close(context.Background())
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("partial files remain: %v", entries)
			}
			if _, err := os.Stat(filepath.Join(dir, "stream.bin")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("partial published: %v", err)
			}
		})
	}
}

func TestRealBrowserDownloadsNavigationSubmitAndSeparateTabs(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<!doctype html><form method="post" action="/file?source=form"><button>submit file</button></form>`))
	})
	mux.HandleFunc("/file", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="same.txt"`)
		_, _ = w.Write([]byte(r.Method + ":" + r.URL.Query().Get("source")))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true, DownloadDir: t.TempDir(), AllowEvaluate: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	first, err := manager.Open(context.Background(), OpenRequest{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(context.Background(), SnapshotRequest{SessionID: first.SessionID, PageID: first.PageID})
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.Click(context.Background(), ClickRequest{SessionID: first.SessionID, PageID: first.PageID, Ref: smokeElementRef(t, snapshot, "button", "submit file"), Revision: snapshot.Revision})
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.NewTab(context.Background(), OpenRequest{SessionID: first.SessionID, URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	// 原生 location 导航触发附件下载，无需 browserDownload.start。
	_, _ = manager.Evaluate(context.Background(), EvaluateRequest{SessionID: first.SessionID, PageID: second.PageID, Code: `location.href=` + strconv.Quote(server.URL+"/file?source=navigation")})
	listed := awaitSmokeDownload(t, manager, first.SessionID, 2)
	paths := map[string]bool{}
	sources := map[string]string{}
	for _, download := range listed {
		response, waitErr := manager.Download(context.Background(), DownloadRequest{SessionID: first.SessionID, Action: "wait", DownloadID: download.ID, TimeoutMS: 5000})
		if waitErr != nil {
			t.Fatal(waitErr)
		}
		data, readErr := os.ReadFile(response.Path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if paths[response.Path] {
			t.Fatalf("downloads shared path: %s", response.Path)
		}
		paths[response.Path] = true
		deadline := time.Now().Add(time.Second)
		for response.TargetID == "" && time.Now().Before(deadline) {
			response, err = manager.Download(context.Background(), DownloadRequest{SessionID: first.SessionID, Action: "query", DownloadID: download.ID})
			time.Sleep(10 * time.Millisecond)
		}
		sources[string(data)] = response.TargetID
	}
	_, firstPage, err := manager.get(context.Background(), first.SessionID, first.PageID)
	if err != nil {
		t.Fatal(err)
	}
	_, secondPage, err := manager.get(context.Background(), first.SessionID, second.PageID)
	if err != nil {
		t.Fatal(err)
	}
	if sources["POST:form"] != firstPage.targetID || sources["GET:navigation"] != secondPage.targetID {
		t.Fatalf("sources=%v first=%s second=%s", sources, firstPage.targetID, secondPage.targetID)
	}
	for _, download := range listed {
		if download.TargetID == secondPage.targetID {
			_, err := manager.Download(context.Background(), DownloadRequest{SessionID: first.SessionID, PageID: first.PageID, Action: "query", DownloadID: download.ID})
			if err == nil {
				t.Fatal("cross-tab download accepted")
			}
		}
	}
}

func TestRealBrowserDownloadFromNestedOOPIFOpenShadow(t *testing.T) {
	leaf := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/file" {
			w.Header().Set("Content-Disposition", `attachment; filename="frame.txt"`)
			_, _ = w.Write([]byte("frame-download"))
			return
		}
		_, _ = w.Write([]byte(`<!doctype html><div id="host"></div><script>host.attachShadow({mode:'open'}).innerHTML='<a href="/file">frame download</a>'</script>`))
	}))
	listener, err := listenCrossSite("127.0.0.3:0")
	if err != nil {
		t.Fatal(err)
	}
	leaf.Listener = listener
	leaf.Start()
	defer leaf.Close()
	parent := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<!doctype html><iframe src="` + leaf.URL + `" style="width:600px;height:200px"></iframe>`))
	}))
	listener, err = listenCrossSite("127.0.0.2:0")
	if err != nil {
		t.Fatal(err)
	}
	parent.Listener = listener
	parent.Start()
	defer parent.Close()
	top := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<!doctype html><iframe src="` + parent.URL + `" style="width:700px;height:300px"></iframe>`))
	}))
	defer top.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true, DownloadDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: top.URL})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(context.Background(), SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	ref := smokeElementRef(t, snapshot, "link", "frame download")
	response := triggerSingleFixtureDownload(t, manager, DownloadRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: ref, Revision: snapshot.Revision, TimeoutMS: 5000})
	if response.State != DownloadCompleted {
		t.Fatalf("response=%+v", response)
	}
	data, err := os.ReadFile(response.Path)
	if err != nil || string(data) != "frame-download" {
		t.Fatalf("data=%q err=%v", data, err)
	}
	_, page, err := manager.get(context.Background(), opened.SessionID, opened.PageID)
	if err != nil {
		t.Fatal(err)
	}
	if response.TargetID != page.targetID || response.FrameID == page.targetID {
		t.Fatalf("frame identity lost: response=%+v target=%s", response, page.targetID)
	}
}

func TestRealBrowserSDKNavigationDownloadKeepsSession(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/file" {
			requests.Add(1)
			w.Header().Set("Content-Disposition", `attachment; filename="navigation.txt"`)
			_, _ = w.Write([]byte("native-navigation-download"))
			return
		}
		_, _ = w.Write([]byte(`<!doctype html><title>Kept document</title><button>kept button</button>`))
	}))
	defer server.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true, DownloadDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	// 首次 Open 就是附件，仍返回可查询下载的 session/page。
	opened, err := manager.Open(context.Background(), OpenRequest{URL: server.URL + "/file"})
	if err != nil || opened.LoadStatus != OpenLoadDownload || opened.SessionID == "" || opened.PageID == "" || opened.URL != "about:blank" {
		t.Fatalf("initial attachment=%+v err=%v", opened, err)
	}
	first := awaitSmokeDownload(t, manager, opened.SessionID, 1)[0]
	completed, err := manager.Download(context.Background(), DownloadRequest{SessionID: opened.SessionID, Action: "wait", DownloadID: first.ID, TimeoutMS: 5000})
	if err != nil || completed.State != DownloadCompleted {
		t.Fatalf("initial download=%+v err=%v", completed, err)
	}
	navigated, err := manager.Navigate(context.Background(), NavigateRequest{SessionID: opened.SessionID, PageID: opened.PageID, URL: server.URL})
	if err != nil || navigated.LoadStatus != OpenLoadLoaded {
		t.Fatalf("document navigation=%+v err=%v", navigated, err)
	}
	snapshot, err := manager.Snapshot(context.Background(), SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	keptRef := smokeElementRef(t, snapshot, "button", "kept button")
	navigated, err = manager.Navigate(context.Background(), NavigateRequest{SessionID: opened.SessionID, PageID: opened.PageID, URL: server.URL + "/file"})
	if err != nil || navigated.LoadStatus != OpenLoadDownload || navigated.URL != server.URL+"/" {
		t.Fatalf("download navigation=%+v err=%v", navigated, err)
	}
	if _, err := manager.Click(context.Background(), ClickRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: keptRef, Revision: snapshot.Revision}); err != nil {
		t.Fatalf("unchanged document ref lost: %v", err)
	}
	listed := awaitSmokeDownload(t, manager, opened.SessionID, 2)
	for _, info := range listed {
		response, err := manager.Download(context.Background(), DownloadRequest{SessionID: opened.SessionID, Action: "wait", DownloadID: info.ID, TimeoutMS: 5000})
		if err != nil || response.State != DownloadCompleted {
			t.Fatalf("download=%+v err=%v", response, err)
		}
	}
	reused, err := manager.Open(context.Background(), OpenRequest{SessionID: opened.SessionID, URL: server.URL + "/file"})
	if err != nil || reused.LoadStatus != OpenLoadDownload || reused.URL != server.URL+"/" {
		t.Fatalf("reused open=%+v err=%v", reused, err)
	}
	tab, err := manager.NewTab(context.Background(), OpenRequest{SessionID: opened.SessionID, URL: server.URL + "/file"})
	if err != nil || tab.LoadStatus != OpenLoadDownload || tab.PageID == opened.PageID {
		t.Fatalf("new tab attachment=%+v err=%v", tab, err)
	}
	awaitSmokeDownload(t, manager, opened.SessionID, 4)
	if requests.Load() != 4 {
		t.Fatalf("unexpected requests: %d", requests.Load())
	}
}

func TestRealBrowserDownloadStartDialogDoesNotConfirmClick(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<!doctype html><button style="margin:100px" onmouseenter="alert('hover first')" onclick="document.body.dataset.clicked='yes'">download</button>`))
	}))
	defer site.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true, AllowEvaluate: true, DownloadDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: site.URL})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(context.Background(), SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	response, err := manager.Download(context.Background(), DownloadRequest{SessionID: opened.SessionID, PageID: opened.PageID, Action: "start", Ref: smokeElementRef(t, snapshot, "button", "download"), Revision: snapshot.Revision})
	var browserErr *Error
	if !errors.As(err, &browserErr) || browserErr.Kind != "download_unconfirmed" || response.Triggered || response.DownloadID != "" || response.Path != "" || response.Ref == "" {
		t.Fatalf("dialog start=%+v err=%v", response, err)
	}
	_, page, err := manager.get(context.Background(), opened.SessionID, opened.PageID)
	if err != nil {
		t.Fatal(err)
	}
	dialog := pendingPageDialog(page.page)
	if dialog == nil {
		t.Fatal("hover fixture did not open dialog")
	}
	if _, err := manager.HandleDialog(context.Background(), HandleDialogRequest{SessionID: opened.SessionID, PageID: opened.PageID, DialogID: dialog.ID, Action: "accept"}); err != nil {
		t.Fatal(err)
	}
	value, err := manager.Evaluate(context.Background(), EvaluateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Code: `document.body.dataset.clicked || 'no'`})
	if err != nil || string(value.Value) != `"no"` {
		t.Fatalf("fixture click unexpectedly executed: value=%s err=%v", value.Value, err)
	}
}
