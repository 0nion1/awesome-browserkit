package browserkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeSession struct {
	page    *fakePage
	closed  bool
	options PageOptions
}

type fakeViewportSession struct {
	*fakeSession
	viewportCalls []Viewport
	viewportErr   error
	closedTargets []string
	activated     []string
	tabs          []TabInfo
}

// fakeExternalSession 模拟外部浏览器：PageForTab 仍由 fakeViewportSession 提供，
// ActiveTab 用来模拟 Chrome 中真正获得焦点的页面。
type fakeExternalSession struct {
	*fakeViewportSession
	activeTarget string
	activeErr    error
}

// fakeConnectionSession 模拟底层 CDP 连接断开。
type fakeConnectionSession struct {
	*fakeViewportSession
	done   chan struct{}
	closes chan struct{}
}

func (session *fakeConnectionSession) Done() <-chan struct{} { return session.done }

func (session *fakeConnectionSession) Close() error {
	session.closes <- struct{}{}
	return session.fakeViewportSession.Close()
}

func (session *fakeExternalSession) ActiveTab(context.Context, []TabInfo) (string, error) {
	return session.activeTarget, session.activeErr
}

type blockingCloseSession struct {
	*fakeSession
	closeEntered chan struct{}
	releaseClose chan struct{}
}

func (session *blockingCloseSession) Close() error {
	close(session.closeEntered)
	<-session.releaseClose
	return session.fakeSession.Close()
}

// blockingListViewportSession 让测试精确控制 ListTabs 持有 session.mu 的时间，
// 用于复现图流订阅与 viewport 设置之间的锁顺序问题。
type blockingListViewportSession struct {
	*fakeViewportSession
	listEntered chan struct{}
	releaseList chan struct{}
	listOnce    sync.Once
	blockList   bool
}

func (session *blockingListViewportSession) ListTabs(ctx context.Context) ([]TabInfo, error) {
	if !session.blockList {
		return []TabInfo{{TargetID: "target-1", URL: session.page.info.URL, Title: session.page.info.Title, Type: "page"}}, nil
	}
	session.listOnce.Do(func() { close(session.listEntered) })
	select {
	case <-session.releaseList:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return []TabInfo{{TargetID: "target-1", URL: session.page.info.URL, Title: session.page.info.Title, Type: "page"}}, nil
}

func (session *fakeViewportSession) ListTabs(context.Context) ([]TabInfo, error) {
	if session.tabs != nil {
		return append([]TabInfo(nil), session.tabs...), nil
	}
	return []TabInfo{{TargetID: "target-1", URL: session.page.info.URL, Title: session.page.info.Title, Type: "page"}}, nil
}

func (session *fakeViewportSession) PageForTab(context.Context, string) (Page, error) {
	return session.page, nil
}

func (session *fakeViewportSession) ActivateTab(_ context.Context, targetID string) error {
	session.activated = append(session.activated, targetID)
	return nil
}
func (session *fakeViewportSession) CloseTab(_ context.Context, targetID string) error {
	session.closedTargets = append(session.closedTargets, targetID)
	return nil
}
func (session *fakeViewportSession) SetTabViewport(_ context.Context, _ string, viewport Viewport) error {
	session.viewportCalls = append(session.viewportCalls, viewport)
	return session.viewportErr
}

func (session *fakeSession) NewPage(_ context.Context, options PageOptions) (Page, error) {
	session.options = options
	if session.page == nil {
		session.page = &fakePage{info: PageInfo{URL: "about:blank", Title: "blank"}}
	}
	return session.page, nil
}
func (session *fakeSession) Cookies() ([]Cookie, error) { return nil, nil }
func (session *fakeSession) Close() error               { session.closed = true; return nil }
func (session *fakeSession) Closed() bool               { return session.closed }

type fakePage struct {
	snapshotData     *Snapshot
	snapshotCalls    int
	clickCalls       int
	clickPointCalled bool
	clickPointX      float64
	clickPointY      float64
	clickErr         error
	clickOptions     ClickOptions
	pressOptions     PressOptions
	checked          bool
	dragSource       ElementRef
	dragTarget       ElementRef
	dragByCalled     bool
	dragByDeltaX     float64
	dragByDeltaY     float64
	dragPointCalled  bool
	dragPointX       float64
	dragPointY       float64
	info             PageInfo
	closed           bool
	navigated        string
	lastText         string
	lastClear        bool
	lastRef          ElementRef
	waitCalled       bool
	waitOptions      WaitOptions
	waitBlock        <-chan struct{}
	hoverCalled      bool
	hoverBlock       <-chan struct{}
	clickBlock       <-chan struct{}
	injectedScripts  []string
	evaluatedScripts []string
	idleCalled       bool
	idleErr          error
	idleTimeout      time.Duration
	waitErr          error
	snapshotErr      error
	downloadInfo     DownloadInfo
	downloadErr      error
	downloadCalled   bool
	downloadDir      string
	navigateResult   *PageInfo
	inputMu          sync.Mutex
	waitMu           sync.Mutex
	inputBlock       <-chan struct{}
	inputs           []InputEvent
	screenshotData   []byte
	viewport         Viewport
	snapshotHook     func()
	screenshotHook   func() error
	refExists        *bool
	hasRefBlock      <-chan struct{}
	hasRefCalls      int
	historyDirection string
	historyInfo      PageInfo
	historyErr       error
	historyNavigated bool
}

type fakeScreencastPage struct {
	*fakePage
	starts  chan chan Screenshot
	options chan ScreencastOptions
}

type fakeTargetPage struct {
	*fakePage
	targetID string
}

func (page *fakeTargetPage) TargetID() string { return page.targetID }

func (page *fakeScreencastPage) StartScreencast(_ context.Context, options ScreencastOptions) (<-chan Screenshot, func(), error) {
	frames := make(chan Screenshot, 1)
	page.starts <- frames
	if page.options != nil {
		page.options <- options
	}
	var once sync.Once
	return frames, func() { once.Do(func() { close(frames) }) }, nil
}

func (page *fakePage) TargetID() string { return "target-1" }

func (page *fakePage) Info(context.Context) (PageInfo, error) { return page.info, nil }
func (page *fakePage) Navigate(_ context.Context, address string) (PageInfo, error) {
	page.navigated = address
	page.info.URL = address
	if page.navigateResult != nil {
		return *page.navigateResult, nil
	}
	return page.info, nil
}
func (page *fakePage) Snapshot(context.Context, SnapshotOptions) (Snapshot, error) {
	page.snapshotCalls++
	if page.snapshotHook != nil {
		page.snapshotHook()
	}
	if page.snapshotData != nil {
		return *page.snapshotData, page.snapshotErr
	}
	return Snapshot{Info: page.info, Revision: 1, Text: "hello", Elements: []Element{{Ref: "e1", Role: "button", Name: "Save", Locator: ElementLocator{Selector: "#save", XPath: `//*[@id="save"]`, Role: "button", Name: "Save"}}}}, page.snapshotErr
}
func (page *fakePage) Click(ctx context.Context, ref ElementRef, options ClickOptions) error {
	if page.clickBlock != nil {
		select {
		case <-page.clickBlock:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	page.clickOptions = options
	page.lastRef = ref
	page.clickCalls++
	return page.clickErr
}
func (page *fakePage) ClickPoint(_ context.Context, x, y float64, options ClickOptions) error {
	page.clickPointCalled = true
	page.clickPointX, page.clickPointY = x, y
	page.clickOptions = options
	page.clickCalls++
	return page.clickErr
}

func (page *fakePage) DragBy(_ context.Context, source ElementRef, dx, dy float64) error {
	page.dragByCalled = true
	page.dragSource = source
	page.dragByDeltaX, page.dragByDeltaY = dx, dy
	return nil
}

func (page *fakePage) DragPoint(_ context.Context, x, y, dx, dy float64) error {
	page.dragPointCalled = true
	page.dragPointX, page.dragPointY = x, y
	page.dragByDeltaX, page.dragByDeltaY = dx, dy
	return nil
}

func (page *fakePage) Hover(ctx context.Context, ref ElementRef) error {
	page.lastRef = ref
	page.hoverCalled = true
	if page.hoverBlock != nil {
		select {
		case <-page.hoverBlock:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
func (page *fakePage) Type(_ context.Context, ref ElementRef, text string, clear bool) error {
	page.lastRef, page.lastText, page.lastClear = ref, text, clear
	return nil
}
func (page *fakePage) Press(_ context.Context, options PressOptions) error {
	page.pressOptions = options
	return nil
}
func (page *fakePage) SetChecked(_ context.Context, ref ElementRef, checked bool) (bool, error) {
	page.lastRef = ref
	changed := page.checked != checked
	page.checked = checked
	return changed, nil
}
func (page *fakePage) Drag(_ context.Context, source, target ElementRef) error {
	page.dragSource = source
	page.dragTarget = target
	return nil
}
func (page *fakePage) DragTo(_ context.Context, source ElementRef, selector string) error {
	page.dragSource = source
	page.dragTarget = ElementRef{Ref: selector}
	return nil
}
func (page *fakePage) Wait(ctx context.Context, options WaitOptions) error {
	page.waitMu.Lock()
	defer page.waitMu.Unlock()
	page.waitCalled = true
	page.waitOptions = options
	if page.waitBlock != nil {
		select {
		case <-page.waitBlock:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return page.waitErr
}

func (page *fakePage) HasRef(ctx context.Context, _ string) (bool, error) {
	page.hasRefCalls++
	if page.hasRefBlock != nil {
		select {
		case <-page.hasRefBlock:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	if page.refExists != nil {
		return *page.refExists, nil
	}
	return true, nil
}

func (page *fakePage) HistoryTarget(_ context.Context, direction string) (HistoryEntry, error) {
	page.historyDirection = direction
	if page.historyErr != nil {
		return HistoryEntry{}, page.historyErr
	}
	address := page.historyInfo.URL
	if address == "" {
		address = page.info.URL
	}
	return HistoryEntry{ID: 1, URL: address}, nil
}

func (page *fakePage) NavigateHistory(_ context.Context, entry HistoryEntry) (PageInfo, error) {
	page.historyNavigated = true
	if page.historyErr != nil {
		return PageInfo{}, page.historyErr
	}
	if entry.URL != "" {
		page.info.URL = entry.URL
		page.info.Title = page.historyInfo.Title
	}
	return page.info, nil
}
func (page *fakePage) WaitIdle(_ context.Context, timeout time.Duration) error {
	page.waitMu.Lock()
	defer page.waitMu.Unlock()
	page.idleCalled = true
	page.idleTimeout = timeout
	return page.idleErr
}
func (page *fakePage) Download(_ context.Context, _ ElementRef, dir string) (DownloadInfo, error) {
	page.waitMu.Lock()
	defer page.waitMu.Unlock()
	page.downloadCalled = true
	page.downloadDir = dir
	return page.downloadInfo, page.downloadErr
}
func (page *fakePage) Screenshot(context.Context, ScreenshotOptions) (Screenshot, error) {
	if page.screenshotHook != nil {
		return Screenshot{}, page.screenshotHook()
	}
	data := page.screenshotData
	if len(data) == 0 {
		data = []byte("png")
	}
	return Screenshot{Data: data, Format: "png"}, nil
}
func (page *fakePage) Evaluate(_ context.Context, code string, _ EvaluateOptions) (EvaluateResult, error) {
	page.evaluatedScripts = append(page.evaluatedScripts, code)
	if page.viewport.Width > 0 && page.viewport.Height > 0 {
		data, _ := json.Marshal(page.viewport)
		return EvaluateResult{Type: "object", Value: data}, nil
	}
	return EvaluateResult{Type: "string", Value: []byte(`"ok"`)}, nil
}
func (page *fakePage) AddScriptToEvaluateOnNewDocument(_ context.Context, code string) error {
	page.injectedScripts = append(page.injectedScripts, code)
	return nil
}
func (page *fakePage) Close(context.Context) error { page.closed = true; return nil }
func (page *fakePage) DispatchInput(ctx context.Context, input InputEvent) error {
	if page.inputBlock != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-page.inputBlock:
		}
	}
	page.inputMu.Lock()
	page.inputs = append(page.inputs, input)
	page.inputMu.Unlock()
	return nil
}

func TestManagerLiveInputIsBestEffort(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com", Viewport: Viewport{Width: 800, Height: 600}})
	if err != nil {
		t.Fatal(err)
	}
	blocked := make(chan struct{})
	session.page.inputBlock = blocked
	defer close(blocked)
	started := time.Now()
	for range liveInputQueueSize + 32 {
		if err := manager.DispatchLiveInput(context.Background(), opened.PageID, InputEvent{Kind: "mouse", Action: "move", X: 1, Y: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("best-effort 输入被后台 CDP 阻塞: %s", elapsed)
	}
}

func TestManagerStateSubscriptionReceivesInternalBrowserSessionEvents(t *testing.T) {
	manager, _ := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates, err := manager.SubscribeState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	manager.emit(context.Background(), Event{Kind: "tab_opened", SessionID: "bsess-internal"})
	select {
	case <-updates:
	case <-time.After(time.Second):
		t.Fatal("顶层 Session 的状态订阅没有收到内部 Browser session 事件")
	}
}

func TestManagerDropsAndCancelsInputsFromPreviousActivePage(t *testing.T) {
	manager, browser := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com/a", Viewport: Viewport{Width: 800, Height: 600}})
	if err != nil {
		t.Fatal(err)
	}
	blocked := make(chan struct{})
	defer close(blocked)
	browser.page.inputBlock = blocked
	current := manager.sessions[opened.SessionID]
	secondPage := &fakePage{info: PageInfo{URL: "https://example.com/b"}}
	current.mu.Lock()
	second := manager.registerPage(current, secondPage, Viewport{Width: 800, Height: 600}, "")
	current.mu.Unlock()
	beforeSwitch, err := manager.ListTabs(context.Background(), TabsRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}

	first := InputEvent{Kind: "mouse", Action: "down", X: 10, Y: 10, Button: "left"}
	if err := manager.DispatchLiveInput(context.Background(), opened.PageID, first); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		current.mu.Lock()
		started := current.inputCancel != nil
		current.mu.Unlock()
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("首个输入没有进入后台 dispatcher")
		}
		time.Sleep(time.Millisecond)
	}
	if err := manager.DispatchLiveInput(context.Background(), opened.PageID, InputEvent{Kind: "text", Text: "stale"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ActivateTab(context.Background(), TabRequest{SessionID: opened.SessionID, PageID: second.id}); err != nil {
		t.Fatal(err)
	}
	tabs, err := manager.ListTabs(context.Background(), TabsRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if tabs.ActivePageID != second.id || tabs.TabsRevision <= beforeSwitch.TabsRevision {
		t.Fatalf("切页提交状态没有单调 revision: %+v, before=%d", tabs, beforeSwitch.TabsRevision)
	}
	if err := manager.DispatchLiveInput(context.Background(), opened.PageID, InputEvent{Kind: "text", Text: "also stale"}); err != nil {
		t.Fatal(err)
	}
	if err := manager.DispatchLiveInput(context.Background(), second.id, InputEvent{Kind: "text", Text: "current"}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(time.Second)
	for {
		secondPage.inputMu.Lock()
		gotSecond := append([]InputEvent(nil), secondPage.inputs...)
		secondPage.inputMu.Unlock()
		if len(gotSecond) == 1 {
			if gotSecond[0].Text != "current" {
				t.Fatalf("新页面输入=%+v", gotSecond)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("新页面没有收到输入: %+v", gotSecond)
		}
		time.Sleep(time.Millisecond)
	}
	browser.page.inputMu.Lock()
	gotFirst := append([]InputEvent(nil), browser.page.inputs...)
	browser.page.inputMu.Unlock()
	if len(gotFirst) != 0 {
		t.Fatalf("切页后仍回放旧页面输入: %+v", gotFirst)
	}
}

func TestManagerRestartsLiveFramesWhenActiveTabChanges(t *testing.T) {
	manager, _ := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{
		URL: "https://example.com/a", Viewport: Viewport{Width: 800, Height: 600},
	})
	if err != nil {
		t.Fatal(err)
	}
	current := manager.sessions[opened.SessionID]
	secondPage := &fakePage{info: PageInfo{URL: "https://example.com/b"}}
	current.mu.Lock()
	first := current.tabs[opened.PageID]
	second := manager.registerPage(current, secondPage, Viewport{Width: 800, Height: 600}, "")
	current.mu.Unlock()
	cachePageLiveFrame(first, LiveFrame{Data: []byte("first"), MIME: "image/jpeg"})
	cachePageLiveFrame(second, LiveFrame{Data: []byte("second"), MIME: "image/jpeg"})

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	firstFrames, err := manager.SubscribeLiveFrames(firstCtx)
	if err != nil {
		t.Fatal(err)
	}
	if frame := <-firstFrames; string(frame.Data) != "first" {
		t.Fatalf("切换前画面=%q", frame.Data)
	}
	if _, err := manager.ActivateTab(context.Background(), TabRequest{SessionID: opened.SessionID, PageID: second.id}); err != nil {
		t.Fatal(err)
	}
	select {
	case _, ok := <-firstFrames:
		if ok {
			t.Fatal("切换 Tab 后旧图流仍然打开")
		}
	case <-time.After(time.Second):
		t.Fatal("切换 Tab 后旧图流没有关闭")
	}

	secondCtx, cancelSecond := context.WithCancel(context.Background())
	defer cancelSecond()
	secondFrames, err := manager.SubscribeLiveFrames(secondCtx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case frame := <-secondFrames:
		if string(frame.Data) != "second" {
			t.Fatalf("切换后画面=%q", frame.Data)
		}
	case <-time.After(time.Second):
		t.Fatal("切换后没有收到新 Tab 缓存画面")
	}
}

func TestCloseActiveTabDoesNotWaitForLiveHubWhileHoldingTabLock(t *testing.T) {
	manager, _ := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	hubCtx, stop := context.WithCancel(context.Background())
	manager.liveMu.Lock()
	manager.live = &liveHub{manager: manager, ctx: hubCtx, stop: stop, done: make(chan struct{})}
	manager.liveMu.Unlock()

	started := time.Now()
	if _, err := manager.CloseTab(context.Background(), TabRequest{SessionID: opened.SessionID, PageID: opened.PageID}); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("关闭 active Tab 等待了未退出的 live hub: %s", elapsed)
	}
}

func adjustedTiming(change func(*timing)) timing {
	value := defaultTiming()
	change(&value)
	return value
}

func fakeManager(t *testing.T, config Config) (*Manager, *fakeSession) {
	return fakeManagerWithTiming(t, config, defaultTiming())
}

func fakeManagerWithTiming(t *testing.T, config Config, timing timing) (*Manager, *fakeSession) {
	t.Helper()
	session := &fakeSession{}
	config.Factory = func(_ context.Context, _ SessionConfig) (Session, error) {
		return session, nil
	}
	manager, err := newManager(config, timing)
	if err != nil {
		t.Fatal(err)
	}
	return manager, session
}

func TestManagerDefaultsViewportToFullHD(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	if _, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"}); err != nil {
		t.Fatal(err)
	}
	if got := session.options.Viewport; got != (Viewport{Width: 1920, Height: 1080}) {
		t.Fatalf("默认 viewport=%#v", got)
	}
}

func TestManagerDispatchesValidatedLiveInputToActivePage(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{
		URL: "https://example.com", Viewport: Viewport{Width: 800, Height: 600},
	})
	if err != nil {
		t.Fatal(err)
	}
	events := []InputEvent{
		{Kind: "mouse", Action: "down", X: 400, Y: 300, Button: "left", Buttons: 1, ClickCount: 1},
		{Kind: "touch", Action: "start", Touches: []InputTouchPoint{{ID: 7, X: 200, Y: 150, Force: 0.75}}},
		{Kind: "key", Action: "down", Key: "A", Code: "KeyA", KeyCode: 65, Text: "A"},
		{Kind: "composition", Text: "中文", SelectionStart: 2, SelectionEnd: 2},
		{Kind: "text", Text: "中文"},
		{Kind: "shortcut", Action: "select_all"},
		{Kind: "shortcut", Action: "copy"},
		{Kind: "shortcut", Action: "paste"},
	}
	for _, event := range events {
		if err := manager.DispatchLiveInput(context.Background(), opened.PageID, event); err != nil {
			t.Fatalf("DispatchLiveInput(%+v): %v", event, err)
		}
	}
	deadline := time.Now().Add(time.Second)
	for {
		session.page.inputMu.Lock()
		inputs := append([]InputEvent(nil), session.page.inputs...)
		session.page.inputMu.Unlock()
		if len(inputs) == len(events) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("页面输入=%+v", inputs)
		}
		time.Sleep(time.Millisecond)
	}
	if err := manager.DispatchLiveInput(context.Background(), "", InputEvent{Kind: "text", Text: "active"}); err != nil {
		t.Fatalf("未指定 page_id 的 active page 输入失败: %v", err)
	}
	deadline = time.Now().Add(time.Second)
	for {
		session.page.inputMu.Lock()
		inputs := append([]InputEvent(nil), session.page.inputs...)
		session.page.inputMu.Unlock()
		if len(inputs) == len(events)+1 {
			if inputs[len(events)].Text != "active" {
				t.Fatalf("active page 输入=%+v", inputs[len(events)])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("未指定 page_id 的输入没有到达 active page: %+v", inputs)
		}
		time.Sleep(time.Millisecond)
	}
	if err := manager.DispatchLiveInput(context.Background(), opened.PageID, InputEvent{Kind: "mouse", Action: "move", X: 800, Y: 300}); err == nil {
		t.Fatal("没有拒绝 viewport 右边界之外的坐标")
	}
	if err := manager.DispatchLiveInput(context.Background(), opened.PageID, InputEvent{Kind: "key", Action: "down", Key: "a", Code: "KeyA", Modifiers: 2}); err == nil {
		t.Fatal("没有拒绝直接透传 Ctrl 组合键")
	}
	if err := manager.DispatchLiveInput(context.Background(), opened.PageID, InputEvent{Kind: "shortcut", Action: "cut"}); err == nil {
		t.Fatal("没有拒绝未允许的语义化快捷键")
	}
	if err := manager.DispatchLiveInput(context.Background(), opened.PageID, InputEvent{Kind: "shortcut", Action: "copy", Modifiers: 1}); err == nil {
		t.Fatal("没有拒绝快捷键不支持的修饰键")
	}
}

func TestManagerSetsTabViewportAfterBrowserAcceptsIt(t *testing.T) {
	base := &fakeSession{page: &fakePage{info: PageInfo{URL: "about:blank", Title: "blank"}}}
	session := &fakeViewportSession{fakeSession: base}
	manager, err := newManager(Config{
		AllowPrivateNetwork: true,
		Factory:             func(context.Context, SessionConfig) (Session, error) { return session, nil },
	}, adjustedTiming(func(value *timing) { value.tabReconcileTimeout = 20 * time.Millisecond }))
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com", Viewport: Viewport{Width: 800, Height: 600}})
	if err != nil {
		t.Fatal(err)
	}
	next := Viewport{Width: 927, Height: 641}
	if err := manager.SetViewport(context.Background(), ViewportRequest{SessionID: opened.SessionID, PageID: opened.PageID, Viewport: next}); err != nil {
		t.Fatal(err)
	}
	if len(session.viewportCalls) != 1 || session.viewportCalls[0] != next {
		t.Fatalf("viewport calls=%+v", session.viewportCalls)
	}
	state, err := manager.LiveState(context.Background())
	if err != nil || state.Viewport != next {
		t.Fatalf("LiveState()=%+v, %v", state, err)
	}
	if err := manager.SetViewport(context.Background(), ViewportRequest{SessionID: opened.SessionID, PageID: opened.PageID, Viewport: next}); err != nil {
		t.Fatal(err)
	}
	if len(session.viewportCalls) != 1 {
		t.Fatalf("相同 viewport 被重复发送: %+v", session.viewportCalls)
	}
	// 页面可能被浏览器自身改成别的实际尺寸；记录值相同也要能强制重新下发。
	if err := manager.SetViewport(context.Background(), ViewportRequest{SessionID: opened.SessionID, PageID: opened.PageID, Viewport: next, Force: true}); err != nil {
		t.Fatal(err)
	}
	if len(session.viewportCalls) != 2 || session.viewportCalls[1] != next {
		t.Fatalf("强制 viewport calls=%+v", session.viewportCalls)
	}
}

func TestManagerClosesTabsBeyondConfiguredLimit(t *testing.T) {
	base := &fakeSession{page: &fakePage{info: PageInfo{URL: "about:blank", Title: "blank"}}}
	session := &fakeViewportSession{fakeSession: base}
	manager, err := NewManager(Config{
		AllowPrivateNetwork: true, MaxTabsPerSession: 1,
		Factory: func(context.Context, SessionConfig) (Session, error) { return session, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	session.tabs = []TabInfo{
		{TargetID: "target-1", URL: "https://example.com", Type: "page"},
		{TargetID: "target-2", URL: "https://example.com/popup", Type: "page"},
	}
	tabs, err := manager.ListTabs(context.Background(), TabsRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(tabs.Tabs) != 1 || len(session.closedTargets) != 1 || session.closedTargets[0] != "target-2" || len(session.activated) == 0 || session.activated[len(session.activated)-1] != "target-1" {
		t.Fatalf("tabs=%+v closed targets=%v activation calls=%v", tabs.Tabs, session.closedTargets, session.activated)
	}
}

func TestStateReconcileDoesNotBlockViewportOrLiveSubscription(t *testing.T) {
	base := &fakeSession{page: &fakePage{info: PageInfo{URL: "about:blank", Title: "blank"}}}
	session := &blockingListViewportSession{
		fakeViewportSession: &fakeViewportSession{fakeSession: base},
		listEntered:         make(chan struct{}),
		releaseList:         make(chan struct{}),
	}
	manager, err := newManager(Config{
		AllowPrivateNetwork: true,
		Factory:             func(context.Context, SessionConfig) (Session, error) { return session, nil },
	}, adjustedTiming(func(value *timing) { value.tabReconcileTimeout = 500 * time.Millisecond }))
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com", Viewport: Viewport{Width: 800, Height: 600}})
	if err != nil {
		t.Fatal(err)
	}
	session.blockList = true
	cachePageLiveFrame(manager.sessions[opened.SessionID].tabs[opened.PageID], LiveFrame{Data: []byte("cached"), MIME: "image/png"})
	reconcileDone := make(chan error, 1)
	go func() {
		_, listErr := manager.ListTabs(context.Background(), TabsRequest{SessionID: opened.SessionID})
		reconcileDone <- listErr
	}()
	select {
	case <-session.listEntered:
	case <-time.After(time.Second):
		t.Fatal("状态读取没有进入受控的 ListTabs")
	}

	viewportDone := make(chan error, 1)
	go func() {
		viewportDone <- manager.SetViewport(context.Background(), ViewportRequest{
			SessionID: opened.SessionID, PageID: opened.PageID,
			Viewport: Viewport{Width: 801, Height: 601},
		})
	}()
	streamCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	streamDone := make(chan error, 1)
	go func() {
		_, subscribeErr := manager.SubscribeLiveFrames(streamCtx)
		streamDone <- subscribeErr
	}()

	select {
	case err := <-viewportDone:
		if err != nil {
			t.Fatalf("SetViewport() error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("viewport 被状态 reconcile 阻塞")
	}
	select {
	case err := <-streamDone:
		if err != nil {
			t.Fatalf("SubscribeLiveFrames() error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("直播订阅被状态 reconcile 阻塞")
	}
	close(session.releaseList)
	select {
	case err := <-reconcileDone:
		if err != nil {
			t.Fatalf("ListTabs() error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("状态 reconcile 没有退出")
	}
}

func TestManagerKeepsOldViewportWhenBrowserRejectsResize(t *testing.T) {
	base := &fakeSession{page: &fakePage{info: PageInfo{URL: "about:blank"}}}
	session := &fakeViewportSession{fakeSession: base, viewportErr: errors.New("CDP failed")}
	manager, err := NewManager(Config{
		AllowPrivateNetwork: true,
		Factory:             func(context.Context, SessionConfig) (Session, error) { return session, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	initial := Viewport{Width: 800, Height: 600}
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com", Viewport: initial})
	if err != nil {
		t.Fatal(err)
	}
	err = manager.SetViewport(context.Background(), ViewportRequest{SessionID: opened.SessionID, PageID: opened.PageID, Viewport: Viewport{Width: 900, Height: 700}})
	if err == nil || !strings.Contains(err.Error(), "CDP failed") {
		t.Fatalf("SetViewport() error=%v", err)
	}
	state, stateErr := manager.LiveState(context.Background())
	if stateErr != nil || state.Viewport != initial {
		t.Fatalf("失败后 LiveState()=%+v, %v", state, stateErr)
	}
}

func TestManagerLiveFramesFallBackToScreenshots(t *testing.T) {
	manager, _ := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	if _, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	frames, err := manager.SubscribeLiveFrames(ctx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case frame := <-frames:
		if string(frame.Data) != "png" || frame.MIME != "image/png" {
			t.Fatalf("回退画面=%+v", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("没有收到截图回退画面")
	}
	cancel()
	select {
	case _, ok := <-frames:
		if ok {
			t.Fatal("取消订阅后图流仍然打开")
		}
	case <-time.After(time.Second):
		t.Fatal("取消订阅后图流没有关闭")
	}
}

func TestLiveScreencastRestartsWhenViewportChanges(t *testing.T) {
	manager, _ := fakeManagerWithTiming(t, Config{AllowPrivateNetwork: true}, adjustedTiming(func(value *timing) {
		value.liveFirstFrame = 20 * time.Millisecond
	}))
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{
		URL: "https://example.com", Viewport: Viewport{Width: 800, Height: 600},
	})
	if err != nil {
		t.Fatal(err)
	}
	current := manager.sessions[opened.SessionID]
	streamer := &fakeScreencastPage{fakePage: &fakePage{info: PageInfo{URL: opened.URL}}, starts: make(chan chan Screenshot, 2)}
	current.mu.Lock()
	current.tabs[opened.PageID].page = streamer
	current.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames, err := manager.SubscribeLiveFrames(ctx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-streamer.starts:
	case <-time.After(time.Second):
		t.Fatal("screencast 没有启动")
	}
	select {
	case frame := <-frames:
		if string(frame.Data) != "png" || frame.MIME != "image/png" {
			t.Fatalf("无首帧时的兜底画面=%q, %q", frame.Data, frame.MIME)
		}
	case <-time.After(time.Second):
		t.Fatal("screencast 无首帧时没有发布有界截图")
	}
	current.mu.Lock()
	current.tabs[opened.PageID].viewport = Viewport{Width: 801, Height: 601}
	current.mu.Unlock()
	select {
	case <-streamer.starts:
	case <-time.After(time.Second):
		t.Fatal("viewport 变化后 screencast 没有原地重启")
	}
	select {
	case frame := <-frames:
		if string(frame.Data) != "png" {
			t.Fatalf("重启后的直播帧=%q", frame.Data)
		}
	case <-time.After(time.Second):
		t.Fatal("重启后没有通过原 WebSocket 订阅发布新尺寸兜底帧")
	}
	// 先等待 live goroutine 退出，再由 Cleanup 恢复测试用的全局 timeout。
	// 仅 cancel 订阅会让 hub 从 Manager 脱钩，但不能保证 run 已经结束。
	if err := manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestLivePublishesImmediatelyAndThrottlesToFrameInterval(t *testing.T) {
	manager, _ := fakeManager(t, Config{AllowPrivateNetwork: true, LiveQuality: LiveQualityLow})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	current := manager.sessions[opened.SessionID]
	streamer := &fakeScreencastPage{fakePage: &fakePage{info: PageInfo{URL: opened.URL}}, starts: make(chan chan Screenshot, 1)}
	current.mu.Lock()
	current.tabs[opened.PageID].page = streamer
	current.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames, err := manager.SubscribeLiveFrames(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var stream chan Screenshot
	select {
	case stream = <-streamer.starts:
	case <-time.After(time.Second):
		t.Fatal("screencast 没有启动")
	}
	stream <- Screenshot{Data: []byte("first"), Format: "jpeg"}
	select {
	case frame := <-frames:
		if string(frame.Data) != "first" {
			t.Fatalf("首帧=%q", frame.Data)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("首帧没有立即发布")
	}
	stream <- Screenshot{Data: []byte("second"), Format: "jpeg"}
	select {
	case frame := <-frames:
		t.Fatalf("不足帧间隔时提前发布=%q", frame.Data)
	case <-time.After(20 * time.Millisecond):
	}
	select {
	case frame := <-frames:
		if string(frame.Data) != "second" {
			t.Fatalf("节流后的最新帧=%q", frame.Data)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("最新帧没有在帧间隔后发布")
	}
}

func TestLivePublishesImmediatelyDuringInputBurst(t *testing.T) {
	manager, _ := fakeManager(t, Config{AllowPrivateNetwork: true, LiveQuality: LiveQualityLow})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	current := manager.sessions[opened.SessionID]
	streamer := &fakeScreencastPage{fakePage: &fakePage{info: PageInfo{URL: opened.URL}}, starts: make(chan chan Screenshot, 1)}
	current.mu.Lock()
	current.tabs[opened.PageID].page = streamer
	current.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames, err := manager.SubscribeLiveFrames(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var stream chan Screenshot
	select {
	case stream = <-streamer.starts:
	case <-time.After(time.Second):
		t.Fatal("screencast 没有启动")
	}
	stream <- Screenshot{Data: []byte("first"), Format: "jpeg"}
	select {
	case <-frames:
	case <-time.After(time.Second):
		t.Fatal("首帧没有发布")
	}
	if err := manager.DispatchLiveInput(ctx, opened.PageID, InputEvent{Kind: "mouse", Action: "move", X: 10, Y: 10}); err != nil {
		t.Fatal(err)
	}
	stream <- Screenshot{Data: []byte("burst"), Format: "jpeg"}
	select {
	case frame := <-frames:
		if string(frame.Data) != "burst" {
			t.Fatalf("交互突发帧=%q", frame.Data)
		}
	case <-time.After(25 * time.Millisecond):
		t.Fatal("输入后帧仍等待档位间隔")
	}
	time.Sleep(liveInteractiveWindow + 20*time.Millisecond)
	stream <- Screenshot{Data: []byte("baseline"), Format: "jpeg"}
	select {
	case <-frames:
	case <-time.After(time.Second):
		t.Fatal("突发结束后的基准帧没有发布")
	}
	stream <- Screenshot{Data: []byte("throttled"), Format: "jpeg"}
	select {
	case frame := <-frames:
		t.Fatalf("突发结束后提前发布=%q", frame.Data)
	case <-time.After(15 * time.Millisecond):
	}
	select {
	case frame := <-frames:
		if string(frame.Data) != "throttled" {
			t.Fatalf("节流帧=%q", frame.Data)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("突发结束后没有恢复档位发布")
	}
}

func TestLiveScreencastAppliesDefaultSourceCompression(t *testing.T) {
	manager, _ := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	current := manager.sessions[opened.SessionID]
	streamer := &fakeScreencastPage{
		fakePage: &fakePage{info: PageInfo{URL: opened.URL}},
		starts:   make(chan chan Screenshot, 1),
		options:  make(chan ScreencastOptions, 1),
	}
	current.mu.Lock()
	current.tabs[opened.PageID].page = streamer
	current.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := manager.SubscribeLiveFrames(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case options := <-streamer.options:
		if options.Quality != 90 || options.EveryNthFrame != 2 {
			t.Fatalf("默认 screencast options=%+v", options)
		}
	case <-time.After(time.Second):
		t.Fatal("screencast 没有启动")
	}
}

func TestManagerLiveFramesFollowActiveSession(t *testing.T) {
	created := []*fakeSession{
		{page: &fakePage{info: PageInfo{URL: "about:blank"}, screenshotData: []byte("first")}},
		{page: &fakePage{info: PageInfo{URL: "about:blank"}, screenshotData: []byte("second")}},
	}
	next := 0
	manager, err := NewManager(Config{AllowPrivateNetwork: true, Factory: func(context.Context, SessionConfig) (Session, error) {
		session := created[next]
		next++
		return session, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	if _, err := manager.Open(context.Background(), OpenRequest{URL: "https://one.example"}); err != nil {
		t.Fatal(err)
	}
	firstCtx, firstCancel := context.WithCancel(context.Background())
	defer firstCancel()
	firstFrames, err := manager.SubscribeLiveFrames(firstCtx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case frame := <-firstFrames:
		if string(frame.Data) != "first" {
			t.Fatalf("第一条图流=%q", frame.Data)
		}
	case <-time.After(time.Second):
		t.Fatal("没有收到第一条图流")
	}
	if _, err := manager.Open(context.Background(), OpenRequest{URL: "https://two.example"}); err != nil {
		t.Fatal(err)
	}
	select {
	case _, ok := <-firstFrames:
		if ok {
			t.Fatal("切换 active session 后旧图流没有关闭")
		}
	case <-time.After(time.Second):
		t.Fatal("切换 active session 后旧图流关闭超时")
	}
	secondCtx, secondCancel := context.WithCancel(context.Background())
	defer secondCancel()
	secondFrames, err := manager.SubscribeLiveFrames(secondCtx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case frame := <-secondFrames:
		if string(frame.Data) != "second" {
			t.Fatalf("第二条图流=%q", frame.Data)
		}
	case <-time.After(time.Second):
		t.Fatal("没有收到第二条图流")
	}
}

func TestManagerBlocksPrivateAndUnapprovedOrigins(t *testing.T) {
	manager, _ := fakeManager(t, Config{AllowedOrigins: []string{"https://example.com"}})
	for _, address := range []string{"http://127.0.0.1:8080", "http://localhost.:8080", "https://other.example"} {
		if _, err := manager.Open(context.Background(), OpenRequest{URL: address}); err == nil {
			t.Fatalf("Open(%q) 没有拒绝", address)
		} else {
			var structured *Error
			if !errors.As(err, &structured) || structured.Kind != "navigation_blocked" {
				t.Fatalf("Open(%q) 错误=%v", address, err)
			}
		}
	}
}

func TestManagerCloseIsIdempotent(t *testing.T) {
	manager, _ := fakeManager(t, Config{AllowPrivateNetwork: true})
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	session := manager.sessions[opened.SessionID].browser.(*fakeSession)
	ctx := context.Background()
	first, err := manager.CloseSession(ctx, CloseRequest{SessionID: opened.SessionID})
	if err != nil || !first.Closed {
		t.Fatalf("第一次关闭=%+v, %v", first, err)
	}
	second, err := manager.CloseSession(ctx, CloseRequest{SessionID: opened.SessionID})
	if err != nil || !second.Closed {
		t.Fatalf("重复关闭=%+v, %v", second, err)
	}
	if !session.closed {
		t.Fatal("关闭 session 没有释放 fake 浏览器")
	}
}

func TestCloseActivePublishesLogicalCloseBeforeBrowserCleanup(t *testing.T) {
	session := &blockingCloseSession{
		fakeSession:  &fakeSession{page: &fakePage{info: PageInfo{URL: "about:blank"}}},
		closeEntered: make(chan struct{}),
		releaseClose: make(chan struct{}),
	}
	manager, err := NewManager(Config{
		AllowPrivateNetwork: true,
		Factory: func(context.Context, SessionConfig) (Session, error) {
			return session, nil
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
	closed := make(chan error, 1)
	go func() { closed <- manager.CloseActive(context.Background()) }()
	select {
	case <-session.closeEntered:
	case <-time.After(time.Second):
		t.Fatal("Browser 清理没有开始")
	}
	select {
	case <-updates:
	case <-time.After(time.Second):
		t.Fatal("底层 Browser 清理阻塞时没有先发布逻辑关闭")
	}
	close(session.releaseClose)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}

func TestBrowserToolsUseRevisionAndDoNotEchoTypeText(t *testing.T) {
	manager, _ := fakeManager(t, Config{AllowPrivateNetwork: true})
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil || snapshot.Revision != 1 {
		t.Fatalf("快照=%+v, %v", snapshot, err)
	}
	response, err := manager.Type(ctx, TypeRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: "e1", Revision: snapshot.Revision, Text: "secret-password"})
	if err != nil {
		t.Fatal(err)
	}
	if response.TextLength != len([]rune("secret-password")) || response.Submitted {
		t.Fatalf("输入响应=%+v", response)
	}
}

func TestBrowserHoverUsesSnapshotReferenceWithoutExposingMoves(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	response, err := manager.Hover(ctx, HoverRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: snapshot.Elements[0].Ref, Revision: snapshot.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if !session.page.hoverCalled || response.Ref != snapshot.Elements[0].Ref || response.TabsRevision == 0 {
		t.Fatalf("hover 没有使用快照 ref 或返回稳定 Tab 元数据: response=%+v hover=%v", response, session.page.hoverCalled)
	}
}

func TestBrowserNavigateHasOneWaitBehavior(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	session.page.idleCalled = false
	if _, err := manager.Navigate(context.Background(), NavigateRequest{SessionID: opened.SessionID, PageID: opened.PageID, URL: "https://example.com/next"}); err != nil {
		t.Fatal(err)
	}
	if !session.page.idleCalled {
		t.Fatal("Manager.Navigate 没有执行统一的页面空闲等待")
	}
}

func TestTabStatusChangesTabsRevision(t *testing.T) {
	manager, _ := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	before, err := manager.ListTabs(context.Background(), TabsRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	current := manager.sessions[opened.SessionID]
	current.mu.Lock()
	current.tabs[opened.PageID].status = "loading"
	current.mu.Unlock()
	after, err := manager.ListTabs(context.Background(), TabsRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if after.TabsRevision <= before.TabsRevision {
		t.Fatalf("Tab status 改变没有推进 tabs_revision: before=%d after=%d", before.TabsRevision, after.TabsRevision)
	}
}

func TestBrowserHoverWaitsForSessionInteraction(t *testing.T) {
	manager, _ := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	current := manager.sessions[opened.SessionID]
	current.interactionMu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := manager.Hover(ctx, HoverRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: snapshot.Elements[0].Ref, Revision: snapshot.Revision})
		done <- err
	}()
	select {
	case err := <-done:
		current.interactionMu.Unlock()
		t.Fatalf("hover 没有等待 session 交互锁: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	current.interactionMu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("释放交互锁后 hover 没有继续")
	}
}

func TestBrowserSnapshotDefaultsToReconciledActiveTab(t *testing.T) {
	manager, _ := fakeManager(t, Config{AllowPrivateNetwork: true})
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	activeSnapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID})
	if err != nil || activeSnapshot.PageID != opened.PageID || activeSnapshot.ActivePageID != opened.PageID {
		t.Fatalf("browserSnapshot 缺少 page_id 应使用 active Tab: %+v, %v", activeSnapshot, err)
	}
	if _, err := manager.Type(ctx, TypeRequest{SessionID: opened.SessionID, Ref: "e1", Revision: 1, Text: "value"}); err == nil {
		t.Fatal("browserType 缺少 page_id 时不应回退到 active Tab")
	}
	if _, err := manager.Screenshot(ctx, ScreenshotRequest{SessionID: opened.SessionID}); err == nil {
		t.Fatal("browserScreenshot 缺少 page_id 时不应回退到 active Tab")
	}
	current := manager.sessions[opened.SessionID]
	current.mu.Lock()
	second := manager.registerPage(current, &fakePage{info: PageInfo{URL: "https://other.example"}}, defaultTiming().viewport, "")
	current.mu.Unlock()
	snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil || len(snapshot.Elements) == 0 {
		t.Fatalf("源 Tab 快照=%+v, %v", snapshot, err)
	}
	if _, err := manager.Type(ctx, TypeRequest{SessionID: opened.SessionID, PageID: second.id, Ref: snapshot.Elements[0].Ref, Revision: snapshot.Revision, Text: "value"}); err == nil {
		t.Fatal("来自另一个 Tab 的 ref 不应被接受")
	}
}

func TestPreferredTargetOrderKeepsRemainingTabs(t *testing.T) {
	listed := []TabInfo{
		{TargetID: "old-1"},
		{TargetID: "new-2"},
		{TargetID: "old-3"},
		{TargetID: "new-4"},
	}
	ordered := preferredTargetOrder(listed, []string{"new-2", "new-4"})
	ids := make([]string, 0, len(ordered))
	for _, tab := range ordered {
		ids = append(ids, tab.TargetID)
	}
	want := []string{"new-2", "new-4", "old-1", "old-3"}
	if !slices.Equal(ids, want) {
		t.Fatalf("target 顺序 = %v，期望 %v", ids, want)
	}
}

func TestExternalBrowserAdoptsWebsiteOpenedActiveTab(t *testing.T) {
	session := &fakeExternalSession{
		fakeViewportSession: &fakeViewportSession{fakeSession: &fakeSession{}},
		activeTarget:        "target-2",
	}
	manager, err := newManager(Config{
		AllowPrivateNetwork: true,
		Session:             SessionConfig{RemoteAddress: "ws://127.0.0.1:9222"},
		Factory: func(context.Context, SessionConfig) (Session, error) {
			return session, nil
		},
	}, adjustedTiming(func(value *timing) { value.tabReconcileTimeout = 300 * time.Millisecond }))
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	first, err := manager.ListTabs(ctx, TabsRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	// 网站自己新开 Tab 后，Chrome 已切到新页；外部浏览器不应被 Manager
	// 反向 ActivateTab 抢焦点，但本地 active 投影必须跟随真实焦点。
	session.tabs = []TabInfo{
		{TargetID: "target-1", URL: "https://example.com", Title: "example", Type: "page"},
		{TargetID: "target-2", URL: "https://popup.example", Title: "popup", Type: "page"},
	}
	tabs, err := manager.ListTabs(ctx, TabsRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if first.ActivePageID == tabs.ActivePageID || len(tabs.Tabs) != 2 {
		t.Fatalf("外部浏览器没有跟随新活动 Tab: first=%+v tabs=%+v", first, tabs)
	}
	if len(session.activated) != 0 {
		t.Fatalf("外部浏览器不应被主动激活: %v", session.activated)
	}
	if len(session.viewportCalls) != 0 {
		t.Fatalf("外部浏览器不应被改写视口: %v", session.viewportCalls)
	}
}

func TestBrowserSnapshotReportsStableTabRevisionAndNewActiveTab(t *testing.T) {
	session := &fakeViewportSession{fakeSession: &fakeSession{}}
	manager, err := newManager(Config{AllowPrivateNetwork: true, Factory: func(context.Context, SessionConfig) (Session, error) {
		return session, nil
	}}, adjustedTiming(func(value *timing) { value.tabReconcileTimeout = 300 * time.Millisecond }))
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	first, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if first.TabsRevision == 0 || second.TabsRevision != first.TabsRevision || second.TabsChanged {
		t.Fatalf("无 Tab 变化时 revision 应稳定且 tabs_changed=false: first=%+v second=%+v", first, second)
	}
	current := manager.sessions[opened.SessionID]
	current.mu.Lock()
	newTarget := "target-popup"
	current.mu.Unlock()
	session.tabs = []TabInfo{
		{TargetID: "target-1", URL: "https://example.com", Title: "example", Type: "page"},
		{TargetID: newTarget, URL: "https://popup.example", Title: "popup", Type: "page"},
	}
	third, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if third.TabsRevision <= second.TabsRevision || !third.TabsChanged || third.ActivePageID == first.ActivePageID || third.PageID != third.ActivePageID || len(third.Tabs) != 2 {
		t.Fatalf("新 Tab 应在 Snapshot 中被发现并成为 active: %+v", third)
	}
	stable := third
	deadline := time.Now().Add(time.Second)
	for activeTabStatus(stable) != "ready" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		stable, err = manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID})
		if err != nil {
			t.Fatal(err)
		}
	}
	if activeTabStatus(stable) != "ready" {
		t.Fatalf("新 Tab 没有进入 ready 状态: %+v", stable)
	}
	fourth, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if fourth.TabsRevision != stable.TabsRevision || fourth.TabsChanged {
		t.Fatalf("Tab ready 后重复 Snapshot 不应重复报告变化: stable=%+v fourth=%+v", stable, fourth)
	}
}

func TestBrowserSnapshotRefreshesTabProjectionFromPage(t *testing.T) {
	session := &fakeViewportSession{fakeSession: &fakeSession{}}
	session.tabs = []TabInfo{{TargetID: "target-1", URL: "https://example.com/old", Title: "old", Type: "page"}}
	manager, err := newManager(Config{AllowPrivateNetwork: true, Factory: func(context.Context, SessionConfig) (Session, error) {
		return session, nil
	}}, defaultTiming())
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com/old"})
	if err != nil {
		t.Fatal(err)
	}
	session.page.snapshotData = &Snapshot{
		Info:     PageInfo{URL: "https://example.com/new", Title: "new"},
		Revision: 2,
		Text:     "new page",
		Elements: []Element{{Ref: "e1", Role: "button", Name: "Next"}},
	}
	response, err := manager.Snapshot(context.Background(), SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	if response.URL != "https://example.com/new" || response.Title != "new" {
		t.Fatalf("快照没有返回页面新地址: %+v", response)
	}
	var tab *TabView
	for index := range response.Tabs {
		if response.Tabs[index].PageID == response.PageID {
			tab = &response.Tabs[index]
			break
		}
	}
	if tab == nil {
		t.Fatalf("快照结果缺少当前 Tab: %+v", response.Tabs)
	}
	if tab.URL != response.URL || tab.Title != response.Title {
		t.Fatalf("Tab 投影没有跟随页面快照更新: tab=%+v response.url=%q response.title=%q", tab, response.URL, response.Title)
	}
}

func TestBrowserSnapshotNextOffsetRequiresMoreElements(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	session.page.snapshotData = &Snapshot{
		Info:     PageInfo{URL: "https://example.com", Title: "example"},
		Revision: 3,
		Elements: []Element{{Ref: "e1", Role: "button", Name: "One"}, {Ref: "e2", Role: "button", Name: "Two"}},
	}
	response, err := manager.Snapshot(context.Background(), SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID, Offset: 10})
	if err != nil {
		t.Fatal(err)
	}
	if response.NextOffset != 0 {
		t.Fatalf("最后一页不应返回 next_offset: %+v", response)
	}
	session.page.snapshotData.HasMore = true
	session.page.snapshotData.Truncated = true
	response, err = manager.Snapshot(context.Background(), SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID, Offset: 10})
	if err != nil {
		t.Fatal(err)
	}
	if response.NextOffset != 12 || !response.Truncated {
		t.Fatalf("还有元素时应返回下一页偏移: %+v", response)
	}
	// 自定义 Page 未声明 PaginationKnown 时，满页按旧契约继续分页。
	session.page.snapshotData.HasMore = false
	session.page.snapshotData.Truncated = false
	response, err = manager.Snapshot(context.Background(), SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID, Offset: 0, MaxElements: 2})
	if err != nil || response.NextOffset != 2 {
		t.Fatalf("未知分页状态的自定义 Page 应保留满页推断: %+v %v", response, err)
	}
	// 新 Page 明确返回 has_more=false 时，恰好满页也应停止。
	session.page.snapshotData.PaginationKnown = true
	response, err = manager.Snapshot(context.Background(), SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID, Offset: 0, MaxElements: 2})
	if err != nil || response.NextOffset != 0 {
		t.Fatalf("明确无下一页时不应返回 next_offset: %+v %v", response, err)
	}
}

func TestBrowserSnapshotDoesNotOverwriteNewerTabProjection(t *testing.T) {
	session := &fakeViewportSession{fakeSession: &fakeSession{}}
	manager, err := newManager(Config{AllowPrivateNetwork: true, Factory: func(context.Context, SessionConfig) (Session, error) {
		return session, nil
	}}, defaultTiming())
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	current := manager.sessions[opened.SessionID]
	current.mu.Lock()
	currentPage := current.tabs[current.activeTab]
	currentPage.info = PageInfo{URL: "https://example.com/new", Title: "new"}
	current.mu.Unlock()
	session.page.info = PageInfo{URL: "https://example.com/new", Title: "new"}
	// Open 现在会附带一次快照并注册 refs；这里只关心后续旧快照，
	// 先清空已有 refs 再模拟读取期间发生导航。
	current.mu.Lock()
	currentPage.refs = map[string]struct{}{}
	current.mu.Unlock()
	// 模拟快照读取期间发生导航：generation 递增后，旧快照不能覆盖投影。
	session.page.snapshotHook = func() {
		current.mu.Lock()
		currentPage.generation++
		current.mu.Unlock()
	}
	session.page.snapshotData = &Snapshot{
		Info:     PageInfo{URL: "https://example.com/old", Title: "old"},
		Revision: 1,
		Text:     "old",
		Elements: []Element{{Ref: "e1", Role: "button", Name: "Next"}},
	}
	response, err := manager.Snapshot(context.Background(), SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	var tab *TabView
	for index := range response.Tabs {
		if response.Tabs[index].PageID == response.PageID {
			tab = &response.Tabs[index]
			break
		}
	}
	if tab == nil || tab.URL != "https://example.com/new" || tab.Title != "new" {
		t.Fatalf("旧快照覆盖了较新的 Tab 投影: %+v", response.Tabs)
	}
	current.mu.Lock()
	_, registered := currentPage.refs["e1"]
	current.mu.Unlock()
	if registered {
		t.Fatal("旧快照不应把 ref 注册到新 generation 的页面")
	}
}

func TestCompactInteractionSnapshotTruncatesTextAndStableTabs(t *testing.T) {
	response := SnapshotResponse{
		Text:        strings.Repeat("a", interactionTextLimit+10),
		TabsChanged: false,
		Tabs:        []TabView{{PageID: "page-1", URL: "https://example.com"}},
	}
	compactInteractionSnapshot(&response)
	if !response.TextTruncated || len([]rune(response.Text)) != interactionTextLimit+1 {
		t.Fatalf("动作快照正文没有截断: truncated=%v length=%d", response.TextTruncated, len([]rune(response.Text)))
	}
	if response.Tabs != nil {
		t.Fatalf("Tab 未变化时不应重复返回: %+v", response.Tabs)
	}
	response.TabsChanged = true
	response.Tabs = []TabView{{PageID: "page-1", URL: "https://example.com"}}
	compactInteractionSnapshot(&response)
	if len(response.Tabs) != 1 {
		t.Fatalf("Tab 变化时应保留投影: %+v", response.Tabs)
	}
}

func TestBrowserSnapshotKeepsFullTextWhileActionResultUsesPreview(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	session.page.snapshotData = &Snapshot{
		Info:     PageInfo{URL: "https://example.com", Title: "example"},
		Revision: 4,
		Text:     strings.Repeat("x", interactionTextLimit+50),
		Elements: []Element{{Ref: "e1", Role: "button", Name: "Save"}},
	}
	explicit, err := manager.Snapshot(context.Background(), SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	if explicit.TextTruncated || len([]rune(explicit.Text)) != interactionTextLimit+50 {
		t.Fatalf("显式 browserSnapshot 不应压缩正文: truncated=%v length=%d", explicit.TextTruncated, len([]rune(explicit.Text)))
	}
	action, err := manager.Click(context.Background(), ClickRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: explicit.Elements[0].Ref, Revision: explicit.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if action.Snapshot == nil || !action.Snapshot.TextTruncated || len([]rune(action.Snapshot.Text)) != interactionTextLimit+1 {
		t.Fatalf("动作结果应使用正文预览: %+v", action.Snapshot)
	}
}

func TestBrowserOpenRecoversAfterClosingLastTab(t *testing.T) {
	session := &fakeViewportSession{fakeSession: &fakeSession{}}
	manager, err := newManager(Config{AllowPrivateNetwork: true, Factory: func(context.Context, SessionConfig) (Session, error) {
		return session, nil
	}}, defaultTiming())
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CloseTab(context.Background(), TabRequest{SessionID: opened.SessionID, PageID: opened.PageID}); err != nil {
		t.Fatal(err)
	}
	reopened, err := manager.Open(context.Background(), OpenRequest{SessionID: opened.SessionID, URL: "https://example.com/next"})
	if err != nil {
		t.Fatalf("关闭最后一个 Tab 后 browserOpen 应能恢复: %v", err)
	}
	if reopened.SessionID != opened.SessionID || reopened.PageID == "" || reopened.URL != "https://example.com/next" {
		t.Fatalf("恢复结果=%+v", reopened)
	}
}

func TestTabActionFunctionSupportsListNewAndReload(t *testing.T) {
	manager, _ := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	tabs := functionByName(t, manager, "browserTabs").Handler.(func(context.Context, TabActionRequest) (TabActionResponse, error))
	listed, err := tabs(context.Background(), TabActionRequest{SessionID: opened.SessionID, Action: "list"})
	if err != nil || len(listed.Tabs) != 1 || listed.Action != "list" {
		t.Fatalf("list=%+v err=%v", listed, err)
	}
	created, err := tabs(context.Background(), TabActionRequest{SessionID: opened.SessionID, Action: "new", URL: "https://example.com/new"})
	if err != nil || created.PageID == "" || created.Action != "new" || created.URL != "https://example.com/new" {
		t.Fatalf("new=%+v err=%v", created, err)
	}
	reloaded, err := tabs(context.Background(), TabActionRequest{SessionID: opened.SessionID, Action: "reload", PageID: opened.PageID})
	if err != nil || reloaded.Action != "reload" || reloaded.URL == "" {
		t.Fatalf("reload=%+v err=%v", reloaded, err)
	}
}

func TestBrowserActExecutesStepsAndStopsOnFailure(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true, AllowEvaluate: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	checked := true
	snapshot, err := manager.Snapshot(context.Background(), SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	beforeSnapshots := session.page.snapshotCalls
	response, err := manager.Act(context.Background(), ActRequest{
		SessionID: opened.SessionID,
		PageID:    opened.PageID,
		Revision:  snapshot.Revision,
		Steps: []ActStep{
			{Op: "type", Ref: "e1", Text: "Alice"},
			{Op: "check", Ref: "e1", Checked: &checked},
			{Op: "evaluate", Code: "document.title"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Completed != 3 || response.FailedStep != 0 || len(response.Steps) != 3 || response.Snapshot == nil {
		t.Fatalf("批量动作结果=%+v", response)
	}
	if response.Steps[2].Type != "string" || string(response.Steps[2].Result) != `"ok"` {
		t.Fatalf("evaluate 结果没有返回: %+v", response.Steps[2])
	}
	if delta := session.page.snapshotCalls - beforeSnapshots; delta != 1 {
		t.Fatalf("批量动作应只在结束时快照一次，实际 %d 次", delta)
	}
	if session.page.lastText != "Alice" || !session.page.checked {
		t.Fatalf("批量动作没有落到页面: text=%q checked=%v", session.page.lastText, session.page.checked)
	}
	failed, err := manager.Act(context.Background(), ActRequest{
		SessionID: opened.SessionID,
		PageID:    opened.PageID,
		Revision:  1,
		Steps: []ActStep{
			{Op: "type", Ref: "e1", Text: "Bob"},
			{Op: "click"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if failed.Completed != 0 || failed.FailedStep != 2 || len(failed.Steps) != 1 || failed.Steps[0].Success || failed.Steps[0].Error == "" {
		t.Fatalf("预检失败没有阻止副作用并报告: %+v", failed)
	}
	if session.page.lastText != "Alice" {
		t.Fatalf("预检失败前不应执行第一步: %q", session.page.lastText)
	}
	invalidRef, err := manager.Act(context.Background(), ActRequest{
		SessionID: opened.SessionID,
		PageID:    opened.PageID,
		Revision:  snapshot.Revision,
		Steps: []ActStep{
			{Op: "type", Ref: "e1", Text: "Charlie"},
			{Op: "click", Ref: "missing"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if invalidRef.Completed != 0 || invalidRef.FailedStep != 2 || invalidRef.Steps[0].Index != 1 {
		t.Fatalf("无效 ref 预检结果=%+v", invalidRef)
	}
	if session.page.lastText != "Alice" {
		t.Fatalf("无效 ref 预检前不应执行第一步: %q", session.page.lastText)
	}
	legal, err := manager.Act(context.Background(), ActRequest{
		SessionID: opened.SessionID,
		PageID:    opened.PageID,
		Revision:  snapshot.Revision,
		Steps: []ActStep{
			{Op: "type", Ref: "e1", Text: "Dave"},
			{Op: "wait", Condition: "text_contains", RefOrSelector: "e1_2"},
		},
	})
	if err != nil || legal.Completed != 2 {
		t.Fatalf("text_contains 的自由文本不应被当成 ref: %+v err=%v", legal, err)
	}
}

func TestBrowserOpenWaitsForInflightReconcileBeforeRecovery(t *testing.T) {
	session := &blockingListViewportSession{
		fakeViewportSession: &fakeViewportSession{fakeSession: &fakeSession{}},
		listEntered:         make(chan struct{}),
		releaseList:         make(chan struct{}),
	}
	manager, err := newManager(Config{AllowPrivateNetwork: true, Factory: func(context.Context, SessionConfig) (Session, error) {
		return session, nil
	}}, defaultTiming())
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com/one"})
	if err != nil {
		t.Fatal(err)
	}
	current := manager.sessions[opened.SessionID]
	current.mu.Lock()
	current.tabs = map[string]*page{}
	current.tabOrder = nil
	current.activeTab = ""
	current.mu.Unlock()
	session.blockList = true
	listDone := make(chan struct{})
	go func() {
		_, _ = manager.ListTabs(context.Background(), TabsRequest{SessionID: opened.SessionID})
		close(listDone)
	}()
	select {
	case <-session.listEntered:
	case <-time.After(time.Second):
		t.Fatal("ListTabs 没有进入 reconcile")
	}
	openDone := make(chan OpenResponse, 1)
	go func() {
		response, _ := manager.Open(context.Background(), OpenRequest{SessionID: opened.SessionID, URL: "https://example.com/two"})
		openDone <- response
	}()
	select {
	case <-openDone:
		t.Fatal("并发 reconcile 未完成时 browserOpen 不应直接新建页面")
	case <-time.After(30 * time.Millisecond):
	}
	close(session.releaseList)
	select {
	case response := <-openDone:
		if response.PageID == "" {
			t.Fatalf("恢复结果为空: %+v", response)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reconcile 完成后 browserOpen 仍未返回")
	}
	<-listDone
	current.mu.Lock()
	count := len(current.tabs)
	current.mu.Unlock()
	if count > 1 {
		t.Fatalf("恢复后出现重复 Tab: %d", count)
	}
}

func TestBrowserSnapshotDoesNotRegressRefsToOlderRevision(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com/new"})
	if err != nil {
		t.Fatal(err)
	}
	session.page.snapshotData = &Snapshot{Info: PageInfo{URL: "https://example.com/new", Title: "new"}, Revision: 6, Elements: []Element{{Ref: "e6", Role: "button", Name: "New"}}}
	if _, err := manager.Snapshot(context.Background(), SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID}); err != nil {
		t.Fatal(err)
	}
	session.page.snapshotData = &Snapshot{Info: PageInfo{URL: "https://example.com/old", Title: "old"}, Revision: 4, Elements: []Element{{Ref: "e4", Role: "button", Name: "Old"}}}
	if _, err := manager.Snapshot(context.Background(), SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID}); err != nil {
		t.Fatal(err)
	}
	current := manager.sessions[opened.SessionID]
	current.mu.Lock()
	currentPage := current.tabs[opened.PageID]
	_, hasOld := currentPage.refs["e4"]
	_, hasNew := currentPage.refs["e6"]
	info := currentPage.info
	current.mu.Unlock()
	if hasOld || !hasNew || currentPage.refRevision != 6 || info.URL != "https://example.com/new" {
		t.Fatalf("旧快照回退了 refs/投影: refs=%v info=%+v", currentPage.refs, info)
	}
}

func activeTabStatus(snapshot SnapshotResponse) string {
	for _, tab := range snapshot.Tabs {
		if tab.PageID == snapshot.ActivePageID {
			return tab.Status
		}
	}
	return ""
}

func TestBrowserInteractionReleasesLockBeforeTabReconcile(t *testing.T) {
	base := &fakeSession{page: &fakePage{info: PageInfo{URL: "about:blank"}}}
	session := &blockingListViewportSession{
		fakeViewportSession: &fakeViewportSession{fakeSession: base},
		listEntered:         make(chan struct{}),
		releaseList:         make(chan struct{}),
	}
	manager, err := NewManager(Config{AllowPrivateNetwork: true, Factory: func(context.Context, SessionConfig) (Session, error) {
		return session, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	session.blockList = true
	session.listOnce = sync.Once{}
	done := make(chan struct{})
	go func() {
		_, _ = manager.Click(ctx, ClickRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: snapshot.Elements[0].Ref, Revision: snapshot.Revision})
		close(done)
	}()
	select {
	case <-session.listEntered:
	case <-time.After(time.Second):
		t.Fatal("点击没有进入 Tab reconcile")
	}
	probeCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	if err := lockInteraction(probeCtx, &manager.sessions[opened.SessionID].interactionMu); err != nil {
		cancel()
		close(session.releaseList)
		t.Fatalf("Tab reconcile 期间 interactionMu 仍被点击持有，可能卡死: %v", err)
	}
	manager.sessions[opened.SessionID].interactionMu.Unlock()
	cancel()
	close(session.releaseList)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("点击在 Tab reconcile 后没有返回")
	}
}

func TestBrowserScreenshotUsesDistinctPaths(t *testing.T) {
	manager, _ := fakeManager(t, Config{AllowPrivateNetwork: true, ScreenshotDir: t.TempDir()})
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	request := ScreenshotRequest{SessionID: opened.SessionID, PageID: opened.PageID}
	first, err := manager.Screenshot(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Screenshot(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Path == second.Path {
		t.Fatalf("连续截图覆盖了已返回的文件: %q", first.Path)
	}
}

func TestBrowserScreenshotRestoresActiveTab(t *testing.T) {
	base := &fakeSession{page: &fakePage{info: PageInfo{URL: "about:blank", Title: "blank"}}}
	session := &fakeViewportSession{fakeSession: base}
	manager, err := NewManager(Config{
		AllowPrivateNetwork: true, ScreenshotDir: t.TempDir(),
		Factory: func(context.Context, SessionConfig) (Session, error) { return session, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	current := manager.sessions[opened.SessionID]
	current.mu.Lock()
	background := manager.registerPage(current, &fakeTargetPage{
		fakePage: &fakePage{info: PageInfo{URL: "https://example.com/background"}},
		targetID: "target-2",
	}, defaultTiming().viewport, opened.PageID)
	current.mu.Unlock()
	ctx := context.Background()
	if _, err := manager.Screenshot(ctx, ScreenshotRequest{SessionID: opened.SessionID, PageID: background.id}); err != nil {
		t.Fatal(err)
	}
	current.mu.Lock()
	active := current.activeTab
	current.mu.Unlock()
	if active != opened.PageID || len(session.activated) == 0 || session.activated[len(session.activated)-1] != "target-1" {
		t.Fatalf("active=%q activation calls=%v", active, session.activated)
	}
}

func TestBrowserWaitAcceptsSnapshotRefAndCSSSelector(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil || snapshot.Revision == 0 {
		t.Fatalf("snapshot=%+v error=%v", snapshot, err)
	}
	request := WaitRequest{
		SessionID: opened.SessionID, PageID: opened.PageID,
		Condition: "selector_visible", RefOrSelector: snapshot.Elements[0].Ref, Revision: snapshot.Revision,
	}
	if _, err := manager.Wait(ctx, request); err != nil {
		t.Fatalf("snapshot ref 应可直接用于 browserWait: %v", err)
	}
	if session.page.waitOptions.Selector != `[data-browserkit-ref="e1"]` {
		t.Fatalf("snapshot ref 没有转换为安全 selector: %+v", session.page.waitOptions)
	}
	request.RefOrSelector = "example-widget"
	request.Revision = 0
	if _, err := manager.Wait(ctx, request); err != nil {
		t.Fatalf("e 开头的 CSS selector 不应被误判为 ref: %v", err)
	}
	if session.page.waitOptions.Selector != "example-widget" {
		t.Fatalf("CSS selector 被意外改写: %+v", session.page.waitOptions)
	}
	request.RefOrSelector = "eabc123_4"
	if _, err := manager.Wait(ctx, request); err == nil {
		t.Fatal("未知的 snapshot ref 应返回 invalid_reference")
	}
}

func TestBrowserWaitTimeoutIsStructured(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	session.page.waitErr = context.DeadlineExceeded
	_, err = manager.Wait(context.Background(), WaitRequest{
		SessionID: opened.SessionID, PageID: opened.PageID,
		Condition: "url_contains", RefOrSelector: "/done", TimeoutMS: 1000,
	})
	var browserErr *Error
	if !errors.As(err, &browserErr) || browserErr.Kind != "wait_timeout" {
		t.Fatalf("等待超时应返回 wait_timeout 结构化错误: %v", err)
	}
	if !strings.Contains(browserErr.Message, "url_contains") || !strings.Contains(browserErr.Message, "/done") {
		t.Fatalf("超时错误缺少条件或目标: %+v", browserErr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := manager.Wait(ctx, WaitRequest{
		SessionID: opened.SessionID, PageID: opened.PageID,
		Condition: "load_complete", TimeoutMS: 1000,
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("调用方取消应保持 context.Canceled: %v", err)
	}
}

func TestBrowserDownloadStartReturnsUnconfirmedWithoutInventedFile(t *testing.T) {
	dir := t.TempDir()
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true, DownloadDir: dir})
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(context.Background(), SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	// 即使自定义 Page 交付旧式猜测身份，Manager 的 start 也只交付点击事实。
	session.page.downloadInfo = DownloadInfo{Triggered: true, ID: "unproven", Filename: "report.txt", Path: dir + "/report.txt", Bytes: 12}
	request := DownloadRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: snapshot.Elements[0].Ref, Revision: snapshot.Revision}
	response, err := manager.Download(context.Background(), request)
	var browserErr *Error
	if !errors.As(err, &browserErr) || browserErr.Kind != "download_unconfirmed" || !response.Triggered || response.State != "unconfirmed" || response.DownloadID != "" || response.Path != "" || response.FileName != "" || response.Bytes != 0 || !session.page.downloadCalled {
		t.Fatalf("response=%+v err=%v", response, err)
	}
	session.page.downloadInfo.Triggered = false
	response, err = manager.Download(context.Background(), request)
	if !errors.As(err, &browserErr) || browserErr.Kind != "download_unconfirmed" || response.Triggered {
		t.Fatalf("nil error invented completion: response=%+v err=%v", response, err)
	}
	session.page.downloadErr = context.DeadlineExceeded
	response, err = manager.Download(context.Background(), request)
	if !errors.As(err, &browserErr) || browserErr.Kind != "download_trigger_timeout" || response.Triggered || response.SessionID != opened.SessionID || response.PageID != opened.PageID || response.Ref != request.Ref || response.DownloadID != "" || response.Path != "" {
		t.Fatalf("partial response=%+v err=%v", response, err)
	}
}

func TestBrowserActClickWaitAfterTimeoutReportsWarning(t *testing.T) {
	manager, _ := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(context.Background(), SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	response, err := manager.Act(context.Background(), ActRequest{
		SessionID: opened.SessionID,
		PageID:    opened.PageID,
		Revision:  snapshot.Revision,
		Steps: []ActStep{
			{Op: "click", Ref: "e1", WaitAfterMS: 200, TimeoutMS: 20},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.FailedStep != 0 || len(response.Steps) != 1 || !response.Steps[0].Success {
		t.Fatalf("点击已执行，不应标记步骤失败: %+v", response)
	}
	if !strings.Contains(response.Steps[0].Warning, "不要重放") {
		t.Fatalf("wait_after_ms 超时的 warning 不可操作: %+v", response.Steps[0])
	}
}

func TestBrowserActIncludesScreenshot(t *testing.T) {
	manager, _ := fakeManager(t, Config{AllowPrivateNetwork: true, ScreenshotDir: t.TempDir()})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	response, err := manager.Act(context.Background(), ActRequest{
		SessionID: opened.SessionID, PageID: opened.PageID, Revision: 1, IncludeScreenshot: true,
		Steps: []ActStep{{Op: "wait", Condition: "load_complete"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Screenshot == nil || len(response.Screenshot.Data) == 0 || response.Screenshot.Bytes == 0 {
		t.Fatalf("批次截图缺失: %+v", response)
	}
}

func TestBrowserActKeepsStepsWhenObservationIsCanceled(t *testing.T) {
	for _, phase := range []string{"snapshot", "screenshot"} {
		t.Run(phase, func(t *testing.T) {
			manager, session := fakeManager(t, Config{AllowPrivateNetwork: true, ScreenshotDir: t.TempDir()})
			defer manager.Close(context.Background())
			opened, err := manager.Open(context.Background(), OpenRequest{URL: "http://127.0.0.1/"})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if phase == "snapshot" {
				session.page.snapshotHook = cancel
				session.page.snapshotErr = context.Canceled
			} else {
				session.page.screenshotHook = func() error {
					cancel()
					return context.Canceled
				}
			}
			response, err := manager.Act(ctx, ActRequest{
				SessionID: opened.SessionID, PageID: opened.PageID, IncludeScreenshot: true,
				Steps: []ActStep{{Op: "wait", Condition: "load_complete"}},
			})
			if !errors.Is(err, context.Canceled) || !response.Cancelled || response.Completed != 1 || response.FailedStep != 0 || len(response.Steps) != 1 || !response.Steps[0].Success {
				t.Fatalf("lost completed step after %s cancellation: %+v, %v", phase, response, err)
			}
			if (response.SnapshotError != "") != (phase == "snapshot") || (response.ScreenshotError != "") != (phase == "screenshot") {
				t.Fatalf("wrong observation error after %s cancellation: %+v", phase, response)
			}
		})
	}
}

func TestBrowserClickIncludesScreenshot(t *testing.T) {
	manager, _ := fakeManager(t, Config{AllowPrivateNetwork: true, ScreenshotDir: t.TempDir()})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(context.Background(), SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	response, err := manager.Click(context.Background(), ClickRequest{
		SessionID: opened.SessionID, PageID: opened.PageID,
		Ref: snapshot.Elements[0].Ref, Revision: snapshot.Revision, IncludeScreenshot: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Screenshot == nil || len(response.Screenshot.Data) == 0 || response.Screenshot.Bytes == 0 {
		t.Fatalf("点击截图缺失: %+v", response)
	}
}

func TestBrowserActWaitTimeoutIsStructured(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	session.page.waitErr = context.DeadlineExceeded
	response, err := manager.Act(context.Background(), ActRequest{
		SessionID: opened.SessionID, PageID: opened.PageID, Revision: 1,
		Steps: []ActStep{{Op: "wait", Condition: "selector_hidden", RefOrSelector: "#loading", TimeoutMS: 1000}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.FailedStep != 1 || len(response.Steps) != 1 {
		t.Fatalf("批量 wait 超时结果=%+v", response)
	}
	step := response.Steps[0]
	if step.Kind != "wait_timeout" || !step.Recoverable || !strings.Contains(step.Error, "隐藏") {
		t.Fatalf("批量 wait 超时缺少结构化字段或建议: %+v", step)
	}
}

func TestBrowserNavigateHistoryDirections(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com/current"})
	if err != nil {
		t.Fatal(err)
	}
	session.page.historyInfo = PageInfo{URL: "https://example.com/previous", Title: "Previous"}
	response, err := manager.Navigate(context.Background(), NavigateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Direction: "back"})
	if err != nil {
		t.Fatal(err)
	}
	if session.page.historyDirection != "back" || response.URL != "https://example.com/previous" || response.Title != "Previous" {
		t.Fatalf("历史后退结果=%+v direction=%q", response, session.page.historyDirection)
	}
	if _, err := manager.Navigate(context.Background(), NavigateRequest{SessionID: opened.SessionID, PageID: opened.PageID, URL: "https://example.com", Direction: "back"}); err == nil {
		t.Fatal("url 与 direction 同时提供时应拒绝")
	}
	session.page.historyErr = newError("history_unavailable", "没有可用的后退历史", "使用 browserNavigate 打开目标地址", true)
	if _, err := manager.Navigate(context.Background(), NavigateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Direction: "back"}); err == nil {
		t.Fatal("history_unavailable 应返回错误")
	}
}

func TestBrowserNavigateHistoryRespectsAddressPolicy(t *testing.T) {
	manager, session := fakeManager(t, Config{})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	session.page.historyInfo = PageInfo{URL: "http://127.0.0.1/secret"}
	_, err = manager.Navigate(context.Background(), NavigateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Direction: "back"})
	var browserErr *Error
	if !errors.As(err, &browserErr) || browserErr.Kind != "navigation_blocked" {
		t.Fatalf("历史目标策略错误=%v，期望 navigation_blocked", err)
	}
	if session.page.historyNavigated {
		t.Fatal("策略校验失败后不应执行历史导航")
	}
}

func TestBrowserWaitDetectsRemovedRefBeforeTimeout(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(context.Background(), SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	missing := false
	session.page.refExists = &missing
	session.page.waitCalled = false
	started := time.Now()
	_, err = manager.Wait(context.Background(), WaitRequest{
		SessionID: opened.SessionID, PageID: opened.PageID, Condition: "selector_visible",
		RefOrSelector: "e1", Revision: snapshot.Revision, TimeoutMS: 5000,
	})
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("失效 ref 不应等待超时，耗时 %s", elapsed)
	}
	var browserErr *Error
	if !errors.As(err, &browserErr) || browserErr.Kind != "stale_reference" {
		t.Fatalf("失效 ref 错误=%v，期望 stale_reference", err)
	}
	if session.page.waitCalled {
		t.Fatal("失效 ref 不应进入 page.Wait")
	}
}

func TestBrowserActPreflightRejectsAmbiguousSelect(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(context.Background(), SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	response, err := manager.Act(context.Background(), ActRequest{
		SessionID: opened.SessionID, PageID: opened.PageID, Revision: snapshot.Revision,
		Steps: []ActStep{
			{Op: "type", Ref: "e1", Text: "MUTATED"},
			{Op: "select", Ref: "e1", Value: "a", Label: "A"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.FailedStep != 2 || response.Completed != 0 || len(response.Steps) != 1 {
		t.Fatalf("select 预检结果=%+v", response)
	}
	if session.page.lastText == "MUTATED" {
		t.Fatal("预检失败前不应执行前面的 type")
	}
}

func TestBrowserWaitPreservesCallerDeadline(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	block := make(chan struct{})
	defer close(block)
	session.page.waitBlock = block
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = manager.Wait(ctx, WaitRequest{SessionID: opened.SessionID, PageID: opened.PageID, Condition: "selector_visible", RefOrSelector: "#later"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("调用方 deadline 错误=%v，期望 context.DeadlineExceeded", err)
	}
	var browserErr *Error
	if errors.As(err, &browserErr) {
		t.Fatalf("调用方 deadline 不应包装成 wait_timeout: %+v", browserErr)
	}
}

func TestBrowserWaitRequestTimeoutIsStructured(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	block := make(chan struct{})
	defer close(block)
	session.page.waitBlock = block
	_, err = manager.Wait(context.Background(), WaitRequest{SessionID: opened.SessionID, PageID: opened.PageID, Condition: "selector_visible", RefOrSelector: "#later", TimeoutMS: 50})
	var browserErr *Error
	if !errors.As(err, &browserErr) || browserErr.Kind != "wait_timeout" || !browserErr.Recoverable {
		t.Fatalf("请求超时错误=%v，期望 wait_timeout", err)
	}
}

func TestRefCheckBudgetSkipsTightDeadlines(t *testing.T) {
	tight, tightCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer tightCancel()
	if _, ok := refCheckBudget(tight); ok {
		t.Fatal("20ms 预算应跳过 HasRef 预检")
	}
	roomy, roomyCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer roomyCancel()
	budget, ok := refCheckBudget(roomy)
	if !ok || budget <= 0 || budget > 50*time.Millisecond {
		t.Fatalf("200ms 预算=%s ok=%v", budget, ok)
	}
	for _, test := range []struct {
		timeout time.Duration
		want    bool
	}{
		{35 * time.Millisecond, false},
		{45 * time.Millisecond, true},
		{100 * time.Millisecond, true},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), test.timeout)
		_, ok := refCheckBudget(ctx)
		cancel()
		if ok != test.want {
			t.Fatalf("%s 预算 ok=%v，期望 %v", test.timeout, ok, test.want)
		}
	}
}

func TestBrowserWaitHasRefBudgetDoesNotConsumeWait(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(context.Background(), SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	block := make(chan struct{})
	defer close(block)
	session.page.hasRefBlock = block
	started := time.Now()
	if _, err := manager.Wait(context.Background(), WaitRequest{SessionID: opened.SessionID, PageID: opened.PageID, Condition: "selector_visible", RefOrSelector: "e1", Revision: snapshot.Revision, TimeoutMS: 20}); err != nil {
		t.Fatalf("HasRef 预检不应吃掉全部等待预算: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("HasRef 预检耗时过长: %s", elapsed)
	}
	if session.page.hasRefCalls != 0 {
		t.Fatalf("紧超时不应调用 HasRef: %d", session.page.hasRefCalls)
	}
}

func TestBrowserActParentDeadlineIsNotStepTimeout(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	block := make(chan struct{})
	defer close(block)
	session.page.waitBlock = block
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	response, err := manager.Act(ctx, ActRequest{
		SessionID: opened.SessionID, PageID: opened.PageID, Revision: 1,
		Steps: []ActStep{{Op: "wait", Condition: "selector_visible", RefOrSelector: "#later", TimeoutMS: 5000}},
	})
	if !errors.Is(err, context.DeadlineExceeded) || !response.Cancelled {
		t.Fatalf("父 deadline 结果=%+v err=%v", response, err)
	}
	if len(response.Steps) == 1 && response.Steps[0].Kind != "" {
		t.Fatalf("父 deadline 不应标成步骤超时: %+v", response.Steps[0])
	}
}

func TestBrowserActClickTimeoutAdvisesNotToReplay(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(context.Background(), SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	block := make(chan struct{})
	defer close(block)
	session.page.clickBlock = block
	response, err := manager.Act(context.Background(), ActRequest{
		SessionID: opened.SessionID, PageID: opened.PageID, Revision: snapshot.Revision,
		Steps: []ActStep{{Op: "click", Ref: "e1", TimeoutMS: 50}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.FailedStep != 1 || len(response.Steps) != 1 || response.Steps[0].Kind != "step_timeout" {
		t.Fatalf("click 超时结果=%+v", response)
	}
	if !strings.Contains(response.Steps[0].RecommendedNextStep, "不要直接重放") {
		t.Fatalf("click 超时建议不可操作: %+v", response.Steps[0])
	}
}

func TestBrowserActStepTimeoutCoversAllActions(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(context.Background(), SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	block := make(chan struct{})
	defer close(block)
	session.page.hoverBlock = block
	started := time.Now()
	response, err := manager.Act(context.Background(), ActRequest{
		SessionID: opened.SessionID, PageID: opened.PageID, Revision: snapshot.Revision,
		Steps: []ActStep{{Op: "hover", Ref: "e1", TimeoutMS: 80}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("步骤超时没有生效，耗时 %s", elapsed)
	}
	if response.FailedStep != 1 || response.Completed != 0 || len(response.Steps) != 1 {
		t.Fatalf("步骤超时结果=%+v", response)
	}
	if step := response.Steps[0]; step.Kind != "step_timeout" || !step.Recoverable {
		t.Fatalf("步骤超时缺少结构化字段: %+v", step)
	}
}

func TestBrowserActWaitStepDeadlineIsStructured(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	block := make(chan struct{})
	defer close(block)
	session.page.waitBlock = block
	response, err := manager.Act(context.Background(), ActRequest{
		SessionID: opened.SessionID, PageID: opened.PageID, Revision: 1,
		Steps: []ActStep{{Op: "wait", Condition: "selector_visible", RefOrSelector: "#later", TimeoutMS: 80}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.FailedStep != 1 || response.Completed != 0 || len(response.Steps) != 1 {
		t.Fatalf("wait 步骤超时结果=%+v", response)
	}
	step := response.Steps[0]
	if step.Kind != "wait_timeout" || !step.Recoverable || step.RecommendedNextStep == "" {
		t.Fatalf("wait 步骤超时没有保留结构化错误: %+v", step)
	}
}

func TestBrowserTypeDefaultsToClearAndAllowsExplicitAppend(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}

	var request TypeRequest
	err = json.Unmarshal([]byte(`{"session_id":"`+opened.SessionID+`","page_id":"`+opened.PageID+`","ref":"e1","revision":`+fmt.Sprint(snapshot.Revision)+`,"text":"new value"}`), &request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Type(ctx, request); err != nil {
		t.Fatal(err)
	}
	if !session.page.lastClear {
		t.Fatal("省略 clear 的 browserType 没有清空原值")
	}

	var appendRequest TypeRequest
	err = json.Unmarshal([]byte(`{"session_id":"`+opened.SessionID+`","page_id":"`+opened.PageID+`","ref":"e1","revision":`+fmt.Sprint(snapshot.Revision)+`,"text":"suffix","append":true}`), &appendRequest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Type(ctx, appendRequest); err != nil {
		t.Fatal(err)
	}
	if session.page.lastClear {
		t.Fatal("append=true 时不应清空原值")
	}
}

func TestBrowserOpenInjectsBeforeNavigationAndEvaluateIsGated(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true, AllowEvaluate: true})
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com", InjectJS: "window.__browserkit = true"})
	if err != nil {
		t.Fatal(err)
	}
	if session.options.InjectScript != "window.__browserkit = true" {
		t.Fatalf("注入脚本没有在创建页面时传递: %#v", session.options)
	}
	if !session.page.idleCalled {
		t.Fatal("browserOpen 没有等待页面空闲")
	}
	if opened.LoadStatus != OpenLoadLoaded {
		t.Fatalf("browserOpen load_status=%q", opened.LoadStatus)
	}
	ctx := context.Background()
	response, err := manager.Evaluate(ctx, EvaluateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Code: "document.title"})
	if err != nil || string(response.Value) != `"ok"` {
		t.Fatalf("evaluate=%#v error=%v", response, err)
	}
	blocked, _ := fakeManager(t, Config{AllowPrivateNetwork: true})
	if _, err := blocked.Evaluate(ctx, EvaluateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Code: "1"}); err == nil {
		t.Fatal("未启用 JavaScript 时 evaluate 没有拒绝")
	}
}

func TestBrowserOpenExistingPageAppliesInjectScript(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true, AllowEvaluate: true})
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	session.page.injectedScripts = nil
	session.page.evaluatedScripts = nil
	const script = "window.__diagnostic = true"
	if _, err := manager.Open(context.Background(), OpenRequest{SessionID: opened.SessionID, InjectJS: script}); err != nil {
		t.Fatalf("复用 page 时 inject_js 不应被拒绝: %v", err)
	}
	if len(session.page.injectedScripts) != 1 || session.page.injectedScripts[0] != script {
		t.Fatalf("已有 page 没有注册后续 document 脚本: %+v", session.page.injectedScripts)
	}
	if len(session.page.evaluatedScripts) != 1 || session.page.evaluatedScripts[0] != script {
		t.Fatalf("已有 page 没有执行当前 document 脚本: %+v", session.page.evaluatedScripts)
	}
	if _, err := manager.Open(context.Background(), OpenRequest{SessionID: opened.SessionID, InjectJS: script}); err != nil {
		t.Fatal(err)
	}
	if len(session.page.injectedScripts) != 1 || len(session.page.evaluatedScripts) != 2 {
		t.Fatalf("重复 browserOpen 不应重复注册脚本: injected=%d evaluated=%d", len(session.page.injectedScripts), len(session.page.evaluatedScripts))
	}
}

func TestBrowserOpenValidatesURLBeforeInjectingIntoExistingPage(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true, AllowEvaluate: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	session.page.injectedScripts = nil
	session.page.evaluatedScripts = nil
	if _, err := manager.Open(context.Background(), OpenRequest{SessionID: opened.SessionID, URL: "file:///tmp/blocked", InjectJS: "window.__blocked = true"}); err == nil {
		t.Fatal("非法 URL 没有被拒绝")
	}
	if len(session.page.injectedScripts) != 0 || len(session.page.evaluatedScripts) != 0 {
		t.Fatalf("URL 校验失败前已经产生注入副作用: injected=%v evaluated=%v", session.page.injectedScripts, session.page.evaluatedScripts)
	}
}

func TestBrowserOpenReportsIdleTimeoutWithoutDiscardingPage(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	session.page = &fakePage{info: PageInfo{URL: "about:blank", Title: "blank"}, idleErr: context.DeadlineExceeded}
	manager.timing.openIdle = time.Millisecond

	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if opened.LoadStatus != OpenLoadTimeout {
		t.Fatalf("超时状态=%q", opened.LoadStatus)
	}
	if _, _, err := manager.get(context.Background(), opened.SessionID, opened.PageID); err != nil {
		t.Fatalf("页面超时后不应丢失: %v", err)
	}
}

func TestBrowserOpenRefreshesPageInfoAfterIdle(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	session.page = &fakePage{
		info:           PageInfo{URL: "about:blank", Title: "Example Domain"},
		navigateResult: &PageInfo{URL: "", Title: "about:blank"},
	}
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if opened.URL != "https://example.com" || opened.Title != "Example Domain" {
		t.Fatalf("browserOpen 没有返回稳定后的页面信息: %+v", opened)
	}
}

func TestBrowserOpenReportsOtherErrorsAsOpenFailed(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	session.page = &fakePage{info: PageInfo{URL: "about:blank", Title: "blank"}, idleErr: errors.New("attach failed")}
	if _, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"}); err == nil {
		t.Fatal("非超时等待错误没有失败")
	} else {
		var structured *Error
		if !errors.As(err, &structured) || structured.Kind != "open_failed" {
			t.Fatalf("非超时错误=%v", err)
		}
	}
}

func TestManagerReclaimsIdleSessionWithoutExternalClose(t *testing.T) {
	manager, _ := fakeManagerWithTiming(t, Config{AllowPrivateNetwork: true, IdleTimeout: 5 * time.Millisecond}, adjustedTiming(func(value *timing) {
		value.sessionIdleCheck = time.Millisecond
	}))
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		manager.mu.Lock()
		_, present := manager.sessions[opened.SessionID]
		manager.mu.Unlock()
		if !present {
			break
		}
		time.Sleep(time.Millisecond)
	}
	manager.mu.Lock()
	_, present := manager.sessions[opened.SessionID]
	manager.mu.Unlock()
	if present {
		t.Fatal("空闲浏览器 session 未被自动回收")
	}
	if _, _, err := manager.get(context.Background(), opened.SessionID, opened.PageID); err == nil {
		t.Fatal("自动回收后 session 仍可访问")
	}
	_ = manager.Close(context.Background())
}

func TestReconcileTabsUsesOneShortBudget(t *testing.T) {
	base := &fakeSession{page: &fakePage{info: PageInfo{URL: "about:blank"}}}
	session := &blockingListViewportSession{
		fakeViewportSession: &fakeViewportSession{fakeSession: base},
		listEntered:         make(chan struct{}),
		releaseList:         make(chan struct{}),
	}
	manager, err := newManager(Config{
		AllowPrivateNetwork: true,
		Factory:             func(context.Context, SessionConfig) (Session, error) { return session, nil },
	}, adjustedTiming(func(value *timing) { value.tabReconcileTimeout = 20 * time.Millisecond }))
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	session.blockList = true
	session.listOnce = sync.Once{}
	manager.mu.Lock()
	manager.active = ""
	manager.mu.Unlock()
	started := time.Now()
	err = manager.reconcileTabs(context.Background(), manager.sessions[opened.SessionID])
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 500*time.Millisecond {
		t.Fatalf("Tab reconcile 没有共享短预算: elapsed=%s err=%v", time.Since(started), err)
	}
}

func TestExplicitPageOperationDoesNotRunStateReconcile(t *testing.T) {
	base := &fakeSession{page: &fakePage{info: PageInfo{URL: "about:blank"}}}
	session := &blockingListViewportSession{
		fakeViewportSession: &fakeViewportSession{fakeSession: base},
		listEntered:         make(chan struct{}),
		releaseList:         make(chan struct{}),
	}
	manager, err := NewManager(Config{
		AllowPrivateNetwork: true,
		Factory:             func(context.Context, SessionConfig) (Session, error) { return session, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	session.blockList = true
	session.listOnce = sync.Once{}
	started := time.Now()
	if _, err := manager.ActivateTab(context.Background(), TabRequest{SessionID: opened.SessionID, PageID: opened.PageID}); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("显式 page 操作被状态 reconcile 阻塞: %s", elapsed)
	}
	manager.mu.Lock()
	activeSession := manager.active
	manager.mu.Unlock()
	if activeSession != opened.SessionID {
		t.Fatalf("显式 page 操作没有恢复 active browser session: %q", activeSession)
	}
}

func TestConnectionLossDropsSessionAndNotifiesState(t *testing.T) {
	browser := &fakeConnectionSession{
		fakeViewportSession: &fakeViewportSession{fakeSession: &fakeSession{}},
		done:                make(chan struct{}),
		closes:              make(chan struct{}, 1),
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
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	subscribeCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates, err := manager.SubscribeState(subscribeCtx)
	if err != nil {
		t.Fatal(err)
	}
	close(browser.done)
	select {
	case <-updates:
	case <-time.After(2 * time.Second):
		t.Fatal("CDP 连接断开后没有清理缓存 Tab")
	}
	view, err := manager.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !view.Closed || len(view.Tabs) != 0 {
		t.Fatalf("连接断开后的视图=%+v", view)
	}
	// 会话已从 Manager 摘除，底层 Close 仍要回收自建浏览器和临时 profile。
	select {
	case <-browser.closes:
	case <-time.After(2 * time.Second):
		t.Fatal("连接断开后底层浏览器没有关闭")
	}
	if _, err := manager.CloseTab(context.Background(), TabRequest{SessionID: opened.SessionID, PageID: opened.PageID}); err == nil {
		t.Fatal("失效会话仍然可以关闭 Tab")
	}
	closed, err := manager.CloseSession(context.Background(), CloseRequest{SessionID: opened.SessionID})
	if err != nil || !closed.Closed {
		t.Fatalf("重复关闭失效会话=%+v, %v", closed, err)
	}
}

func TestCloseTabPushesUpdatedTabs(t *testing.T) {
	manager, _ := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.NewTab(context.Background(), OpenRequest{SessionID: opened.SessionID, URL: "https://example.org"})
	if err != nil {
		t.Fatal(err)
	}
	subscribeCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates, err := manager.SubscribeState(subscribeCtx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CloseTab(context.Background(), TabRequest{SessionID: opened.SessionID, PageID: opened.PageID}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-updates:
	case <-time.After(2 * time.Second):
		t.Fatal("关闭 Tab 后没有状态推送")
	}
	view, err := manager.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Tabs) != 1 || view.Tabs[0].PageID != second.PageID {
		t.Fatalf("关闭后的 Tab 投影=%+v", view.Tabs)
	}
}
