//go:build browser_smoke

package browserkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// macOS 默认不绑定 127.0.0.2 等额外地址。回退到同一 loopback 的 localhost
// 域名，仍与父页面的 127.0.0.1 属于不同站点，不跳过 OOPIF 的真实验证。
func listenCrossSite(address string) (net.Listener, error) {
	listener, err := net.Listen("tcp", address)
	if err == nil {
		return listener, nil
	}
	listener, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	return crossSiteListener{Listener: listener, address: crossSiteAddress(net.JoinHostPort("localhost", port))}, nil
}

type crossSiteListener struct {
	net.Listener
	address net.Addr
}

func (listener crossSiteListener) Addr() net.Addr { return listener.address }

type crossSiteAddress string

func (address crossSiteAddress) Network() string { return "tcp" }
func (address crossSiteAddress) String() string  { return string(address) }

func TestRealBrowserNetworkPolicyBlocksPrivateSubresources(t *testing.T) {
	var privateRequests atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Hostname() {
		case "public.test":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<!doctype html><title>Policy</title><script>fetch('http://private.test/secret').catch(() => document.body.dataset.blocked = 'true')</script>`))
		case "private.test":
			privateRequests.Add(1)
			_, _ = w.Write([]byte("private data"))
		default:
			http.Error(w, "unexpected proxy target", http.StatusBadGateway)
		}
	}))
	defer proxy.Close()
	manager, err := NewManager(Config{
		AllowEvaluate: true,
		DNSResolver: policyResolver{addresses: map[string][]net.IP{
			"public.test":  {net.ParseIP("93.184.216.34")},
			"private.test": {net.ParseIP("127.0.0.1")},
		}},
		Session: SessionConfig{Proxy: proxy.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "http://public.test/"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		state, evaluateErr := manager.Evaluate(context.Background(), EvaluateRequest{
			SessionID: opened.SessionID,
			PageID:    opened.PageID,
			Code:      `document.body.dataset.blocked`,
		})
		if evaluateErr == nil && string(state.Value) == `"true"` {
			if privateRequests.Load() != 0 {
				t.Fatalf("private subresource reached proxy %d times", privateRequests.Load())
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("browser did not observe the blocked subresource; private requests=%d", privateRequests.Load())
}

func TestRealBrowserManagerWorkflow(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Main</title>
<style>@keyframes drift { from { transform: translateX(0) } to { transform: translateX(1px) } } #apply { animation: drift 1s linear infinite alternate }</style>
<label>Name <input aria-label="Name" onkeydown="if(event.key === 'Enter') document.querySelector('#result').textContent = 'enter:' + this.value"></label>
<button id="apply" onclick="document.querySelector('#result').textContent = document.querySelector('input').value">Apply</button>
<a href="/popup" target="_blank">Popup</a><div id="result"></div>`))
	})
	mux.HandleFunc("/popup", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Popup</title><h1>Popup ready</h1>`))
	})
	mux.HandleFunc("/other", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Other</title><h1>Other page</h1>`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	manager, err := NewManager(Config{
		AllowEvaluate: true, AllowPrivateNetwork: true,
		ScreenshotDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()

	open := functionByName(t, manager, "browserOpen").Handler.(func(context.Context, OpenRequest) (OpenResponse, error))
	snapshotPage := functionByName(t, manager, "browserSnapshot").Handler.(func(context.Context, SnapshotRequest) (SnapshotResponse, error))
	typeText := functionByName(t, manager, "browserType").Handler.(func(context.Context, TypeRequest) (TypeResponse, error))
	opened, err := open(ctx, OpenRequest{
		URL: server.URL, Viewport: Viewport{Width: 960, Height: 640},
	})
	if err != nil {
		t.Fatal(err)
	}
	if opened.LoadStatus != OpenLoadLoaded || opened.Title != "Main" {
		t.Fatalf("browserOpen=%+v", opened)
	}

	snapshot, err := snapshotPage(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	inputRef := smokeElementRef(t, snapshot, "textbox", "Name")
	applyRef := smokeElementRef(t, snapshot, "button", "Apply")
	popupRef := smokeElementRef(t, snapshot, "link", "Popup")
	if _, err := typeText(ctx, TypeRequest{
		SessionID: opened.SessionID, PageID: opened.PageID,
		Ref: inputRef, Revision: snapshot.Revision, Text: "browserkit",
	}); err != nil {
		t.Fatal(err)
	}
	if err := manager.DispatchLiveInput(ctx, opened.PageID, InputEvent{Kind: "shortcut", Action: "select_all"}); err != nil {
		t.Fatal(err)
	}
	if err := manager.DispatchLiveInput(ctx, opened.PageID, InputEvent{Kind: "text", Text: "shortcut"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		value, evaluateErr := manager.Evaluate(ctx, EvaluateRequest{
			SessionID: opened.SessionID, PageID: opened.PageID, Code: `document.querySelector('input').value`,
		})
		if evaluateErr == nil && string(value.Value) == `"shortcut"` {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("直播快捷键没有在真实页面全选文本: value=%s err=%v", value.Value, evaluateErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, event := range []InputEvent{
		{Kind: "shortcut", Action: "select_all"},
		{Kind: "shortcut", Action: "copy"},
		{Kind: "text", Text: "changed"},
		{Kind: "shortcut", Action: "select_all"},
		{Kind: "shortcut", Action: "paste"},
	} {
		if err := manager.DispatchLiveInput(ctx, opened.PageID, event); err != nil {
			t.Fatal(err)
		}
	}
	deadline = time.Now().Add(time.Second)
	for {
		value, evaluateErr := manager.Evaluate(ctx, EvaluateRequest{
			SessionID: opened.SessionID, PageID: opened.PageID, Code: `document.querySelector('input').value`,
		})
		if evaluateErr == nil && string(value.Value) == `"shortcut"` {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("直播复制粘贴没有在真实页面恢复文本: value=%s err=%v", value.Value, evaluateErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := manager.Press(ctx, PressRequest{SessionID: opened.SessionID, PageID: opened.PageID, Key: "Enter"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Wait(ctx, WaitRequest{
		SessionID: opened.SessionID, PageID: opened.PageID,
		Condition: "selector_visible", RefOrSelector: "#result", TimeoutMS: 1_000,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Wait(ctx, WaitRequest{
		SessionID: opened.SessionID, PageID: opened.PageID,
		Condition: "selector_visible", RefOrSelector: inputRef, Revision: snapshot.Revision, TimeoutMS: 1_000,
	}); err != nil {
		t.Fatalf("browserWait 不支持 snapshot ref: %v", err)
	}
	pressed, err := manager.Evaluate(ctx, EvaluateRequest{
		SessionID: opened.SessionID, PageID: opened.PageID,
		Code: `document.querySelector('#result').textContent`,
	})
	if err != nil || string(pressed.Value) != `"enter:shortcut"` {
		t.Fatalf("browserPress=%s, %v", pressed.Value, err)
	}
	if _, err := manager.Click(ctx, ClickRequest{
		SessionID: opened.SessionID, PageID: opened.PageID,
		Ref: applyRef, Revision: snapshot.Revision,
	}); err != nil {
		t.Fatal(err)
	}
	evaluated, err := manager.Evaluate(ctx, EvaluateRequest{
		SessionID: opened.SessionID, PageID: opened.PageID,
		Code: `document.querySelector('#result').textContent`,
	})
	if err != nil || string(evaluated.Value) != `"shortcut"` {
		t.Fatalf("browserEvaluate=%s, %v", evaluated.Value, err)
	}
	shot, err := manager.Screenshot(ctx, ScreenshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil || shot.Bytes == 0 || shot.Format != "png" {
		t.Fatalf("browserScreenshot=%+v, %v", shot, err)
	}
	center, err := manager.Evaluate(ctx, EvaluateRequest{SessionID: opened.SessionID, PageID: opened.PageID,
		Code: `(() => { const r = document.querySelector('#apply').getBoundingClientRect(); return {x: r.left + r.width / 2, y: r.top + r.height / 2} })()`,
	})
	if err != nil {
		t.Fatal(err)
	}
	var point struct{ X, Y float64 }
	if err := json.Unmarshal(center.Value, &point); err != nil {
		t.Fatal(err)
	}
	selected, err := manager.InspectPoint(ctx, InspectPointRequest{SessionID: opened.SessionID, PageID: opened.PageID, X: point.X, Y: point.Y})
	if err != nil || selected.Ref == "" || selected.Revision == 0 || selected.Role != "button" || selected.Name != "Apply" {
		t.Fatalf("browserInspectPoint=%+v, %v", selected, err)
	}
	if _, err := manager.Click(ctx, ClickRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: selected.Ref, Revision: selected.Revision}); err != nil {
		t.Fatalf("click inspected ref: %v", err)
	}
	region, err := manager.Screenshot(ctx, ScreenshotRequest{SessionID: opened.SessionID, PageID: opened.PageID,
		Region: &ScreenshotRegion{X: 10, Y: 10, Width: 40, Height: 30, ViewportWidth: 960, ViewportHeight: 640},
	})
	if err != nil {
		t.Fatal(err)
	}
	croppedImage, _, err := image.Decode(bytes.NewReader(region.Data))
	if err != nil {
		t.Fatal(err)
	}
	if croppedImage.Bounds().Dx() != 40 || croppedImage.Bounds().Dy() != 30 {
		t.Fatalf("region screenshot bounds=%v", croppedImage.Bounds())
	}

	if _, err := manager.Click(ctx, ClickRequest{
		SessionID: opened.SessionID, PageID: opened.PageID,
		Ref: popupRef, Revision: snapshot.Revision, WaitAfterMS: 200,
	}); err != nil {
		t.Fatal(err)
	}
	tabs := waitForSmokeTabs(t, manager, opened.SessionID, 2)
	popupID := tabs.ActivePageID
	if popupID == opened.PageID {
		t.Fatalf("Popup 没有成为 active Tab: %+v", tabs)
	}
	if _, err := manager.Screenshot(ctx, ScreenshotRequest{SessionID: opened.SessionID, PageID: opened.PageID}); err != nil {
		t.Fatalf("后台 Tab 截图失败: %v", err)
	}
	tabs, err = manager.ListTabs(ctx, TabsRequest{SessionID: opened.SessionID})
	if err != nil || tabs.ActivePageID != popupID {
		t.Fatalf("后台 Tab 截图改变了 active Tab: %+v, %v", tabs, err)
	}
	state, err := manager.LiveState(ctx)
	if err != nil || state.Title != "Popup" {
		t.Fatalf("Popup LiveState=%+v, %v", state, err)
	}
	frame, err := manager.LiveFrame(ctx)
	if err != nil || len(frame.Data) == 0 || !strings.HasPrefix(frame.MIME, "image/") {
		t.Fatalf("Popup LiveFrame=%+v, %v", frame, err)
	}
	liveCtx, stopLive := context.WithCancel(ctx)
	liveFrames, err := manager.SubscribeLiveFrames(liveCtx)
	if err != nil {
		stopLive()
		t.Fatal(err)
	}
	waitForSmokeFrameSize(t, liveFrames, 960, 640)
	resizedViewport := Viewport{Width: 900, Height: 620}
	if err := manager.SetViewport(ctx, ViewportRequest{SessionID: opened.SessionID, PageID: popupID, Viewport: resizedViewport}); err != nil {
		stopLive()
		t.Fatal(err)
	}
	waitForSmokeFrameSize(t, liveFrames, resizedViewport.Width, resizedViewport.Height)
	stopLive()
	if _, err := manager.ActivateTab(ctx, TabRequest{SessionID: opened.SessionID, PageID: opened.PageID}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CloseTab(ctx, TabRequest{SessionID: opened.SessionID, PageID: popupID}); err != nil {
		t.Fatal(err)
	}
	tabs, err = manager.ListTabs(ctx, TabsRequest{SessionID: opened.SessionID})
	if err != nil || len(tabs.Tabs) != 1 || tabs.ActivePageID != opened.PageID {
		t.Fatalf("关闭 Popup 后 tabs=%+v, %v", tabs, err)
	}

	reused, err := manager.Open(ctx, OpenRequest{SessionID: opened.SessionID, URL: "/other"})
	if err != nil || reused.PageID != opened.PageID || reused.Title != "Other" {
		t.Fatalf("复用 session 导航=%+v, %v", reused, err)
	}
	closed, err := manager.CloseSession(ctx, CloseRequest{SessionID: opened.SessionID})
	if err != nil || !closed.Closed {
		t.Fatalf("browserClose=%+v, %v", closed, err)
	}
	closed, err = manager.CloseSession(ctx, CloseRequest{SessionID: opened.SessionID})
	if err != nil || !closed.Closed {
		t.Fatalf("重复 browserClose=%+v, %v", closed, err)
	}
}

func TestRealBrowserTypeRejectsReadonlyAndClearsText(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Type guard</title>
<input id="readonly" aria-label="Readonly" readonly value="fixed">
<input id="disabled" aria-label="Disabled" disabled value="fixed">
<input id="date" aria-label="Date" type="date">
<input id="number" aria-label="Number" type="number" value="7">
<input id="image" aria-label="Image" type="image" alt="Image">
<select id="select" aria-label="Select"><option>A</option></select>
<input id="editable" aria-label="Editable" value="old">
<script>window.inputEvents = 0; document.querySelector('#editable').addEventListener('input', () => window.inputEvents++)</script>`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true, AllowEvaluate: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: server.URL, Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	readonlyRef := smokeElementRef(t, snapshot, "textbox", "Readonly")
	disabledRef := smokeElementRef(t, snapshot, "textbox", "Disabled")
	numberRef := smokeElementRef(t, snapshot, "spinbutton", "Number")
	imageRef := smokeElementRef(t, snapshot, "button", "Image")
	selectRef := smokeElementRef(t, snapshot, "combobox", "Select")
	editableRef := smokeElementRef(t, snapshot, "textbox", "Editable")
	for _, test := range []struct {
		ref  string
		kind string
	}{
		{readonlyRef, "element_readonly"},
		{disabledRef, "element_disabled"},
		{imageRef, "element_not_editable"},
	} {
		_, err := manager.Type(ctx, TypeRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: test.ref, Revision: snapshot.Revision, Text: "changed"})
		var browserErr *Error
		if !errors.As(err, &browserErr) || browserErr.Kind != test.kind {
			t.Fatalf("Type(%s) 错误=%v，期望 %s", test.kind, err, test.kind)
		}
	}
	_, err = manager.Type(ctx, TypeRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: selectRef, Revision: snapshot.Revision, Text: "B"})
	var selectErr *Error
	if !errors.As(err, &selectErr) || selectErr.Kind != "element_not_editable" || !strings.Contains(selectErr.RecommendedNextStep, "browserSelectOption") {
		t.Fatalf("select 的 Type 错误缺少 browserSelectOption 建议: %v", err)
	}
	if _, err := manager.Type(ctx, TypeRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: numberRef, Revision: snapshot.Revision, Text: "abc"}); err == nil {
		t.Fatal("number 输入非法文本不应报告成功")
	} else {
		var browserErr *Error
		if !errors.As(err, &browserErr) || browserErr.Kind != "invalid_value" {
			t.Fatalf("number 非法文本错误=%v", err)
		}
	}
	numberValue, err := manager.Evaluate(ctx, EvaluateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Code: `document.querySelector('#number').value`})
	if err != nil || string(numberValue.Value) != `"7"` {
		t.Fatalf("非法 number 输入不应改变原值: value=%s err=%v", numberValue.Value, err)
	}
	if _, err := manager.Type(ctx, TypeRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: numberRef, Revision: snapshot.Revision, Text: "2"}); err != nil {
		t.Fatalf("number 输入不应被拒绝: %v", err)
	}
	numberValue, err = manager.Evaluate(ctx, EvaluateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Code: `document.querySelector('#number').value`})
	if err != nil || string(numberValue.Value) != `"2"` {
		t.Fatalf("number 输入值=%s err=%v", numberValue.Value, err)
	}
	if _, err := manager.Type(ctx, TypeRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: editableRef, Revision: snapshot.Revision, Text: ""}); err != nil {
		t.Fatal(err)
	}
	value, err := manager.Evaluate(ctx, EvaluateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Code: `document.querySelector('#editable').value`})
	if err != nil || string(value.Value) != `""` {
		t.Fatalf("清空输入框失败: value=%s err=%v", value.Value, err)
	}
	if _, err := manager.Type(ctx, TypeRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: editableRef, Revision: snapshot.Revision, Text: "new", Append: true}); err != nil {
		t.Fatal(err)
	}
	value, err = manager.Evaluate(ctx, EvaluateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Code: `document.querySelector('#editable').value`})
	if err != nil || string(value.Value) != `"new"` {
		t.Fatalf("追加输入失败: value=%s err=%v", value.Value, err)
	}
}

func TestRealBrowserTypeSpecializedInputs(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Typed values</title>
<input id="date" aria-label="Date" type="date" min="2026-01-01" max="2026-12-31">
<input id="month" aria-label="Month" type="month">
<input id="week" aria-label="Week" type="week">
<input id="time" aria-label="Time" type="time">
<input id="datetime" aria-label="DateTime" type="datetime-local">
<input id="color" aria-label="Color" type="color">
<input id="range" aria-label="Range" type="range" min="0" max="10" step="1">
<input id="range-wide" aria-label="RangeWide" type="range" min="0" max="100" step="1" value="7">
<input id="free-range" aria-label="FreeRange" type="range" min="0" max="10" step="any">
<input id="readonly-range" aria-label="ReadonlyRange" type="range" min="0" max="10" value="4" readonly>
<input id="readonly-color" aria-label="ReadonlyColor" type="color" value="#112233" readonly>
<input id="number" aria-label="Number" type="number" min="0" max="10" step="2" value="4">
<input id="append-number" aria-label="AppendNumber" type="number" value="4">
<input id="range-large" aria-label="LargeRange" type="range" min="1000000" max="2000000" step="0.01" value="1000000">
<input id="range-invalid" aria-label="InvalidAttrs" type="range" min="+5" max="0x10" step="+2" value="+5">
<input id="range-huge" aria-label="HugeRange" type="range" min="1000000000000000" max="1000000000000100" step="0.0001" value="1000000000000000">
<input id="range-1e16" aria-label="Range1e16" type="range" min="10000000000000000" max="10000000000000100" step="1" value="10000000000000000">
<input id="number-invalid" aria-label="InvalidNumber" type="number" min="10" max="0" value="10">
<input id="bad-number" aria-label="BadNumber" type="number" value="5">
<script>
window.valueEvents = {};
for (const element of document.querySelectorAll('input')) {
  const counts = {input: 0, change: 0};
  window.valueEvents[element.id] = counts;
  element.addEventListener('input', () => counts.input++);
  element.addEventListener('change', () => counts.change++);
}
</script>`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true, AllowEvaluate: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: server.URL, Viewport: Viewport{Width: 960, Height: 800}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	readonlyByLabel := map[string]bool{}
	for _, element := range snapshot.Elements {
		readonlyByLabel[element.Name] = element.ReadOnly
	}
	for _, label := range []string{"ReadonlyRange", "ReadonlyColor"} {
		if readonlyByLabel[label] {
			t.Fatalf("%s 不应在快照中标记为 readonly", label)
		}
	}
	read := func(expression string) string {
		t.Helper()
		value, err := manager.Evaluate(ctx, EvaluateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Code: expression})
		if err != nil {
			t.Fatal(err)
		}
		return string(value.Value)
	}
	if got := read(`document.querySelector('#range-invalid').value`); got != `"50"` {
		t.Fatalf("非法 range 属性应按默认值初始化: %s", got)
	}
	valid := []struct {
		label string
		role  string
		text  string
		value string
		id    string
	}{
		{"Date", "textbox", "2026-10-21", `"2026-10-21"`, "date"},
		{"Month", "textbox", "2026-10", `"2026-10"`, "month"},
		{"Week", "textbox", "2026-W42", `"2026-W42"`, "week"},
		{"Time", "textbox", "23:59", `"23:59"`, "time"},
		{"DateTime", "textbox", "2026-10-21T23:59", `"2026-10-21T23:59"`, "datetime"},
		{"Color", "textbox", "#A1b2c3", `"#a1b2c3"`, "color"},
		{"Range", "slider", "7", `"7"`, "range"},
		{"RangeWide", "slider", "5e0", `"5"`, "range-wide"},
		{"FreeRange", "slider", "5.5", `"5.5"`, "free-range"},
		{"ReadonlyRange", "slider", "6", `"6"`, "readonly-range"},
		{"ReadonlyColor", "textbox", "#abcdef", `"#abcdef"`, "readonly-color"},
		{"Number", "spinbutton", "8", `"8"`, "number"},
		{"LargeRange", "slider", "1000000.02", `"1000000.02"`, "range-large"},
		{"InvalidAttrs", "slider", "40", `"40"`, "range-invalid"},
	}
	for _, test := range valid {
		ref := smokeElementRef(t, snapshot, test.role, test.label)
		if _, err := manager.Type(ctx, TypeRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: ref, Revision: snapshot.Revision, Text: test.text}); err != nil {
			t.Fatalf("Type(%s, %q)=%v", test.label, test.text, err)
		}
		if got := read(`document.querySelector('#` + test.id + `').value`); got != test.value {
			t.Fatalf("%s value=%s，期望 %s", test.label, got, test.value)
		}
		if got := read(`window.valueEvents['` + test.id + `'].input`); got != "1" {
			t.Fatalf("%s input 事件=%s，期望 1", test.label, got)
		}
		if got := read(`window.valueEvents['` + test.id + `'].change`); got != "1" {
			t.Fatalf("%s change 事件=%s，期望 1", test.label, got)
		}
	}
	dateRef := smokeElementRef(t, snapshot, "textbox", "Date")
	if _, err := manager.Type(ctx, TypeRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: dateRef, Revision: snapshot.Revision, Text: "2026-10-21"}); err != nil {
		t.Fatalf("重复写入相同值失败: %v", err)
	}
	if got := read(`window.valueEvents.date.input`); got != "1" {
		t.Fatalf("重复写入相同值不应再触发 input 事件: %s", got)
	}
	if got := read(`window.valueEvents.date.change`); got != "1" {
		t.Fatalf("重复写入相同值不应再触发 change 事件: %s", got)
	}
	appendRef := smokeElementRef(t, snapshot, "spinbutton", "AppendNumber")
	if _, err := manager.Type(ctx, TypeRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: appendRef, Revision: snapshot.Revision, Text: "2", Append: true}); err != nil {
		t.Fatalf("number 追加输入失败: %v", err)
	}
	if got := read(`document.querySelector('#append-number').value`); got != `"42"` {
		t.Fatalf("number 追加后的值=%s，期望 42", got)
	}
	if got := read(`window.valueEvents['append-number'].input`); got != "1" {
		t.Fatalf("number 追加 input 事件=%s，期望 1", got)
	}
	for _, test := range []struct {
		label string
		role  string
		text  string
		id    string
		want  string
	}{
		{"Date", "textbox", "2026-02-30", "date", `"2026-10-21"`},
		{"Date", "textbox", "2027-01-01", "date", `"2026-10-21"`},
		{"Date", "textbox", "0000-01-01", "date", `"2026-10-21"`},
		{"Month", "textbox", "2026-13", "month", `"2026-10"`},
		{"Week", "textbox", "2026-W54", "week", `"2026-W42"`},
		{"Week", "textbox", "2025-W53", "week", `"2026-W42"`},
		{"Time", "textbox", "25:00", "time", `"23:59"`},
		{"Time", "textbox", "23:59:58", "time", `"23:59"`},
		{"Time", "textbox", "23:59:58.1234", "time", `"23:59"`},
		{"DateTime", "textbox", "2026-02-30T10:00", "datetime", `"2026-10-21T23:59"`},
		{"DateTime", "textbox", "2026-10-21T23:59:58.1234", "datetime", `"2026-10-21T23:59"`},
		{"Color", "textbox", "", "color", `"#a1b2c3"`},
		{"Color", "textbox", "red", "color", `"#a1b2c3"`},
		{"Range", "slider", "99", "range", `"7"`},
		{"Range", "slider", "5.5", "range", `"7"`},
		{"Range", "slider", "5.", "range", `"7"`},
		{"Range", "slider", "+5", "range", `"7"`},
		{"Range", "slider", " 5 ", "range", `"7"`},
		{"RangeWide", "slider", "0x10", "range-wide", `"5"`},
		{"HugeRange", "slider", "1000000000000000.1", "range-huge", `"1000000000000000"`},
		{"Range1e16", "slider", "10000000000000001", "range-1e16", `"10000000000000000"`},
		{"Number", "spinbutton", "99", "number", `"8"`},
		{"Number", "spinbutton", "5", "number", `"8"`},
		{"Number", "spinbutton", "5.", "number", `"8"`},
		{"LargeRange", "slider", "1000000.021", "range-large", `"1000000.02"`},
	} {
		ref := smokeElementRef(t, snapshot, test.role, test.label)
		_, err := manager.Type(ctx, TypeRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: ref, Revision: snapshot.Revision, Text: test.text})
		var browserErr *Error
		if !errors.As(err, &browserErr) || browserErr.Kind != "invalid_value" {
			t.Fatalf("Type(%s, %q) 错误=%v，期望 invalid_value", test.label, test.text, err)
		}
		if got := read(`document.querySelector('#` + test.id + `').value`); got != test.want {
			t.Fatalf("%s 非法值改变了原值: %s，期望 %s", test.label, got, test.want)
		}
		wantEvents := "1"
		if test.id == "range-huge" || test.id == "range-1e16" {
			wantEvents = "0"
		}
		if got := read(`window.valueEvents['` + test.id + `'].input`); got != wantEvents {
			t.Fatalf("%s 非法值 input 事件: %s，期望 %s", test.label, got, wantEvents)
		}
		if got := read(`window.valueEvents['` + test.id + `'].change`); got != wantEvents {
			t.Fatalf("%s 非法值 change 事件: %s，期望 %s", test.label, got, wantEvents)
		}
	}
	if _, err := manager.Type(ctx, TypeRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: appendRef, Revision: snapshot.Revision, Text: ""}); err != nil {
		t.Fatalf("number 清空失败: %v", err)
	}
	if got := read(`document.querySelector('#append-number').value`); got != `""` {
		t.Fatalf("number 清空后的值=%s", got)
	}
	invalidNumberRef := smokeElementRef(t, snapshot, "spinbutton", "InvalidNumber")
	_, err = manager.Type(ctx, TypeRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: invalidNumberRef, Revision: snapshot.Revision, Text: "10"})
	var invalidNumberErr *Error
	if !errors.As(err, &invalidNumberErr) || invalidNumberErr.Kind != "invalid_value" {
		t.Fatalf("无效控件同值写入错误=%v，期望 invalid_value", err)
	}
	if got := read(`document.querySelector('#number-invalid').value`); got != `"10"` {
		t.Fatalf("无效 number 同值写入后的值=%s", got)
	}
	badNumberRef := smokeElementRef(t, snapshot, "spinbutton", "BadNumber")
	if _, err := manager.Press(ctx, PressRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: badNumberRef, Revision: snapshot.Revision, Key: "e"}); err != nil {
		t.Fatalf("number 输入 e 失败: %v", err)
	}
	if got := read(`(() => { const e=document.querySelector('#bad-number'); return e.validity.badInput + ':' + e.value; })()`); got != `"true:"` {
		t.Fatalf("number badInput 状态=%s", got)
	}
	if _, err := manager.Type(ctx, TypeRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: badNumberRef, Revision: snapshot.Revision, Text: ""}); err != nil {
		t.Fatalf("清空 number badInput 失败: %v", err)
	}
	if got := read(`(() => { const e=document.querySelector('#bad-number'); return e.validity.badInput + ':' + e.value; })()`); got != `"false:"` {
		t.Fatalf("清空后 number badInput 状态=%s", got)
	}
}

func TestRealBrowserNavigateHistory(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/a", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Page A</title><a href="/b">to B</a>`))
	})
	mux.HandleFunc("/b", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Page B</title><a href="/a">to A</a>`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: server.URL + "/a", Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.Navigate(ctx, NavigateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Direction: "back"})
	var historyErr *Error
	if !errors.As(err, &historyErr) || historyErr.Kind != "history_unavailable" {
		t.Fatalf("首页后退错误=%v，期望 history_unavailable", err)
	}
	if _, err := manager.Navigate(ctx, NavigateRequest{SessionID: opened.SessionID, PageID: opened.PageID, URL: server.URL + "/b"}); err != nil {
		t.Fatal(err)
	}
	back, err := manager.Navigate(ctx, NavigateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Direction: "back"})
	if err != nil {
		t.Fatalf("历史后退失败: %v", err)
	}
	if back.URL != server.URL+"/a" || back.Title != "Page A" {
		t.Fatalf("历史后退结果=%+v", back)
	}
	forward, err := manager.Navigate(ctx, NavigateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Direction: "forward"})
	if err != nil {
		t.Fatalf("历史前进失败: %v", err)
	}
	if forward.URL != server.URL+"/b" || forward.Title != "Page B" {
		t.Fatalf("历史前进结果=%+v", forward)
	}
	reloaded, err := manager.Navigate(ctx, NavigateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Direction: "reload"})
	if err != nil || reloaded.URL != server.URL+"/b" {
		t.Fatalf("刷新结果=%+v err=%v", reloaded, err)
	}
}

func TestRealBrowserHistoryBeforeUnloadBlocked(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/a", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Page A</title>A`))
	})
	mux.HandleFunc("/guard", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Guard</title>GUARD<button id="arm">ARM</button><script>window.guard = event => { event.preventDefault(); event.returnValue = ''; setTimeout(() => window.removeEventListener('beforeunload', window.guard), 0); }; window.addEventListener('beforeunload', window.guard)</script>`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true, AllowEvaluate: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: server.URL + "/a", Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Navigate(ctx, NavigateRequest{SessionID: opened.SessionID, PageID: opened.PageID, URL: server.URL + "/guard"}); err != nil {
		t.Fatal(err)
	}
	guardSnapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	armRef := smokeElementRef(t, guardSnapshot, "button", "ARM")
	if _, err := manager.Click(ctx, ClickRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: armRef, Revision: guardSnapshot.Revision}); err != nil {
		t.Fatal(err)
	}
	blocked, err := manager.Navigate(ctx, NavigateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Direction: "back"})
	if err != nil || blocked.Snapshot == nil || len(blocked.Snapshot.Dialogs) != 1 || blocked.Snapshot.Dialogs[0].Type != "beforeunload" {
		t.Fatalf("beforeunload 没有作为待处理弹窗返回: response=%+v err=%v", blocked, err)
	}
	if _, err := manager.HandleDialog(ctx, HandleDialogRequest{
		SessionID: opened.SessionID, PageID: opened.PageID,
		DialogID: blocked.Snapshot.Dialogs[0].ID, Action: "dismiss",
	}); err != nil {
		t.Fatalf("取消 beforeunload 失败: %v", err)
	}
	snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.URL != server.URL+"/guard" {
		t.Fatalf("beforeunload 阻止后仍离开了页面: %s", snapshot.URL)
	}
	// 页面在首次 beforeunload 后通过页面内定时器移除处理器；两次导航之间
	// 不调用 Manager，确保验证的是旧弹窗事实不会再阻塞后续导航。
	back, err := manager.Navigate(ctx, NavigateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Direction: "back"})
	if err != nil {
		t.Fatalf("自行移除 beforeunload 后历史后退失败: %v", err)
	}
	if back.URL != server.URL+"/a" {
		t.Fatalf("自行移除 beforeunload 后后退结果=%s", back.URL)
	}
	if len(back.Dialogs) != 0 {
		t.Fatalf("成功后退仍带出旧弹窗: %+v", back.Dialogs)
	}
}

func TestRealBrowserPendingDialogPrecedesHistoryNavigation(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/a", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>A</title>A`))
	})
	mux.HandleFunc("/guard", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Guard</title>GUARD<button id="arm">ARM</button><script>window.guard = event => { event.preventDefault(); event.returnValue = ''; }; window.addEventListener('beforeunload', window.guard);</script>`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true, AllowEvaluate: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: server.URL + "/a", Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Navigate(ctx, NavigateRequest{SessionID: opened.SessionID, PageID: opened.PageID, URL: server.URL + "/guard"}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	ref := smokeElementRef(t, snapshot, "button", "ARM")
	if _, err := manager.Evaluate(ctx, EvaluateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Code: `document.querySelector('#arm').addEventListener('click', () => setTimeout(() => alert('unrelated'), 100))`}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Click(ctx, ClickRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: ref, Revision: snapshot.Revision}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	started := time.Now()
	_, err = manager.Navigate(ctx, NavigateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Direction: "back"})
	var browserErr *Error
	if !errors.As(err, &browserErr) || browserErr.Kind != "dialog_open" {
		t.Fatalf("已有 alert 时历史导航错误=%v，期望 dialog_open", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("已有弹窗时历史导航没有立即让路: %s", elapsed)
	}
	pending, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil || len(pending.Dialogs) != 1 || pending.Dialogs[0].Type != "alert" || pending.Dialogs[0].Message != "unrelated" {
		t.Fatalf("历史导航丢失了优先级更高的 alert: snapshot=%+v err=%v", pending, err)
	}
	if _, err := manager.HandleDialog(ctx, HandleDialogRequest{
		SessionID: opened.SessionID, PageID: opened.PageID,
		DialogID: pending.Dialogs[0].ID, Action: "accept",
	}); err != nil {
		t.Fatal(err)
	}
	blocked, err := manager.Navigate(ctx, NavigateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Direction: "back"})
	if err != nil || blocked.Snapshot == nil || len(blocked.Snapshot.Dialogs) != 1 || blocked.Snapshot.Dialogs[0].Type != "beforeunload" {
		t.Fatalf("处理 alert 后没有暴露 beforeunload: response=%+v err=%v", blocked, err)
	}
	if _, err := manager.HandleDialog(ctx, HandleDialogRequest{
		SessionID: opened.SessionID, PageID: opened.PageID,
		DialogID: blocked.Snapshot.Dialogs[0].ID, Action: "dismiss",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRealBrowserHistorySameURLPushState(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Push State</title><button id="push" onclick="history.pushState({step:1}, '', location.href)">PUSH</button>`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: server.URL, Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	ref := smokeElementRef(t, snapshot, "button", "PUSH")
	if _, err := manager.Click(ctx, ClickRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: ref, Revision: snapshot.Revision}); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	back, err := manager.Navigate(ctx, NavigateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Direction: "back"})
	if err != nil {
		t.Fatalf("同 URL pushState 后退失败: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("同 URL pushState 后退耗时 %s", elapsed)
	}
	if back.URL != server.URL+"/" {
		t.Fatalf("同 URL pushState 后退结果=%s", back.URL)
	}
}

func TestRealBrowserHistoryDelayedBeforeUnload(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/a", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>A</title>A`))
	})
	mux.HandleFunc("/guard", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Guard</title>GUARD<button id="arm">ARM</button><script>window.guard = event => { event.preventDefault(); event.returnValue = ''; const until = Date.now() + 250; while (Date.now() < until) {} }; window.addEventListener('beforeunload', window.guard)</script>`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: server.URL + "/a", Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Navigate(ctx, NavigateRequest{SessionID: opened.SessionID, PageID: opened.PageID, URL: server.URL + "/guard"}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	ref := smokeElementRef(t, snapshot, "button", "ARM")
	if _, err := manager.Click(ctx, ClickRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: ref, Revision: snapshot.Revision}); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	blocked, err := manager.Navigate(ctx, NavigateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Direction: "back"})
	if err != nil || blocked.Snapshot == nil || len(blocked.Snapshot.Dialogs) != 1 || blocked.Snapshot.Dialogs[0].Type != "beforeunload" {
		t.Fatalf("延迟 beforeunload 没有进入页面状态: response=%+v err=%v", blocked, err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("延迟 beforeunload 锁住历史导航: %s", elapsed)
	}
	if _, err := manager.HandleDialog(ctx, HandleDialogRequest{
		SessionID: opened.SessionID, PageID: opened.PageID,
		DialogID: blocked.Snapshot.Dialogs[0].ID, Action: "dismiss",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRealBrowserHistoryRedirectToErrorPage(t *testing.T) {
	var loads atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/dead", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		if loads.Add(1) > 1 {
			http.Redirect(writer, request, "http://127.0.0.1:1/", http.StatusFound)
			return
		}
		_, _ = writer.Write([]byte(`<!doctype html><title>Dead</title><script>window.addEventListener('unload', function(){})</script>DEAD`))
	})
	mux.HandleFunc("/other", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Other</title>OTHER`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: server.URL + "/dead", Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Navigate(ctx, NavigateRequest{SessionID: opened.SessionID, PageID: opened.PageID, URL: server.URL + "/other"}); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = manager.Navigate(ctx, NavigateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Direction: "back"})
	var browserErr *Error
	if !errors.As(err, &browserErr) || browserErr.Kind != "navigation_failed" {
		t.Fatalf("重定向到错误页结果=%v，期望 navigation_failed", err)
	}
	if elapsed := time.Since(started); elapsed > 4*time.Second {
		t.Fatalf("错误页分类耗时 %s", elapsed)
	}
}

func TestRealBrowserHistoryIgnoresIframeSameDocumentEvents(t *testing.T) {
	var loads atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/a", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		if loads.Add(1) > 1 {
			time.Sleep(600 * time.Millisecond)
		}
		_, _ = writer.Write([]byte(`<!doctype html><title>Page A</title><script>window.addEventListener('unload', function(){})</script>A`))
	})
	mux.HandleFunc("/b", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Page B</title><iframe srcdoc="&lt;script&gt;setInterval(() =&gt; history.replaceState({}, '', '/a'), 20)&lt;/script&gt;"></iframe>B`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: server.URL + "/a", Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Navigate(ctx, NavigateRequest{SessionID: opened.SessionID, PageID: opened.PageID, URL: server.URL + "/b"}); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	back, err := manager.Navigate(ctx, NavigateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Direction: "back"})
	if err != nil {
		t.Fatalf("iframe 同文档事件下的历史后退失败: %v", err)
	}
	if elapsed := time.Since(started); elapsed < 400*time.Millisecond {
		t.Fatalf("iframe 同文档事件让后退提前成功: %s", elapsed)
	}
	if back.URL != server.URL+"/a" || back.Title != "Page A" {
		t.Fatalf("iframe 同文档事件下后退结果=%+v", back)
	}
}

func TestRealBrowserHistoryIgnoresMainFrameReplaceState(t *testing.T) {
	var loads atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/a", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		if loads.Add(1) > 1 {
			time.Sleep(600 * time.Millisecond)
		}
		_, _ = writer.Write([]byte(`<!doctype html><title>Page A</title><script>window.addEventListener('unload', function(){})</script>A`))
	})
	mux.HandleFunc("/b", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Page B</title><script>setInterval(() => history.replaceState({}, '', location.href), 20)</script>B`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: server.URL + "/a", Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Navigate(ctx, NavigateRequest{SessionID: opened.SessionID, PageID: opened.PageID, URL: server.URL + "/b"}); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	back, err := manager.Navigate(ctx, NavigateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Direction: "back"})
	if err != nil {
		t.Fatalf("主 frame replaceState 下的历史后退失败: %v", err)
	}
	if elapsed := time.Since(started); elapsed < 400*time.Millisecond {
		t.Fatalf("主 frame replaceState 让后退提前成功: %s", elapsed)
	}
	if back.URL != server.URL+"/a" || back.Title != "Page A" {
		t.Fatalf("主 frame replaceState 下后退结果=%+v", back)
	}
}

func TestRealBrowserHistoryRedirectTarget(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/source", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Source</title><script>var armed = sessionStorage.getItem('redirect-back') === '1'; sessionStorage.setItem('redirect-back','1'); window.addEventListener('pageshow', function(event){ if (armed || event.persisted) { location.replace('/target'); } }); window.addEventListener('unload', function(){});</script>`))
	})
	mux.HandleFunc("/target", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Redirect Target</title>TARGET`))
	})
	mux.HandleFunc("/other", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Other</title>OTHER`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: server.URL + "/source", Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Navigate(ctx, NavigateRequest{SessionID: opened.SessionID, PageID: opened.PageID, URL: server.URL + "/other"}); err != nil {
		t.Fatal(err)
	}
	back, err := manager.Navigate(ctx, NavigateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Direction: "back"})
	if err != nil {
		t.Fatalf("历史重定向后退失败: %v", err)
	}
	if back.URL != server.URL+"/target" {
		t.Fatalf("历史重定向后退结果=%s", back.URL)
	}
}

func TestRealBrowserHistoryRedirectToSameURL(t *testing.T) {
	mux := http.NewServeMux()
	var loads atomic.Int32
	mux.HandleFunc("/a", func(writer http.ResponseWriter, _ *http.Request) {
		loads.Add(1)
		writer.Header().Set("Cache-Control", "no-store")
		_, _ = writer.Write([]byte(`<!doctype html><title>A</title><script>var armed = sessionStorage.getItem('a-seen') === '1'; sessionStorage.setItem('a-seen','1'); window.addEventListener('pageshow', function(event){ if (armed || event.persisted) { location.replace('/b'); } });</script><p>A</p>`))
	})
	var reloads atomic.Int32
	mux.HandleFunc("/b", func(writer http.ResponseWriter, _ *http.Request) {
		reloads.Add(1)
		writer.Header().Set("Cache-Control", "no-store")
		_, _ = writer.Write([]byte(`<!doctype html><title>B</title><p>B</p>`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: server.URL + "/a", Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Navigate(ctx, NavigateRequest{SessionID: opened.SessionID, PageID: opened.PageID, URL: server.URL + "/b"}); err != nil {
		t.Fatal(err)
	}
	// 后退到 /a 时页面立刻 location.replace('/b')，最终 URL 与出发页相同。
	back, err := manager.Navigate(ctx, NavigateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Direction: "back"})
	if err != nil {
		t.Fatalf("同 URL 重定向后退失败: %v", err)
	}
	if back.URL != server.URL+"/b" {
		t.Fatalf("同 URL 重定向后退结果=%s", back.URL)
	}
	if loads.Load() < 2 && reloads.Load() < 2 {
		t.Fatalf("历史后退没有真正经过 /a 或重新加载 /b：a=%d b=%d", loads.Load(), reloads.Load())
	}
}

func TestRealBrowserSnapshotValueBudget(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, _ *http.Request) {
		var body strings.Builder
		body.WriteString("<!doctype html><title>Value budget</title>")
		for index := 0; index < 40; index++ {
			body.WriteString(`<input aria-label="Field`)
			body.WriteString(strconv.Itoa(index))
			body.WriteString(`" type="text" value="`)
			body.WriteString(strings.Repeat("x", 200))
			body.WriteString(`">`)
		}
		_, _ = writer.Write([]byte(body.String()))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: server.URL, Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID, MaxElements: 100})
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	omittedMarked := false
	for _, element := range snapshot.Elements {
		total += len([]rune(element.Value))
		if element.Value == "" && element.ValueTruncated {
			omittedMarked = true
		}
	}
	if total > 4100 {
		t.Fatalf("快照 value 总量超出预算: %d", total)
	}
	if !omittedMarked {
		t.Fatal("预算耗尽后省略的非空 value 没有标记 value_truncated")
	}
}

func TestRealBrowserClickWaitAfterTimeoutNotice(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Wait notice</title><body>
<button id="slow" onclick="setTimeout(()=>location.href='/done',2000)">GO</button>
</body>`))
	})
	mux.HandleFunc("/done", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Done</title><body>done</body>`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: server.URL, Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	ref := smokeElementRef(t, snapshot, "button", "GO")
	response, err := manager.Click(ctx, ClickRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: ref, Revision: snapshot.Revision, WaitAfterMS: 2000, TimeoutMS: 400})
	if err != nil {
		t.Fatal(err)
	}
	if !response.Changed {
		t.Fatalf("点击应已执行: %+v", response)
	}
	if !strings.Contains(response.SnapshotError, "不要重放点击") {
		t.Fatalf("wait_after_ms 被截断时应给出可操作提示: %q", response.SnapshotError)
	}
}

func TestRealBrowserNestedCrossFrame(t *testing.T) {
	inner := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><body style="margin:0">
<button id="b" style="position:absolute;left:10px;top:10px;width:80px;height:30px" onclick="document.getElementById('out').textContent='NESTED_CLICKED'">CROSS_NESTED</button>
<div id="out" style="position:absolute;left:10px;top:50px">NESTED_READY</div>
</body>`))
	}))
	defer inner.Close()
	middle := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><body style="margin:0">
<iframe src="` + inner.URL + `" style="position:absolute;left:50px;top:50px;width:320px;height:200px;border:0"></iframe>
</body>`))
	}))
	defer middle.Close()
	outer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><body style="margin:0">
<iframe src="` + middle.URL + `" style="position:absolute;left:100px;top:80px;width:420px;height:300px;border:0"></iframe>
</body>`))
	}))
	defer outer.Close()
	uploadDir := t.TempDir()
	uploadPath := filepath.Join(uploadDir, "upload.txt")
	if err := os.WriteFile(uploadPath, []byte("upload-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(Config{AllowPrivateNetwork: true, UploadDir: uploadDir})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: outer.URL, Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	var snapshot SnapshotResponse
	var button *Element
	for attempt := 0; attempt < 20 && button == nil; attempt++ {
		if attempt > 0 {
			time.Sleep(100 * time.Millisecond)
		}
		snapshot, err = manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
		if err != nil {
			t.Fatal(err)
		}
		for index := range snapshot.Elements {
			if snapshot.Elements[index].Name == "CROSS_NESTED" {
				button = &snapshot.Elements[index]
			}
		}
	}
	if button == nil || !strings.HasPrefix(button.Ref, "f") {
		t.Fatalf("嵌套跨域 frame 元素未进入快照: %+v", snapshot.Elements)
	}
	if _, err := manager.Click(ctx, ClickRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: button.Ref, Revision: snapshot.Revision}); err != nil {
		t.Fatalf("嵌套跨域点击失败: %v", err)
	}
	updated, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(updated.Text, "NESTED_CLICKED") {
		t.Fatalf("嵌套跨域点击没有生效: %q", updated.Text)
	}
}

func TestRealBrowserCrossFrameRefsSurvivePagination(t *testing.T) {
	inner := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><body style="margin:0">
<button style="position:absolute;left:10px;top:10px;width:100px;height:30px" onclick="document.title='FRAME_CLICKED'">FRAME_BTN</button>
</body>`))
	}))
	defer inner.Close()
	outer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		var body strings.Builder
		body.WriteString(`<!doctype html><body style="margin:0">`)
		for index := 0; index < 30; index++ {
			body.WriteString(`<button>MAIN_` + strconv.Itoa(index) + `</button>`)
		}
		body.WriteString(`<iframe src="` + inner.URL + `" style="position:absolute;left:80px;top:80px;width:400px;height:240px;border:0"></iframe></body>`)
		_, _ = writer.Write([]byte(body.String()))
	}))
	defer outer.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: outer.URL, Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	var first SnapshotResponse
	var frameButton *Element
	for attempt := 0; attempt < 20 && frameButton == nil; attempt++ {
		if attempt > 0 {
			time.Sleep(100 * time.Millisecond)
		}
		first, err = manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
		if err != nil {
			t.Fatal(err)
		}
		for index := range first.Elements {
			if first.Elements[index].Name == "FRAME_BTN" {
				frameButton = &first.Elements[index]
			}
		}
	}
	if frameButton == nil {
		t.Fatalf("frame 按钮未进入快照: %+v", first.Elements)
	}
	if _, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID, MaxElements: 5, Offset: 20}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Click(ctx, ClickRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: frameButton.Ref, Revision: first.Revision}); err != nil {
		t.Fatalf("分页后 frame ref 不应失效: %v", err)
	}
}

func TestRealBrowserFrameClickRejectsInnerOverlay(t *testing.T) {
	inner := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><body style="margin:0">
<button style="position:absolute;left:10px;top:10px;width:120px;height:30px" onclick="this.textContent='UNDER_CLICKED'">UNDER_BTN</button>
<div style="position:absolute;left:0;top:0;width:200px;height:60px;z-index:10" onclick="this.textContent='OVERLAY_HIT'">OVERLAY</div>
</body>`))
	}))
	defer inner.Close()
	outer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Inner Overlay</title><body style="margin:0">
<iframe src="` + inner.URL + `" style="position:absolute;left:100px;top:80px;width:400px;height:240px;border:0"></iframe>
</body>`))
	}))
	defer outer.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: outer.URL, Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	var snapshot SnapshotResponse
	var button *Element
	for attempt := 0; attempt < 20 && button == nil; attempt++ {
		if attempt > 0 {
			time.Sleep(100 * time.Millisecond)
		}
		snapshot, err = manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
		if err != nil {
			t.Fatal(err)
		}
		for index := range snapshot.Elements {
			if snapshot.Elements[index].Name == "UNDER_BTN" {
				button = &snapshot.Elements[index]
			}
		}
	}
	if button == nil {
		t.Fatalf("frame 内按钮未进入快照: %+v", snapshot.Elements)
	}
	if _, err := manager.Click(ctx, ClickRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: button.Ref, Revision: snapshot.Revision}); err == nil || !strings.Contains(err.Error(), "遮挡") {
		t.Fatalf("frame 内覆盖层应拒绝点击，得到 %v", err)
	}
	after, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(after.Text, "OVERLAY_HIT") || strings.Contains(after.Text, "UNDER_CLICKED") {
		t.Fatalf("覆盖层或底层按钮被误点: %q", after.Text)
	}
}

func TestRealBrowserOOPIFClickCannotEscapeFrame(t *testing.T) {
	listener, err := listenCrossSite("127.0.0.2:0")
	if err != nil {
		t.Skipf("127.0.0.2 不可用: %v", err)
	}
	inner := &httptest.Server{Listener: listener, Config: &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Escape</title><body style="margin:0">
<button id="escape" style="position:fixed;left:500px;top:10px;width:120px;height:30px" onclick="document.title='FRAME_HIT'">ESCAPE_BTN</button>
</body>`))
	})}}
	inner.Start()
	defer inner.Close()
	outer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Parent</title><body style="margin:0">
<button id="parent" style="position:fixed;left:560px;top:80px;width:120px;height:30px" onclick="document.title='PARENT_HIT'">PARENT_BTN</button>
<iframe src="` + inner.URL + `" style="position:absolute;left:100px;top:80px;width:400px;height:240px;border:0"></iframe>
</body>`))
	}))
	defer outer.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: outer.URL, Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	var snapshot SnapshotResponse
	var escape *Element
	for attempt := 0; attempt < 30 && escape == nil; attempt++ {
		if attempt > 0 {
			time.Sleep(100 * time.Millisecond)
		}
		snapshot, err = manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
		if err != nil {
			t.Fatal(err)
		}
		for index := range snapshot.Elements {
			if snapshot.Elements[index].Name == "ESCAPE_BTN" {
				escape = &snapshot.Elements[index]
			}
		}
	}
	if escape == nil {
		t.Fatalf("越界按钮未进入快照: %+v", snapshot.Elements)
	}
	if _, err := manager.Click(ctx, ClickRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: escape.Ref, Revision: snapshot.Revision}); err == nil {
		t.Fatal("越出 iframe 的点击应被拒绝")
	}
	after, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	if after.Title == "PARENT_HIT" {
		t.Fatal("越界点击命中了父页面按钮")
	}
}

func TestRealBrowserFrameClickAfterPageScroll(t *testing.T) {
	inner := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><body style="margin:0">
<button style="position:absolute;left:10px;top:10px;width:120px;height:30px" onclick="this.textContent='SCROLL_CLICKED'; document.title='SCROLL_CLICKED'">SCROLL_BTN</button>
</body>`))
	}))
	defer inner.Close()
	outer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Scroll Host</title><body style="margin:0">
<div style="height:1200px"></div>
<iframe src="` + inner.URL + `" style="position:absolute;left:100px;top:1200px;width:400px;height:240px;border:0"></iframe>
</body>`))
	}))
	defer outer.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true, AllowEvaluate: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: outer.URL, Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Evaluate(ctx, EvaluateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Code: "window.scrollTo(0, 1100)"}); err != nil {
		t.Fatal(err)
	}
	var snapshot SnapshotResponse
	var button *Element
	for attempt := 0; attempt < 20 && button == nil; attempt++ {
		if attempt > 0 {
			time.Sleep(100 * time.Millisecond)
		}
		snapshot, err = manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
		if err != nil {
			t.Fatal(err)
		}
		for index := range snapshot.Elements {
			if snapshot.Elements[index].Name == "SCROLL_BTN" {
				button = &snapshot.Elements[index]
			}
		}
	}
	if button == nil {
		t.Fatalf("滚动后的 frame 按钮未进入快照: %+v", snapshot.Elements)
	}
	before, _ := manager.Evaluate(ctx, EvaluateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Code: "(() => ({scrollY: window.scrollY, rect: (() => { const r = document.querySelector('iframe').getBoundingClientRect(); return {x:r.x,y:r.y,w:r.width,h:r.height}; })()}))()"})
	t.Logf("before=%s", before.Value)
	var scrollState struct {
		ScrollY float64 `json:"scrollY"`
	}
	if err := json.Unmarshal([]byte(before.Value), &scrollState); err != nil || scrollState.ScrollY <= 0 {
		t.Fatalf("测试前提不成立，页面没有滚动: %s", before.Value)
	}
	if _, err := manager.Click(ctx, ClickRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: button.Ref, Revision: snapshot.Revision}); err != nil {
		t.Fatalf("滚动后的跨域点击失败: %v", err)
	}
	afterClick, _ := manager.Evaluate(ctx, EvaluateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Code: "(() => ({scrollY: window.scrollY, rect: (() => { const r = document.querySelector('iframe').getBoundingClientRect(); return {x:r.x,y:r.y,w:r.width,h:r.height}; })()}))()"})
	t.Logf("afterClick=%s", afterClick.Value)
	after, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(after.Text, "SCROLL_CLICKED") {
		t.Fatalf("滚动后的跨域点击未生效: %q", after.Text)
	}
}

func TestRealBrowserFrameClickRejectsCoveredFrame(t *testing.T) {
	inner := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><body style="margin:0">
<button style="position:absolute;left:10px;top:10px;width:120px;height:30px">COVERED_BTN</button>
</body>`))
	}))
	defer inner.Close()
	outer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Covered</title><body style="margin:0">
<iframe src="` + inner.URL + `" style="position:absolute;left:100px;top:80px;width:400px;height:240px;border:0"></iframe>
<div style="position:absolute;left:100px;top:80px;width:400px;height:240px;z-index:10" onclick="document.title='COVER_HIT'"></div>
</body>`))
	}))
	defer outer.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: outer.URL, Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	var snapshot SnapshotResponse
	var button *Element
	for attempt := 0; attempt < 20 && button == nil; attempt++ {
		if attempt > 0 {
			time.Sleep(100 * time.Millisecond)
		}
		snapshot, err = manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
		if err != nil {
			t.Fatal(err)
		}
		for index := range snapshot.Elements {
			if snapshot.Elements[index].Name == "COVERED_BTN" {
				button = &snapshot.Elements[index]
			}
		}
	}
	if button == nil {
		t.Fatalf("被遮挡 frame 内按钮未进入快照: %+v", snapshot.Elements)
	}
	if _, err := manager.Click(ctx, ClickRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: button.Ref, Revision: snapshot.Revision}); err == nil {
		t.Fatal("被遮挡的 frame 点击应被拒绝")
	}
	after, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	if after.Title == "COVER_HIT" {
		t.Fatal("点击穿透到了覆盖层")
	}
}

func TestRealBrowserOOPIFIgnoresMainWorldTampering(t *testing.T) {
	listener, err := listenCrossSite("127.0.0.2:0")
	if err != nil {
		t.Skipf("127.0.0.2 不可用: %v", err)
	}
	inner := &httptest.Server{Listener: listener, Config: &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Tamper</title><body style="margin:0">
<button style="position:absolute;left:10px;top:10px;width:120px;height:30px">TAMPER_BTN</button>
<script>Document.prototype.querySelectorAll = function(){ return []; };</script>
</body>`))
	})}}
	inner.Start()
	defer inner.Close()
	outer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><body style="margin:0">
<iframe src="` + inner.URL + `" style="position:absolute;left:80px;top:80px;width:400px;height:240px;border:0"></iframe>
</body>`))
	}))
	defer outer.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: outer.URL, Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for attempt := 0; attempt < 30 && !found; attempt++ {
		if attempt > 0 {
			time.Sleep(100 * time.Millisecond)
		}
		snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
		if err != nil {
			t.Fatal(err)
		}
		for _, element := range snapshot.Elements {
			if element.Name == "TAMPER_BTN" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("主 world 篡改影响了快照采集")
	}
}

func TestRealBrowserCrossSiteOOPIF(t *testing.T) {
	listener, err := listenCrossSite("127.0.0.2:0")
	if err != nil {
		t.Skipf("127.0.0.2 不可用: %v", err)
	}
	inner := &httptest.Server{Listener: listener, Config: &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>OOPIF</title><body style="margin:0">
<button id="b" style="position:absolute;left:10px;top:10px;width:120px;height:30px" onclick="document.getElementById('out').textContent='OOPIF_CLICKED'">OOPIF_BTN</button>
<input id="i" aria-label="OOPIFInput" style="position:absolute;left:10px;top:60px;width:180px" value="oopif-old">
<div id="out" style="position:absolute;left:10px;top:100px">OOPIF_READY</div>
</body>`))
	})}}
	inner.Start()
	defer inner.Close()
	outer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>OOPIF Host</title><body style="margin:0">
<iframe src="` + inner.URL + `" style="position:absolute;left:100px;top:80px;width:400px;height:240px;border:0"></iframe>
</body>`))
	}))
	defer outer.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true, AllowEvaluate: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: outer.URL, Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	var snapshot SnapshotResponse
	var button, input *Element
	for attempt := 0; attempt < 30 && (button == nil || input == nil); attempt++ {
		if attempt > 0 {
			time.Sleep(100 * time.Millisecond)
		}
		snapshot, err = manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
		if err != nil {
			t.Fatal(err)
		}
		for index := range snapshot.Elements {
			switch snapshot.Elements[index].Name {
			case "OOPIF_BTN":
				button = &snapshot.Elements[index]
			case "OOPIFInput":
				input = &snapshot.Elements[index]
			}
		}
	}
	if button == nil || input == nil || !strings.HasPrefix(button.Ref, "f") {
		t.Fatalf("OOPIF 元素未进入快照: %+v", snapshot.Elements)
	}
	if !strings.Contains(snapshot.Text, "OOPIF_READY") {
		t.Fatalf("OOPIF 正文未聚合: %q", snapshot.Text)
	}
	if _, err := manager.Click(ctx, ClickRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: button.Ref, Revision: snapshot.Revision}); err != nil {
		t.Fatalf("OOPIF 点击失败: %v", err)
	}
	if _, err := manager.Type(ctx, TypeRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: input.Ref, Revision: snapshot.Revision, Text: "oopif-new"}); err != nil {
		t.Fatalf("OOPIF 输入失败: %v", err)
	}
	updated, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	value := ""
	for _, element := range updated.Elements {
		if element.Name == "OOPIFInput" {
			value = element.Value
		}
	}
	if !strings.Contains(updated.Text, "OOPIF_CLICKED") || value != "oopif-new" {
		t.Fatalf("OOPIF 操作未生效: text=%q value=%q", updated.Text, value)
	}
	// 宿主 iframe 隐藏后，缓存的 OOPIF 不应继续进入快照。
	if _, err := manager.Evaluate(ctx, EvaluateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Code: "document.querySelector('iframe').style.visibility='hidden'"}); err != nil {
		t.Fatal(err)
	}
	hidden, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	for _, element := range hidden.Elements {
		if strings.HasPrefix(element.Ref, "f") {
			t.Fatalf("隐藏宿主下的 OOPIF 元素仍进入快照: %+v", element)
		}
	}
	if strings.Contains(hidden.Text, "OOPIF_") {
		t.Fatalf("隐藏宿主下的 OOPIF 正文仍进入快照: %q", hidden.Text)
	}
}

func TestRealBrowserSameOriginHiddenHostExcluded(t *testing.T) {
	var base string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/inner/") {
			marker := strings.TrimPrefix(request.URL.Path, "/inner/")
			_, _ = writer.Write([]byte(`<!doctype html><body style="margin:0">
<div>` + marker + `_TEXT</div><input aria-label="` + marker + `_INPUT" value="` + marker + `_VALUE">
</body>`))
			return
		}
		frames := ""
		for _, marker := range []string{"FADE", "CLIP", "CONTENT", "RIGHT", "LEFT", "BOTTOM", "VISIBLE"} {
			frames += `<div class="host-` + marker + `"><iframe src="/inner/` + marker + `" style="width:320px;height:120px;border:0"></iframe></div>`
		}
		_, _ = writer.Write([]byte(`<!doctype html><title>Same Origin Hosts</title>
<style>
.host-FADE { opacity: 0; }
.host-CLIP { clip-path: inset(0px 100%); }
.host-CONTENT { content-visibility: hidden; }
.host-RIGHT { position: absolute; left: 100000px; top: 0; }
.host-LEFT { position: absolute; left: -10000px; top: 0; }
.host-BOTTOM { position: absolute; left: 0; top: 100000px; }
.host-VISIBLE { visibility: hidden; }
.host-VISIBLE iframe { visibility: visible; }
</style>
<body style="margin:0">` + frames + `</body>`))
	}))
	defer server.Close()
	base = server.URL
	manager, err := NewManager(Config{AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: base + "/", Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	var snapshot SnapshotResponse
	for attempt := 0; attempt < 20; attempt++ {
		if attempt > 0 {
			time.Sleep(100 * time.Millisecond)
		}
		snapshot, err = manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(snapshot.Text, "VISIBLE_TEXT") {
			break
		}
	}
	if !strings.Contains(snapshot.Text, "VISIBLE_TEXT") {
		t.Fatalf("visibility:visible 覆盖的同源 iframe 不应被排除: %q", snapshot.Text)
	}
	for _, marker := range []string{"FADE", "CLIP", "CONTENT", "RIGHT", "LEFT", "BOTTOM"} {
		if strings.Contains(snapshot.Text, marker+"_TEXT") {
			t.Fatalf("同源隐藏宿主 %s 的正文进入快照: %q", marker, snapshot.Text)
		}
		for _, element := range snapshot.Elements {
			if strings.Contains(element.Name, marker+"_INPUT") && element.Value != "" {
				t.Fatalf("同源隐藏宿主 %s 的值进入快照: %+v", marker, element)
			}
		}
	}
}

func TestRealBrowserOOPIFSurvivesAdFrames(t *testing.T) {
	var ads []*httptest.Server
	for _, host := range []string{"127.0.0.2", "127.0.0.3", "127.0.0.4", "127.0.0.5", "127.0.0.7", "127.0.0.8", "127.0.0.9", "127.0.0.10"} {
		listener, err := listenCrossSite(host + ":0")
		if err != nil {
			t.Skipf("%s 不可用: %v", host, err)
		}
		server := &httptest.Server{Listener: listener, Config: &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write([]byte(`<!doctype html><body style="margin:0"><div>AD_SLOT</div></body>`))
		})}}
		server.Start()
		ads = append(ads, server)
	}
	defer func() {
		for _, server := range ads {
			server.Close()
		}
	}()
	targetListener, err := listenCrossSite("127.0.0.6:0")
	if err != nil {
		t.Skipf("127.0.0.6 不可用: %v", err)
	}
	target := &httptest.Server{Listener: targetListener, Config: &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><body style="margin:0">
<button style="position:absolute;left:10px;top:10px;width:120px;height:30px" onclick="this.textContent='TARGET_CLICKED'">TARGET_BTN</button>
</body>`))
	})}}
	target.Start()
	defer target.Close()
	var frames strings.Builder
	for index, server := range ads {
		if index < 4 {
			frames.WriteString(`<iframe src="` + server.URL + `" style="position:absolute;left:` + strconv.Itoa(index*10) + `px;top:0;width:80px;height:40px;border:0"></iframe>`)
			continue
		}
		// 隐藏的大面积广告：不应占用 attach 预算。
		frames.WriteString(`<div style="visibility:hidden"><iframe src="` + server.URL + `" style="position:absolute;left:` + strconv.Itoa(index*10) + `px;top:0;width:1000px;height:1000px;border:0"></iframe></div>`)
	}
	frames.WriteString(`<iframe src="` + target.URL + `" style="position:absolute;left:100px;top:80px;width:400px;height:240px;border:0"></iframe>`)
	outer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Ads First</title><body style="margin:0">` + frames.String() + `</body>`))
	}))
	defer outer.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: outer.URL, Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	var snapshot SnapshotResponse
	var button *Element
	for attempt := 0; attempt < 20 && button == nil; attempt++ {
		if attempt > 0 {
			time.Sleep(100 * time.Millisecond)
		}
		snapshot, err = manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
		if err != nil {
			t.Fatal(err)
		}
		for index := range snapshot.Elements {
			if snapshot.Elements[index].Name == "TARGET_BTN" {
				button = &snapshot.Elements[index]
			}
		}
	}
	if button == nil {
		t.Fatalf("广告 frame 排在前面时目标 OOPIF 未进入快照: %+v", snapshot.Elements)
	}
	if _, err := manager.Click(ctx, ClickRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: button.Ref, Revision: snapshot.Revision}); err != nil {
		t.Fatal(err)
	}
	after, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(after.Text, "TARGET_CLICKED") {
		t.Fatalf("目标 OOPIF 点击未生效: %q", after.Text)
	}
}

func TestRealBrowserOOPIFHiddenHostExcluded(t *testing.T) {
	listener, err := listenCrossSite("127.0.0.2:0")
	if err != nil {
		t.Skipf("127.0.0.2 不可用: %v", err)
	}
	inner := &httptest.Server{Listener: listener, Config: &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		marker := strings.TrimPrefix(request.URL.Path, "/")
		_, _ = writer.Write([]byte(`<!doctype html><body style="margin:0">
<div>` + marker + `_TEXT</div>
<input aria-label="` + marker + `_INPUT" value="` + marker + `_VALUE">
</body>`))
	})}}
	inner.Start()
	defer inner.Close()
	outer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Hidden Hosts</title><body style="margin:0">
<div style="opacity:0"><iframe src="` + inner.URL + `/FADE" style="width:320px;height:120px;border:0"></iframe></div>
<div style="clip-path:inset(100%)"><iframe src="` + inner.URL + `/CLIP" style="width:320px;height:120px;border:0"></iframe></div>
<div style="clip-path:inset(0px 100%)"><iframe src="` + inner.URL + `/CLIP2" style="width:320px;height:120px;border:0"></iframe></div>
<div style="clip-path:inset(0px 0px 100%)"><iframe src="` + inner.URL + `/CLIP3" style="width:320px;height:120px;border:0"></iframe></div>
<div style="clip-path:inset(50% 0px)"><iframe src="` + inner.URL + `/CLIP4" style="width:320px;height:120px;border:0"></iframe></div>
<div style="content-visibility:hidden"><iframe src="` + inner.URL + `/CONTENT" style="width:320px;height:120px;border:0"></iframe></div>
<iframe src="` + inner.URL + `/RIGHT" style="position:absolute;left:100000px;top:20px;width:320px;height:120px;border:0"></iframe>
<div style="visibility:hidden"><iframe src="` + inner.URL + `/VISIBLE" style="width:320px;height:120px;border:0;visibility:visible"></iframe></div>
</body>`))
	}))
	defer outer.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: outer.URL, Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	var snapshot SnapshotResponse
	for attempt := 0; attempt < 20; attempt++ {
		if attempt > 0 {
			time.Sleep(100 * time.Millisecond)
		}
		snapshot, err = manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(snapshot.Text, "FADE_TEXT") || strings.Contains(snapshot.Text, "VISIBLE_TEXT") {
			break
		}
	}
	if !strings.Contains(snapshot.Text, "VISIBLE_TEXT") {
		t.Fatalf("子元素 visibility:visible 的 iframe 不应被排除: %q", snapshot.Text)
	}
	for _, marker := range []string{"FADE", "CLIP", "CLIP2", "CLIP3", "CLIP4", "CONTENT", "RIGHT"} {
		if strings.Contains(snapshot.Text, marker+"_TEXT") {
			t.Fatalf("隐藏宿主 %s 的正文进入快照: %q", marker, snapshot.Text)
		}
		for _, element := range snapshot.Elements {
			if strings.Contains(element.Name, marker+"_INPUT") {
				t.Fatalf("隐藏宿主 %s 的输入框进入快照: %+v", marker, element)
			}
		}
	}
}

func TestRealBrowserCrossFrameTransformVariantsRejected(t *testing.T) {
	transforms := []string{
		"scaleX(0.5)",
		"scaleY(0.5)",
		"rotate(180deg)",
		"scaleX(-1)",
		"skewX(20deg)",
	}
	for _, transform := range transforms {
		t.Run(transform, func(t *testing.T) {
			inner := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = writer.Write([]byte(`<!doctype html><body style="margin:0">
<button style="position:absolute;left:10px;top:10px;width:100px;height:30px">VARIANT_BTN</button>
</body>`))
			}))
			defer inner.Close()
			outer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = writer.Write([]byte(`<!doctype html><body style="margin:0">
<iframe src="` + inner.URL + `" style="position:absolute;left:80px;top:80px;width:400px;height:240px;border:0;transform:` + transform + `;transform-origin:0 0"></iframe>
</body>`))
			}))
			defer outer.Close()
			manager, err := NewManager(Config{AllowPrivateNetwork: true})
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Close(context.Background())
			ctx := context.Background()
			opened, err := manager.Open(ctx, OpenRequest{URL: outer.URL, Viewport: Viewport{Width: 960, Height: 640}})
			if err != nil {
				t.Fatal(err)
			}
			var snapshot SnapshotResponse
			var button *Element
			for attempt := 0; attempt < 20 && button == nil; attempt++ {
				if attempt > 0 {
					time.Sleep(100 * time.Millisecond)
				}
				snapshot, err = manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
				if err != nil {
					t.Fatal(err)
				}
				for index := range snapshot.Elements {
					if snapshot.Elements[index].Name == "VARIANT_BTN" {
						button = &snapshot.Elements[index]
					}
				}
			}
			if button == nil {
				t.Fatalf("变形 frame 内按钮未进入快照: %+v", snapshot.Elements)
			}
			if _, err := manager.Click(ctx, ClickRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: button.Ref, Revision: snapshot.Revision}); err == nil || !strings.Contains(err.Error(), "变形") {
				t.Fatalf("变形 frame 应返回明确错误，得到 %v", err)
			}
		})
	}
}

func TestRealBrowserCrossFrameTransformRejected(t *testing.T) {
	inner := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><body style="margin:0">
<button style="position:absolute;left:10px;top:10px;width:100px;height:30px" onclick="document.title='CLICKED'">SCALED_BTN</button>
</body>`))
	}))
	defer inner.Close()
	outer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><body style="margin:0">
<iframe src="` + inner.URL + `" style="position:absolute;left:80px;top:80px;width:400px;height:240px;border:0;transform:scale(0.5);transform-origin:0 0"></iframe>
</body>`))
	}))
	defer outer.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: outer.URL, Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	var snapshot SnapshotResponse
	var button *Element
	for attempt := 0; attempt < 20 && button == nil; attempt++ {
		if attempt > 0 {
			time.Sleep(100 * time.Millisecond)
		}
		snapshot, err = manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
		if err != nil {
			t.Fatal(err)
		}
		for index := range snapshot.Elements {
			if snapshot.Elements[index].Name == "SCALED_BTN" {
				button = &snapshot.Elements[index]
			}
		}
	}
	if button == nil {
		t.Fatalf("缩放 frame 内按钮未进入快照: %+v", snapshot.Elements)
	}
	_, err = manager.Click(ctx, ClickRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: button.Ref, Revision: snapshot.Revision})
	if err == nil || !strings.Contains(err.Error(), "缩放") {
		t.Fatalf("缩放 frame 应返回明确错误，得到 %v", err)
	}
}

func TestRealBrowserCrossFrameNavigationInvalidatesRefs(t *testing.T) {
	inner := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		title := "A"
		next := "/b"
		if request.URL.Path == "/b" {
			title = "B"
			next = "/a"
		}
		_, _ = writer.Write([]byte(`<!doctype html><title>` + title + `</title><body style="margin:0">
<button aria-label="Navigate" style="position:absolute;left:10px;top:10px;width:120px;height:30px" onclick="location.href='` + next + `'">NEXT</button>
</body>`))
	}))
	defer inner.Close()
	outer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><body style="margin:0">
<iframe src="` + inner.URL + `/a" style="position:absolute;left:80px;top:60px;width:400px;height:240px;border:0"></iframe>
</body>`))
	}))
	defer outer.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: outer.URL, Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	var snapshot SnapshotResponse
	var first *Element
	for attempt := 0; attempt < 20 && first == nil; attempt++ {
		if attempt > 0 {
			time.Sleep(100 * time.Millisecond)
		}
		snapshot, err = manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
		if err != nil {
			t.Fatal(err)
		}
		for index := range snapshot.Elements {
			if snapshot.Elements[index].Name == "Navigate" {
				first = &snapshot.Elements[index]
			}
		}
	}
	if first == nil {
		t.Fatalf("frame 按钮未进入快照: %+v", snapshot.Elements)
	}
	if _, err := manager.Click(ctx, ClickRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: first.Ref, Revision: snapshot.Revision}); err != nil {
		t.Fatalf("frame 内导航点击失败: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	updated, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	var second *Element
	for index := range updated.Elements {
		if updated.Elements[index].Name == "Navigate" {
			second = &updated.Elements[index]
		}
	}
	if second == nil {
		t.Fatalf("导航后 frame 按钮未进入快照: %+v", updated.Elements)
	}
	if second.Ref == first.Ref {
		t.Fatalf("frame 导航后 ref 不应复用: %s", second.Ref)
	}
	if _, err := manager.Click(ctx, ClickRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: first.Ref, Revision: updated.Revision}); err == nil {
		t.Fatal("frame 导航后的旧 ref 应失效")
	}
}

func TestRealBrowserCrossFrameElementsAndActions(t *testing.T) {
	inner := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/zero-frame" {
			_, _ = writer.Write([]byte(`<!doctype html><body><button aria-label="ZeroFrameBtn">ZERO</button></body>`))
			return
		}
		_, _ = writer.Write([]byte(`<!doctype html><title>Inner</title><body style="margin:0">
<button id="b" style="position:absolute;left:20px;top:20px;width:120px;height:40px" onclick="document.getElementById('out').textContent='CLICKED'">CROSS_BTN</button>
<input id="i" aria-label="CrossInput" style="position:absolute;left:20px;top:80px;width:200px;height:30px" value="old">
<div id="out" style="position:absolute;left:20px;top:130px">READY</div>
<select id="s" aria-label="CrossSelect" style="position:absolute;left:20px;top:170px"><option value="one">One</option><option value="two">Two</option></select>
<input id="c" aria-label="CrossCheck" type="checkbox" style="position:absolute;left:20px;top:210px">
<input id="k" aria-label="CrossKey" style="position:absolute;left:20px;top:250px" onkeydown="if(event.key==='Enter')document.getElementById('out').textContent='KEY_ENTER'">
<div id="h" role="button" aria-label="CrossHover" style="position:absolute;left:20px;top:290px;width:120px;height:30px" onmouseover="document.getElementById('out').textContent='HOVERED'">HOVER_ME</div>
<input aria-label="CrossPassword" type="password" value="secret-password" style="position:absolute;left:200px;top:20px">
<input aria-label="CrossMasked" autocomplete="one-time-code" value="secret-otp" style="position:absolute;left:200px;top:60px">
<div style="position:absolute;left:200px;top:100px;opacity:0"><input aria-label="CrossHidden" value="secret-hidden"></div>
<div id="src" role="button" aria-label="CrossDragSource" style="position:absolute;left:20px;top:330px;width:60px;height:30px" onmousedown="window.__cdrag=1">SRC</div>
<div id="dst" role="button" aria-label="CrossDragTarget" style="position:absolute;left:180px;top:330px;width:60px;height:30px" onmouseup="if(window.__cdrag)document.getElementById('out').textContent='DRAGGED'">DST</div>
<input id="f" type="file" aria-label="CrossFile" style="position:absolute;left:20px;top:370px" onchange="document.getElementById('out').textContent='FILE:'+this.files[0].name">
<div contenteditable aria-label="CrossEditor" style="position:absolute;left:200px;top:370px;width:180px;height:30px">edit-me</div>
<iframe src="/zero-frame" style="width:0;height:0;border:0"></iframe>
<form onsubmit="document.getElementById('out').textContent='FORM_SUBMITTED';return false"><input name="q" aria-label="CrossSubmitInput" style="position:absolute;left:200px;top:410px;width:120px"><button type="submit" style="position:absolute;left:330px;top:410px">Go</button></form>
</body>`))
	}))
	defer inner.Close()
	outer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Outer</title><body style="margin:0">
<iframe src="` + inner.URL + `" style="position:absolute;left:100px;top:50px;width:400px;height:300px;border:0"></iframe>
</body>`))
	}))
	defer outer.Close()
	uploadDir := t.TempDir()
	uploadPath := filepath.Join(uploadDir, "upload.txt")
	if err := os.WriteFile(uploadPath, []byte("upload-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(Config{AllowPrivateNetwork: true, UploadDir: uploadDir})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: outer.URL, Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	var button, input, selectBox, checkBox *Element
	for index := range snapshot.Elements {
		element := snapshot.Elements[index]
		switch element.Name {
		case "CROSS_BTN":
			button = &snapshot.Elements[index]
		case "CrossInput":
			input = &snapshot.Elements[index]
		case "CrossSelect":
			selectBox = &snapshot.Elements[index]
		case "CrossCheck":
			checkBox = &snapshot.Elements[index]
		}
	}
	if selectBox == nil || checkBox == nil {
		t.Fatalf("跨域下拉框/复选框未进入快照: %+v", snapshot.Elements)
	}
	if button == nil || input == nil || !strings.HasPrefix(button.Ref, "f") || !strings.HasPrefix(input.Ref, "f") {
		t.Fatalf("跨域 frame 元素未进入快照: %+v", snapshot.Elements)
	}
	if input.Value != "old" {
		t.Fatalf("跨域输入框初始值不正确: %+v", input)
	}
	if !strings.Contains(input.FrameURL, "127.0.0.1") {
		t.Fatalf("跨域元素缺少 frame_url: %+v", input)
	}
	for _, element := range snapshot.Elements {
		switch element.Name {
		case "CrossPassword":
			t.Fatalf("跨域密码框不应进入快照: %+v", element)
		case "CrossMasked":
			if element.Value != "" {
				t.Fatalf("跨域 OTP 字段不应回显值: %+v", element)
			}
		case "CrossHidden":
			t.Fatalf("跨域隐藏字段不应进入快照: %+v", element)
		}
	}
	if _, err := manager.Click(ctx, ClickRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: button.Ref, Revision: snapshot.Revision}); err != nil {
		t.Fatalf("跨域点击失败: %v", err)
	}
	if _, err := manager.Type(ctx, TypeRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: input.Ref, Revision: snapshot.Revision, Text: "new"}); err != nil {
		t.Fatalf("跨域输入失败: %v", err)
	}
	updated, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(updated.Text, "CLICKED") {
		t.Fatalf("跨域点击没有生效: %q", updated.Text)
	}
	value := ""
	for _, element := range updated.Elements {
		if element.Name == "CrossInput" {
			value = element.Value
		}
	}
	if value != "new" {
		t.Fatalf("跨域输入没有生效: %+v", updated.Elements)
	}
	if _, err := manager.SelectOption(ctx, SelectOptionRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: selectBox.Ref, Revision: updated.Revision, Label: "Two"}); err != nil {
		t.Fatalf("跨域下拉框选择失败: %v", err)
	}
	if _, err := manager.SetChecked(ctx, SetCheckedRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: checkBox.Ref, Revision: updated.Revision, Checked: true}); err != nil {
		t.Fatalf("跨域勾选失败: %v", err)
	}
	final, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	checkValue, checkState := "", false
	for _, element := range final.Elements {
		if element.Name == "CrossSelect" {
			checkValue = element.Value
		}
		if element.Name == "CrossCheck" {
			checkState = element.Checked != nil && *element.Checked
		}
	}
	if checkValue != "two" || !checkState {
		t.Fatalf("跨域下拉/勾选未生效: select=%q checked=%v", checkValue, checkState)
	}
	selector := `[data-browserkit-ref="` + button.Ref + `"]`
	if _, err := manager.Wait(ctx, WaitRequest{SessionID: opened.SessionID, PageID: opened.PageID, Condition: "selector_visible", RefOrSelector: selector, TimeoutMS: 2000}); err != nil {
		t.Fatalf("跨域 ref 等待失败: %v", err)
	}
	var hoverTarget *Element
	for index := range final.Elements {
		if final.Elements[index].Name == "CrossHover" {
			hoverTarget = &final.Elements[index]
		}
	}
	if hoverTarget == nil {
		t.Fatalf("跨域 hover 目标未进入快照: %+v", final.Elements)
	}
	if _, err := manager.Hover(ctx, HoverRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: hoverTarget.Ref, Revision: final.Revision}); err != nil {
		t.Fatalf("跨域 hover 失败: %v", err)
	}
	hovered, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hovered.Text, "HOVERED") {
		t.Fatalf("跨域 hover 没有生效: %q", hovered.Text)
	}
	var keyInput *Element
	for index := range final.Elements {
		if final.Elements[index].Name == "CrossKey" {
			keyInput = &final.Elements[index]
		}
	}
	if keyInput == nil {
		t.Fatalf("跨域按键输入框未进入快照: %+v", final.Elements)
	}
	if _, err := manager.Press(ctx, PressRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: keyInput.Ref, Revision: final.Revision, Key: "Enter"}); err != nil {
		t.Fatalf("跨域按键失败: %v", err)
	}
	afterKey, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(afterKey.Text, "KEY_ENTER") {
		t.Fatalf("跨域按键没有生效: %q", afterKey.Text)
	}
	var dragSource, dragTarget *Element
	for index := range afterKey.Elements {
		switch afterKey.Elements[index].Name {
		case "CrossDragSource":
			dragSource = &afterKey.Elements[index]
		case "CrossDragTarget":
			dragTarget = &afterKey.Elements[index]
		}
	}
	if dragSource == nil || dragTarget == nil {
		t.Fatalf("跨域拖拽元素未进入快照: %+v", afterKey.Elements)
	}
	if _, err := manager.Drag(ctx, DragRequest{SessionID: opened.SessionID, PageID: opened.PageID, SourceRef: dragSource.Ref, TargetRef: dragTarget.Ref, Revision: afterKey.Revision}); err != nil {
		t.Fatalf("跨域拖拽失败: %v", err)
	}
	dragged, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dragged.Text, "DRAGGED") {
		t.Fatalf("跨域拖拽没有生效: %q", dragged.Text)
	}
	var fileInput *Element
	for index := range dragged.Elements {
		if dragged.Elements[index].Name == "CrossFile" {
			fileInput = &dragged.Elements[index]
		}
	}
	if fileInput == nil {
		t.Fatalf("跨域 file input 未进入快照: %+v", dragged.Elements)
	}
	if _, err := manager.UploadFile(ctx, UploadFileRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: fileInput.Ref, Revision: dragged.Revision, Path: uploadPath}); err != nil {
		t.Fatalf("跨域上传失败: %v", err)
	}
	uploaded, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(uploaded.Text, "FILE:upload.txt") {
		t.Fatalf("跨域上传没有生效: %q", uploaded.Text)
	}
	var editor *Element
	for index := range uploaded.Elements {
		if uploaded.Elements[index].Name == "CrossEditor" {
			editor = &uploaded.Elements[index]
		}
	}
	if editor == nil {
		t.Fatalf("跨域 contenteditable 未进入快照: %+v", uploaded.Elements)
	}
	if editor.Role != "textbox" || !editor.ContentEditable || !editor.Multiline {
		t.Fatalf("跨域 contenteditable 语义不正确: %+v", editor)
	}
	for _, element := range uploaded.Elements {
		if element.Name == "ZeroFrameBtn" || element.Name == "ZERO" {
			t.Fatalf("零尺寸 iframe 内的元素不应进入快照: %+v", element)
		}
	}
	if _, err := manager.Type(ctx, TypeRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: editor.Ref, Revision: uploaded.Revision, Text: "edited"}); err != nil {
		t.Fatalf("跨域 contenteditable 输入失败: %v", err)
	}
	edited, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	for _, element := range edited.Elements {
		if element.Name == "CrossEditor" && element.Value != "edited" {
			t.Fatalf("跨域 contenteditable 值未更新: %+v", element)
		}
	}
	var submitInput *Element
	for index := range edited.Elements {
		if edited.Elements[index].Name == "CrossSubmitInput" {
			submitInput = &edited.Elements[index]
		}
	}
	if submitInput == nil {
		t.Fatalf("跨域 submit 输入框未进入快照: %+v", edited.Elements)
	}
	if _, err := manager.Type(ctx, TypeRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: submitInput.Ref, Revision: edited.Revision, Text: "submit-me", Submit: true}); err != nil {
		t.Fatalf("跨域 submit 输入失败: %v", err)
	}
	submitted, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(submitted.Text, "FORM_SUBMITTED") {
		t.Fatalf("跨域 submit 没有生效: %q", submitted.Text)
	}
}

func TestRealBrowserSnapshotCrossFrameText(t *testing.T) {
	inner := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/same-origin-inner" {
			_, _ = writer.Write([]byte(`<!doctype html><title>SameOriginInner</title><body><div>SAME_ORIGIN_INNER_TEXT</div></body>`))
			return
		}
		_, _ = writer.Write([]byte(`<!doctype html><title>Inner</title><body>
<div id="inner-title">CROSS_INNER_TEXT</div>
<input aria-label="InnerSecret" value="inner-secret">
<iframe src="/same-origin-inner" width="200" height="80"></iframe>
</body>`))
	}))
	defer inner.Close()
	outer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Outer</title><body>
<div>OUTER_TEXT</div><iframe src="` + inner.URL + `" width="400" height="200"></iframe>
</body>`))
	}))
	defer outer.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: outer.URL, Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(snapshot.Text, "CROSS_INNER_TEXT") || !strings.Contains(snapshot.Text, "OUTER_TEXT") {
		t.Fatalf("跨域 frame 正文未聚合: %q", snapshot.Text)
	}
	if !strings.Contains(snapshot.Text, "SAME_ORIGIN_INNER_TEXT") {
		t.Fatalf("跨域 frame 内的同源子 frame 正文未聚合: %q", snapshot.Text)
	}
	if strings.Contains(snapshot.Text, "inner-secret") {
		t.Fatalf("跨域 frame 输入值不应进入正文: %q", snapshot.Text)
	}
	again, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(again.Text, "CROSS_INNER_TEXT") {
		t.Fatalf("复用 isolated world 后正文丢失: %q", again.Text)
	}
}

func TestRealBrowserSnapshotContentEditableVariants(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Editable</title><body>
<div id="bare" contenteditable aria-label="BareEditor">bare text</div>
<div id="plain" contenteditable="plaintext-only" aria-label="PlainEditor">plain text</div>
<div id="empty" contenteditable="" aria-label="EmptyEditor"></div>
<div id="single" contenteditable aria-label="SingleEditor" aria-multiline="false">single text</div>
<div id="off" contenteditable="false" aria-label="OffEditor">off text</div>
<div id="inherit" contenteditable="inherit" aria-label="InheritEditor">inherit text</div>
</body>`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: server.URL, Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	elements := map[string]Element{}
	for _, element := range snapshot.Elements {
		elements[element.ID] = element
	}
	if elements["bare"].Value != "bare text" || elements["plain"].Value != "plain text" {
		t.Fatalf("contenteditable 变体未返回当前值: %+v", elements)
	}
	for _, id := range []string{"bare", "plain", "empty"} {
		element, ok := elements[id]
		if !ok {
			t.Fatalf("空的 contenteditable 应出现在快照: %+v", elements)
		}
		if element.Role != "textbox" || !element.ContentEditable || !element.Multiline {
			t.Fatalf("contenteditable %s 语义不正确: %+v", id, element)
		}
	}
	if single := elements["single"]; single.Role != "textbox" || !single.ContentEditable || single.Multiline || single.Value != "single text" {
		t.Fatalf("显式单行 contenteditable 语义不正确: %+v", single)
	}
	if _, ok := elements["off"]; ok {
		t.Fatalf("contenteditable=false 不应作为编辑区: %+v", elements)
	}
	if _, ok := elements["inherit"]; ok {
		t.Fatalf("不可编辑的 contenteditable=inherit 不应进入快照: %+v", elements)
	}
}

func TestRealBrowserSnapshotPointerFallback(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Pointer</title><body>
<div id="card" style="cursor:pointer">Open card</div>
<div id="plain">Plain text</div>
<div aria-hidden="true"><div id="hidden-card" style="cursor:pointer">Hidden card</div></div>
</body>`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: server.URL, Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, element := range snapshot.Elements {
		if element.ID == "card" {
			found = true
			if element.Role != "button" || !strings.Contains(element.Name, "Open card") {
				t.Fatalf("cursor:pointer 元素未按按钮暴露: %+v", element)
			}
		}
		if element.ID == "hidden-card" {
			t.Fatalf("aria-hidden 祖先下的元素不应进入快照: %+v", element)
		}
	}
	if !found {
		t.Fatalf("cursor:pointer 的可点击 div 未被发现: %+v", snapshot.Elements)
	}
}

func TestRealBrowserSnapshotTextTruncatesEarly(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, _ *http.Request) {
		var body strings.Builder
		body.WriteString("<!doctype html><title>Text budget</title><body>")
		for index := 0; index < 5000; index++ {
			body.WriteString("<div>item-")
			body.WriteString(strconv.Itoa(index))
			body.WriteString("</div>")
		}
		body.WriteString("</body>")
		_, _ = writer.Write([]byte(body.String()))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: server.URL, Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	limited, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID, MaxChars: 500})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("截断快照耗时过长: %v", elapsed)
	}
	if len([]rune(limited.Text)) > 500 || !limited.TextTruncated {
		t.Fatalf("正文预算未生效: len=%d truncated=%v", len([]rune(limited.Text)), limited.TextTruncated)
	}
	full, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID, MaxChars: 1000000})
	if err != nil {
		t.Fatal(err)
	}
	if full.TextTruncated || !strings.Contains(full.Text, "item-4999") {
		t.Fatalf("完整快照不应提前停止: truncated=%v", full.TextTruncated)
	}
}

func TestRealBrowserSnapshotValueAfterScroll(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Scroll values</title><input aria-label="TopField" value="top-value"><div style="height:12000px"></div><input aria-label="BottomField" value="bottom-value">`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true, AllowEvaluate: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: server.URL, Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Evaluate(ctx, EvaluateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Code: "window.scrollTo(0, 12000)"}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, element := range snapshot.Elements {
		values[element.Name] = element.Value
	}
	if values["TopField"] != "top-value" || values["BottomField"] != "bottom-value" {
		t.Fatalf("滚动后字段值=%v", values)
	}
}

func TestRealBrowserActClickWaitAfterTimeoutWarning(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Slow click</title><button id="go" onclick="setTimeout(() => location.href='/done', 600)">GO</button>`))
	})
	mux.HandleFunc("/done", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Done</title>DONE`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: server.URL, Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	ref := smokeElementRef(t, snapshot, "button", "GO")
	response, err := manager.Act(ctx, ActRequest{
		SessionID: opened.SessionID,
		PageID:    opened.PageID,
		Revision:  snapshot.Revision,
		Steps: []ActStep{
			{Op: "click", Ref: ref, WaitAfterMS: 3000, TimeoutMS: 500},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.FailedStep != 0 || len(response.Steps) != 1 || !response.Steps[0].Success {
		t.Fatalf("点击已执行，不应标记步骤失败: %+v", response)
	}
	if response.Steps[0].Warning == "" {
		t.Fatalf("wait_after_ms 超时没有报告 warning: %+v", response.Steps[0])
	}
}

func TestRealBrowserSnapshotFormValues(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Form values</title>
<input id="name" aria-label="Name" type="text" value="  a   b  ">
<input id="password" aria-label="Password" type="password" value="secret">
<input id="masked" aria-label="Masked" type="text" value="otp123" style="-webkit-text-security: disc">
<input id="autocomplete-password" aria-label="AutocompletePassword" type="text" autocomplete="current-password" value="autopass">
<textarea id="notes" aria-label="Notes">line1
line2</textarea>
<textarea id="masked-notes" aria-label="MaskedNotes" style="-webkit-text-security: disc">secret-textarea</textarea>
<input id="cloaked" aria-label="Cloaked" type="text" value="cloaked-secret" style="opacity:0.01">
<div id="outer-editor" aria-label="OuterEditor" contenteditable="true">outer<span id="inner-editor" role="button">inner</span></div>
<select id="color" aria-label="Color"><option value="red">Red</option><option value="blue" selected>Blue</option></select>
<input id="agree" aria-label="Agree" type="checkbox" checked>
<input id="blank" aria-label="Blank" type="text">
<div id="editor" aria-label="Editor" contenteditable="true">hello world</div>
<input id="send" type="submit" value="SendOnly">
<input id="hidden-token" type="hidden" value="token" style="display:block">
<input id="offscreen" aria-label="Offscreen" type="text" value="offscreen-token" style="position:absolute;left:-10000px;width:100px;height:20px">
<form autocomplete="current-password"><input id="form-password" aria-label="FormPassword" type="text" value="form-secret"></form>
<div style="opacity:0.01"><input id="parent-fade" aria-label="ParentFade" type="text" value="parent-fade-secret"></div>
<input id="clipped" aria-label="Clipped" type="text" value="clipped-secret" style="position:absolute;width:1px;height:1px;clip:rect(0 0 0 0)">
<input id="cc-number" aria-label="CCNumber" type="text" autocomplete="cc-number" value="4111111111111111">
<iframe id="offscreen-frame" style="position:absolute;left:-10000px;width:100px;height:50px" srcdoc="&lt;input aria-label=&quot;IframeSecret&quot; value=&quot;iframe-secret&quot;&gt;"></iframe>
<iframe id="faded-frame" style="opacity:0.01;width:100px;height:50px" srcdoc="&lt;input aria-label=&quot;FadedFrameSecret&quot; value=&quot;faded-frame-secret&quot;&gt;"></iframe>
<div id="faded-shadow-host" style="opacity:0.01"></div>
<select id="masked-select" aria-label="MaskedSelect" style="opacity:0.01"><option value="selected-secret" selected>Secret</option></select>
<select id="masked-many" aria-label="MaskedMany" style="opacity:0.01"></select>
<iframe style="visibility:hidden;width:100px;height:50px" srcdoc="&lt;div&gt;HIDDEN_IFRAME_TEXT&lt;/div&gt;"></iframe>
<div style="visibility:hidden"><iframe srcdoc="&lt;div&gt;HIDDEN_WRAPPED_IFRAME_TEXT&lt;/div&gt;"></iframe></div>
<div id="hidden-shadow-host" style="visibility:hidden"></div>
<div style="visibility:hidden"><span style="visibility:visible">VISIBLE_OVERRIDE_TEXT</span></div>
<div style="font-size:0"><span style="font-size:16px">FONT_VISIBLE</span></div>
<div style="visibility:hidden"><input aria-label="VisibleOverride" value="vis-value" style="visibility:visible"></div>
<div style="visibility:hidden"><input aria-label="HiddenValue" value="hidden-value"></div>
<div style="font-size:0"><input aria-label="FontVisible" value="font-visible" style="font-size:16px"></div>
<div style="-webkit-text-fill-color:transparent"><input aria-label="FillVisible" value="fill-visible" style="-webkit-text-fill-color:#000000"></div>
<div id="visible-shadow-host"></div>
<select id="visible-many" aria-label="VisibleMany"></select>
<script>document.querySelector('#hidden-shadow-host').attachShadow({mode:'open'}).textContent='HIDDEN_SHADOW_DIRECT_TEXT'; document.querySelector('#visible-shadow-host').attachShadow({mode:'open'}).textContent='VISIBLE_SHADOW_TEXT'; const many=document.querySelector('#masked-many'); for(let i=0;i<101;i++){many.add(new Option('Opt'+i,'v'+i));} const vmany=document.querySelector('#visible-many'); for(let i=0;i<101;i++){vmany.add(new Option('VOpt'+i,'vv'+i));}</script>
<iframe style="width:0;height:0" srcdoc="&lt;input aria-label=&quot;ZeroFrame&quot; value=&quot;zero-secret&quot;&gt;"></iframe>
<iframe style="width:100px;height:50px" srcdoc="&lt;div style=&quot;opacity:0.01&quot;&gt;&lt;input aria-label=&quot;HiddenAncestorInFrame&quot; value=&quot;inner-secret&quot;&gt;&lt;/div&gt;"></iframe>
<iframe style="position:absolute;left:100000px;width:100px;height:50px" srcdoc="&lt;input aria-label=&quot;RightFrame&quot; value=&quot;right-secret&quot;&gt;"></iframe>
<input id="clip-path-visible" aria-label="ClipPathVisible" type="text" value="clip-visible" style="clip-path: inset(0)">
<div style="opacity:0">HIDDEN_TEXT_SECRET</div>
<div>VISIBLE_TEXT_MARKER</div>
<div style="display:contents"><span>DISPLAY_CONTENTS_TEXT</span></div>
<script>document.querySelector('#faded-shadow-host').attachShadow({mode:'open'}).innerHTML='<input aria-label="ShadowSecret" value="shadow-secret">';</script>
<input id="long" aria-label="Long" type="text" value="` + strings.Repeat("a", 119) + `😀B">`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: server.URL, Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"Name": "  a   b  ", "Password": "", "Masked": "", "AutocompletePassword": "",
		"Notes": "line1\nline2", "Color": "blue", "Agree": "", "Blank": "",
		"Editor": "hello world", "SendOnly": "", "Offscreen": "", "MaskedNotes": "", "Cloaked": "",
		"OuterEditor": "outerinner", "FormPassword": "", "ParentFade": "", "Clipped": "", "CCNumber": "",
		"IframeSecret": "", "FadedFrameSecret": "", "ShadowSecret": "", "MaskedSelect": "",
		"ZeroFrame": "", "RightFrame": "", "ClipPathVisible": "clip-visible", "HiddenAncestorInFrame": "",
		"VisibleOverride": "vis-value", "FontVisible": "font-visible", "FillVisible": "fill-visible", "MaskedMany": "",
	}
	for _, element := range snapshot.Elements {
		expected, ok := want[element.Name]
		if !ok {
			continue
		}
		if element.Value != expected {
			t.Fatalf("%s 的 value=%q，期望 %q", element.Name, element.Value, expected)
		}
		delete(want, element.Name)
	}
	if len(want) != 0 {
		t.Fatalf("快照缺少元素: %v", want)
	}
	if !strings.Contains(snapshot.Text, "VISIBLE_TEXT_MARKER") || !strings.Contains(snapshot.Text, "DISPLAY_CONTENTS_TEXT") || strings.Contains(snapshot.Text, "HIDDEN_TEXT_SECRET") {
		t.Fatalf("隐藏文本/display:contents 正文处理不正确: %q", snapshot.Text)
	}
	if !strings.Contains(snapshot.Text, "hello world") || strings.Contains(snapshot.Text, "secret-textarea") {
		t.Fatalf("contenteditable/掩码 textarea 正文处理不正确: %q", snapshot.Text)
	}
	if !strings.Contains(snapshot.Text, "VISIBLE_OVERRIDE_TEXT") || !strings.Contains(snapshot.Text, "FONT_VISIBLE") ||
		!strings.Contains(snapshot.Text, "VISIBLE_SHADOW_TEXT") ||
		strings.Contains(snapshot.Text, "HIDDEN_IFRAME_TEXT") || strings.Contains(snapshot.Text, "HIDDEN_WRAPPED_IFRAME_TEXT") ||
		strings.Contains(snapshot.Text, "HIDDEN_SHADOW_DIRECT_TEXT") {
		t.Fatalf("iframe/shadow/可见性覆盖正文处理不正确: %q", snapshot.Text)
	}
	for _, element := range snapshot.Elements {
		if element.ID == "hidden-token" {
			t.Fatalf("隐藏 input 不应进入快照: %+v", element)
		}
		if (element.Name == "MaskedSelect" || element.Name == "MaskedMany") && (element.Value != "" || len(element.Options) != 0 || element.OptionsTruncated) {
			t.Fatalf("隐藏 select 不应输出 value/options: %+v", element)
		}
		if element.Name == "VisibleMany" && !element.OptionsTruncated {
			t.Fatalf("可见超大 select 应标记 options_truncated: %+v", element)
		}
		if element.Name == "HiddenValue" {
			t.Fatalf("隐藏祖先下的 input 不应进入快照: %+v", element)
		}
		if element.ID == "inner-editor" && element.Value != "" {
			t.Fatalf("嵌套 contenteditable 不应重复输出 value: %+v", element)
		}
		if element.ID == "long" {
			if !element.ValueTruncated {
				t.Fatalf("超长 value 没有标记截断: %+v", element)
			}
			if strings.ContainsRune(element.Value, '\uFFFD') {
				t.Fatalf("截断切开了代理对: %q", element.Value)
			}
			if got := len([]rune(element.Value)); got != 120 {
				t.Fatalf("截断应按码点保留 120 字符，实际 %d", got)
			}
		}
	}
	nameRef := smokeElementRef(t, snapshot, "textbox", "Name")
	typed, err := manager.Type(ctx, TypeRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: nameRef, Revision: snapshot.Revision, Text: "Bob"})
	if err != nil {
		t.Fatal(err)
	}
	if typed.Snapshot == nil {
		t.Fatal("输入后没有返回快照")
	}
	updated := ""
	for _, element := range typed.Snapshot.Elements {
		if element.Name == "Name" {
			updated = element.Value
		}
	}
	if updated != "Bob" {
		t.Fatalf("输入后快照 value=%q，期望 Bob", updated)
	}
}

func TestRealBrowserWaitRejectsRemovedRef(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Removed ref</title><div id="temp" role="button">TEMP</div>
<div id="host"></div><script>document.querySelector('#host').attachShadow({mode:'open'}).innerHTML='<button id="shadow">SHADOW</button>'</script>`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true, AllowEvaluate: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: server.URL, Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	ref := smokeElementRef(t, snapshot, "button", "TEMP")
	if _, err := manager.Evaluate(ctx, EvaluateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Code: `document.querySelector('#temp').remove()`}); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = manager.Wait(ctx, WaitRequest{SessionID: opened.SessionID, PageID: opened.PageID, Condition: "selector_visible", RefOrSelector: ref, Revision: snapshot.Revision, TimeoutMS: 5000})
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("失效 ref 不应等待超时，耗时 %s", elapsed)
	}
	var browserErr *Error
	if !errors.As(err, &browserErr) || browserErr.Kind != "stale_reference" {
		t.Fatalf("失效 ref 错误=%v，期望 stale_reference", err)
	}
	shadowRef := smokeElementRef(t, snapshot, "button", "SHADOW")
	if _, err := manager.Evaluate(ctx, EvaluateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Code: `document.querySelector('#host').shadowRoot.querySelector('#shadow').remove()`}); err != nil {
		t.Fatal(err)
	}
	_, err = manager.Wait(ctx, WaitRequest{SessionID: opened.SessionID, PageID: opened.PageID, Condition: "selector_visible", RefOrSelector: shadowRef, Revision: snapshot.Revision, TimeoutMS: 5000})
	if !errors.As(err, &browserErr) || browserErr.Kind != "stale_reference" {
		t.Fatalf("shadow 失效 ref 错误=%v，期望 stale_reference", err)
	}
}

func TestRealBrowserFullPageScreenshotRestoresScroll(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Tall</title><style>html{scroll-behavior:smooth}</style><div style="height:3000px">tall</div>`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true, AllowEvaluate: true, ScreenshotDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: server.URL, Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Evaluate(ctx, EvaluateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Code: `window.scrollTo(0, 500)`}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Screenshot(ctx, ScreenshotRequest{SessionID: opened.SessionID, PageID: opened.PageID, FullPage: true}); err != nil {
		t.Fatal(err)
	}
	position, err := manager.Evaluate(ctx, EvaluateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Code: `window.scrollY`})
	if err != nil {
		t.Fatal(err)
	}
	if string(position.Value) != "500" {
		t.Fatalf("整页截图后滚动位置=%s，期望 500", position.Value)
	}
}

func TestRealBrowserBatchStopsAtPendingDialog(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Batch Dialogs</title>
<button aria-label="Alert" onclick="alert('a')">Alert</button>
<button aria-label="Confirm" onclick="confirm('c')">Confirm</button>
<button aria-label="Prompt" onclick="prompt('p','v')">Prompt</button>`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: server.URL, Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	steps := []ActStep{}
	for _, name := range []string{"Alert", "Confirm", "Prompt"} {
		steps = append(steps, ActStep{Op: "click", Ref: smokeElementRef(t, snapshot, "button", name)})
	}
	response, err := manager.Act(ctx, ActRequest{SessionID: opened.SessionID, PageID: opened.PageID, Revision: snapshot.Revision, Steps: steps})
	if err != nil {
		t.Fatal(err)
	}
	if response.Completed != 0 || response.FailedStep != 1 || len(response.Dialogs) != 1 {
		t.Fatalf("批量动作没有停在第一个待处理弹窗: %+v", response)
	}
	dialog := response.Dialogs[0]
	if dialog.Type != "alert" || dialog.Handled || dialog.ID == 0 {
		t.Fatalf("批量动作弹窗状态=%+v", dialog)
	}
}

func TestRealBrowserExposesDialogOnPageLoad(t *testing.T) {
	started := time.Now()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Boot Dialog</title><script>alert('boot')</script><h1>loaded</h1>`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: server.URL})
	if err != nil {
		t.Fatalf("加载时弹窗阻塞了 browserOpen: %v", err)
	}
	openElapsed := time.Since(started)
	t.Logf("browserOpen 返回耗时 %s", openElapsed)
	if openElapsed > 5*time.Second {
		t.Fatalf("加载弹窗被辅助页面请求锁住: %s", openElapsed)
	}
	snapshot, err := manager.Snapshot(context.Background(), SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil || len(snapshot.Dialogs) != 1 || snapshot.Dialogs[0].Type != "alert" || snapshot.Dialogs[0].Handled {
		t.Fatalf("加载弹窗没有进入页面状态: snapshot=%+v err=%v", snapshot, err)
	}
	handled, err := manager.HandleDialog(context.Background(), HandleDialogRequest{
		SessionID: opened.SessionID, PageID: opened.PageID,
		DialogID: snapshot.Dialogs[0].ID, Action: "accept",
	})
	t.Logf("browserHandleDialog 返回耗时 %s", time.Since(started))
	if err != nil || handled.Snapshot == nil || handled.Dialog.Type != "alert" || !handled.Dialog.Handled || !strings.Contains(handled.Snapshot.Text, "loaded") {
		t.Fatalf("加载弹窗处理后页面没有恢复: response=%+v err=%v", handled, err)
	}
}

func TestRealBrowserDialogsRequireExplicitHandling(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Dialogs</title>
<button aria-label="Alert" onclick="alert('hello')">Alert</button>
<button aria-label="Confirm" onclick="this.textContent = confirm('sure?') ? 'confirmed' : 'dismissed'">Confirm</button>
<button aria-label="Prompt" onclick="this.textContent = prompt('name?', 'default') || 'empty'">Prompt</button>`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: server.URL, Viewport: Viewport{Width: 960, Height: 640}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		dialogType string
		action     string
		promptText string
		expect     string
	}{
		{name: "Alert", dialogType: "alert", action: "accept", expect: "Alert"},
		{name: "Confirm", dialogType: "confirm", action: "dismiss", expect: "dismissed"},
		{name: "Prompt", dialogType: "prompt", action: "accept", promptText: "BrowserKit", expect: "BrowserKit"},
	} {
		snapshot, err = manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
		if err != nil {
			t.Fatal(err)
		}
		ref := smokeElementRef(t, snapshot, "button", test.name)
		response, err := manager.Click(ctx, ClickRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: ref, Revision: snapshot.Revision})
		if err != nil {
			t.Fatalf("%s 点击失败: %v", test.name, err)
		}
		if response.Snapshot == nil || len(response.Snapshot.Dialogs) != 1 {
			t.Fatalf("%s 没有返回待处理弹窗: %+v", test.name, response)
		}
		dialog := response.Snapshot.Dialogs[0]
		if dialog.Type != test.dialogType || dialog.Handled || dialog.ID == 0 || len(dialog.Options) == 0 {
			t.Fatalf("%s 待处理弹窗状态=%+v", test.name, dialog)
		}
		repeated, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
		if err != nil || len(repeated.Dialogs) != 1 || repeated.Dialogs[0].ID != dialog.ID {
			t.Fatalf("%s 弹窗没有稳定保留在页面状态: snapshot=%+v err=%v", test.name, repeated, err)
		}
		handled, err := manager.HandleDialog(ctx, HandleDialogRequest{
			SessionID: opened.SessionID, PageID: opened.PageID,
			DialogID: dialog.ID, Action: test.action, PromptText: test.promptText,
		})
		if err != nil || handled.Snapshot == nil || !handled.Dialog.Handled || handled.Dialog.Accepted != (test.action == "accept") {
			t.Fatalf("%s 处理失败: response=%+v err=%v", test.name, handled, err)
		}
		if test.name != "Alert" && !strings.Contains(handled.Snapshot.Text, test.expect) {
			t.Fatalf("%s 处理后页面结果=%q，期望包含 %q", test.name, handled.Snapshot.Text, test.expect)
		}
	}
}

func waitForSmokeFrameSize(t *testing.T, frames <-chan LiveFrame, width, height int) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case frame, ok := <-frames:
			if !ok {
				t.Fatalf("等待 %dx%d 直播帧时图流关闭", width, height)
			}
			config, _, err := image.DecodeConfig(bytes.NewReader(frame.Data))
			if err == nil && config.Width == width && config.Height == height {
				return
			}
		case <-timer.C:
			t.Fatalf("等待 %dx%d 直播帧超时", width, height)
		}
	}
}

func TestRealBrowserDownloadWaitsForFile(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Download</title><a href="/file">download file</a>`))
	})
	mux.HandleFunc("/file", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Disposition", `attachment; filename="report.txt"`)
		writer.Header().Set("Content-Type", "text/plain")
		_, _ = writer.Write([]byte("download-content"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	dir := t.TempDir()
	manager, err := NewManager(Config{AllowPrivateNetwork: true, DownloadDir: dir, ScreenshotDir: t.TempDir()})
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
	ref := smokeElementRef(t, snapshot, "link", "download file")
	response, err := manager.Download(context.Background(), DownloadRequest{
		SessionID: opened.SessionID, PageID: opened.PageID, Ref: ref, Revision: snapshot.Revision, TimeoutMS: 5000,
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(response.Path)
	if err != nil || string(data) != "download-content" || response.Bytes != int64(len(data)) {
		t.Fatalf("下载文件=%+v data=%q err=%v", response, data, err)
	}
}

func TestRealBrowserDragAcceptsSelectorTarget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Drag</title>
<div id="item" draggable="true" data-value="DRAGGED">drag me</div>
<div id="zone">drop here</div><div id="result">EMPTY</div>
<script>
const item=document.getElementById('item'), zone=document.getElementById('zone');
item.addEventListener('dragstart',e=>e.dataTransfer.setData('text/plain',item.dataset.value));
zone.addEventListener('dragover',e=>e.preventDefault());
zone.addEventListener('drop',e=>{e.preventDefault();document.getElementById('result').textContent='DROP:'+e.dataTransfer.getData('text/plain');});
</script>`))
	}))
	defer server.Close()
	manager, err := NewManager(Config{AllowEvaluate: true, AllowPrivateNetwork: true})
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
	source := smokeElementRef(t, snapshot, "button", "drag me")
	if _, err := manager.Drag(context.Background(), DragRequest{
		SessionID: opened.SessionID, PageID: opened.PageID,
		SourceRef: source, TargetRef: "#zone", Revision: snapshot.Revision,
	}); err != nil {
		t.Fatal(err)
	}
	result, err := manager.Evaluate(context.Background(), EvaluateRequest{
		SessionID: opened.SessionID, PageID: opened.PageID, Code: `document.getElementById('result').textContent`,
	})
	if err != nil || string(result.Value) != `"DROP:DRAGGED"` {
		t.Fatalf("拖放结果=%s err=%v", result.Value, err)
	}
}

func TestRealBrowserDownloadWaitsForChunkedCompletion(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Slow Download</title><a href="/slow">slow file</a>`))
	})
	mux.HandleFunc("/slow", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Disposition", `attachment; filename="slow.bin"`)
		writer.Header().Set("Content-Type", "application/octet-stream")
		_, _ = writer.Write([]byte("0123456789"))
		writer.(http.Flusher).Flush()
		time.Sleep(1200 * time.Millisecond)
		_, _ = writer.Write([]byte("abcde"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "slow.bin"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(Config{AllowPrivateNetwork: true, DownloadDir: dir})
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
	ref := smokeElementRef(t, snapshot, "link", "slow file")
	response, err := manager.Download(context.Background(), DownloadRequest{
		SessionID: opened.SessionID, PageID: opened.PageID, Ref: ref, Revision: snapshot.Revision, TimeoutMS: 8000,
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(response.Path)
	if err != nil || string(data) != "0123456789abcde" || response.Bytes != 15 {
		t.Fatalf("慢速下载未完整: response=%+v data=%q err=%v", response, data, err)
	}
	if response.FileName != "slow (1).bin" {
		t.Fatalf("同名文件没有生成唯一名称: %q", response.FileName)
	}
}

func TestRealBrowserDragRejectsAmbiguousSelector(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Drag</title>
<div id="item" draggable="true" data-value="DRAGGED">drag me</div>
<div class="drop">decoy</div><div class="drop">zone</div><div id="result">EMPTY</div>
<script>
const item=document.getElementById('item');
item.addEventListener('dragstart',e=>e.dataTransfer.setData('text/plain',item.dataset.value));
for (const zone of document.querySelectorAll('.drop')) {
  zone.addEventListener('dragover',e=>e.preventDefault());
  zone.addEventListener('drop',e=>{e.preventDefault();document.getElementById('result').textContent='DROP';});
}
</script>`))
	}))
	defer server.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true})
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
	source := smokeElementRef(t, snapshot, "button", "drag me")
	_, err = manager.Drag(context.Background(), DragRequest{
		SessionID: opened.SessionID, PageID: opened.PageID,
		SourceRef: source, TargetRef: ".drop", Revision: snapshot.Revision,
	})
	var browserErr *Error
	if !errors.As(err, &browserErr) || browserErr.Kind != "ambiguous_target" {
		t.Fatalf("多个 selector 匹配应返回 ambiguous_target: %v", err)
	}
}

func TestRealBrowserDownloadFromNewTab(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>New Tab Download</title><a href="/file" target="_blank">download</a>`))
	})
	mux.HandleFunc("/file", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Disposition", `attachment; filename="tab.txt"`)
		_, _ = writer.Write([]byte("tab-content"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	dir := t.TempDir()
	manager, err := NewManager(Config{AllowPrivateNetwork: true, DownloadDir: dir})
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
	ref := smokeElementRef(t, snapshot, "link", "download")
	response, err := manager.Download(context.Background(), DownloadRequest{
		SessionID: opened.SessionID, PageID: opened.PageID, Ref: ref, Revision: snapshot.Revision, TimeoutMS: 8000,
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(response.Path)
	if err != nil || string(data) != "tab-content" {
		t.Fatalf("新标签下载失败: response=%+v data=%q err=%v", response, data, err)
	}
}

func TestRealBrowserClickScreenshotUsesActiveTab(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Main</title><a href="/new" target="_blank">open new</a>`))
	})
	mux.HandleFunc("/new", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>New Tab</title><h1>New</h1>`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true, ScreenshotDir: t.TempDir()})
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
	ref := smokeElementRef(t, snapshot, "link", "open new")
	response, err := manager.Click(context.Background(), ClickRequest{
		SessionID: opened.SessionID, PageID: opened.PageID, Ref: ref, Revision: snapshot.Revision,
		WaitAfterMS: 1000, IncludeScreenshot: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.ActivePageID == response.PageID {
		t.Fatalf("点击没有激活新 Tab: %+v", response)
	}
	if response.Screenshot == nil || response.Screenshot.PageID != response.ActivePageID {
		t.Fatalf("截图没有使用活动 Tab: page=%s active=%s screenshot=%+v", response.PageID, response.ActivePageID, response.Screenshot)
	}
}

func TestRealBrowserBackgroundTabScreenshotUsesActionPage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Main</title><a href="/other" target="_blank">open other</a>`))
	})
	mux.HandleFunc("/other", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Other</title><button id="action">action</button>`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true, ScreenshotDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	mainSnapshot, err := manager.Snapshot(context.Background(), SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	link := smokeElementRef(t, mainSnapshot, "link", "open other")
	if _, err := manager.Click(context.Background(), ClickRequest{
		SessionID: opened.SessionID, PageID: opened.PageID, Ref: link, Revision: mainSnapshot.Revision, WaitAfterMS: 800,
	}); err != nil {
		t.Fatal(err)
	}
	tabs, err := manager.ListTabs(context.Background(), TabsRequest{SessionID: opened.SessionID})
	if err != nil || len(tabs.Tabs) != 2 {
		t.Fatalf("新 Tab 未创建: %+v %v", tabs, err)
	}
	other := ""
	for _, tab := range tabs.Tabs {
		if tab.PageID != opened.PageID {
			other = tab.PageID
		}
	}
	if other == "" {
		t.Fatal("没有找到新 Tab")
	}
	if _, err := manager.ActivateTab(context.Background(), TabRequest{SessionID: opened.SessionID, PageID: opened.PageID}); err != nil {
		t.Fatal(err)
	}
	otherSnapshot, err := manager.Snapshot(context.Background(), SnapshotRequest{SessionID: opened.SessionID, PageID: other})
	if err != nil {
		t.Fatal(err)
	}
	button := smokeElementRef(t, otherSnapshot, "button", "action")
	response, err := manager.Click(context.Background(), ClickRequest{
		SessionID: opened.SessionID, PageID: other, Ref: button, Revision: otherSnapshot.Revision, IncludeScreenshot: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Screenshot == nil || response.Screenshot.PageID != other {
		t.Fatalf("后台 Tab 截图目标错误: page=%s screenshot=%+v", other, response.Screenshot)
	}
}

func TestRealBrowserActNewTabSnapshotMatchesScreenshot(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Main</title><a href="/new" target="_blank">open new</a>`))
	})
	mux.HandleFunc("/new", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>New Tab</title><h1>New</h1>`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true, ScreenshotDir: t.TempDir()})
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
	ref := smokeElementRef(t, snapshot, "link", "open new")
	response, err := manager.Act(context.Background(), ActRequest{
		SessionID: opened.SessionID, PageID: opened.PageID, Revision: snapshot.Revision,
		IncludeScreenshot: true,
		Steps:             []ActStep{{Op: "click", Ref: ref, WaitAfterMS: 800}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Snapshot == nil || response.Screenshot == nil {
		t.Fatalf("缺少最终快照或截图: %+v", response)
	}
	if response.ActivePageID == response.PageID {
		t.Fatalf("没有切换到新 Tab: %+v", response)
	}
	if response.Snapshot.PageID != response.ActivePageID || response.Screenshot.PageID != response.ActivePageID {
		t.Fatalf("快照与截图没有跟随新 Tab: snapshot=%s screenshot=%s active=%s", response.Snapshot.PageID, response.Screenshot.PageID, response.ActivePageID)
	}
}

func smokeElementRef(t *testing.T, snapshot SnapshotResponse, role, name string) string {
	t.Helper()
	for _, element := range snapshot.Elements {
		if element.Role == role && element.Name == name {
			return element.Ref
		}
	}
	t.Fatalf("快照中没有 %s %q: %+v", role, name, snapshot.Elements)
	return ""
}

func waitForSmokeTabs(t *testing.T, manager *Manager, sessionID string, count int) TabsResponse {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		tabs, err := manager.ListTabs(context.Background(), TabsRequest{SessionID: sessionID})
		if err == nil && len(tabs.Tabs) == count {
			return tabs
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等待 %d 个 Browser Tab 超时", count)
	return TabsResponse{}
}
