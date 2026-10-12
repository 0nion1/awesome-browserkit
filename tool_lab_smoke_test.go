//go:build browser_smoke

package browserkit

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	_ "image/png"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zeoxisca/awesome-browserkit/internal/toollab"
)

// toolLabCall 按函数签名解码 JSON 请求并序列化响应，模拟宿主的工具调用边界。
// 截图的二进制数据刻意不进入 JSON；测试从响应中的文件路径读取它。
type toolLabRunner struct {
	t       *testing.T
	ctx     context.Context
	manager *Manager
	called  map[string]bool
}

func (lab *toolLabRunner) invoke(name string, request any, response any) error {
	lab.t.Helper()
	function := functionByName(lab.t, lab.manager, name)
	lab.called[name] = true
	handler := reflect.ValueOf(function.Handler)
	payload, err := json.Marshal(request)
	if err != nil {
		lab.t.Fatal(err)
	}
	argument := reflect.New(handler.Type().In(1))
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(argument.Interface()); err != nil {
		lab.t.Fatalf("%s 请求解码失败: %v", name, err)
	}
	results := handler.Call([]reflect.Value{reflect.ValueOf(lab.ctx), argument.Elem()})
	if !results[1].IsNil() {
		return results[1].Interface().(error)
	}
	encoded, err := json.Marshal(results[0].Interface())
	if err != nil {
		lab.t.Fatalf("%s 响应编码失败: %v", name, err)
	}
	if err := json.Unmarshal(encoded, response); err != nil {
		lab.t.Fatalf("%s 响应解码失败: %v", name, err)
	}
	return nil
}

func toolLabCall[T any](lab *toolLabRunner, name string, request any) T {
	lab.t.Helper()
	var response T
	if err := lab.invoke(name, request, &response); err != nil {
		lab.t.Fatalf("%s(%v): %v", name, request, err)
	}
	return response
}

func toolLabElement(t *testing.T, snapshot SnapshotResponse, role, name string) Element {
	t.Helper()
	for _, element := range snapshot.Elements {
		if element.Role == role && element.Name == name {
			return element
		}
	}
	t.Fatalf("快照缺少 %s %q: %+v", role, name, snapshot.Elements)
	return Element{}
}

func toolLabWait(lab *toolLabRunner, sessionID, pageID, condition, value string) {
	lab.t.Helper()
	response := toolLabCall[WaitResponse](lab, "browserWait", map[string]any{
		"session_id": sessionID, "page_id": pageID, "condition": condition,
		"ref_or_selector": value, "timeout_ms": 5_000,
	})
	if !response.Completed {
		lab.t.Fatalf("browserWait 未完成: %+v", response)
	}
}

func toolLabClick(lab *toolLabRunner, sessionID, pageID string, snapshot SnapshotResponse, role, name string) {
	lab.t.Helper()
	element := toolLabElement(lab.t, snapshot, role, name)
	result := toolLabCall[ClickResponse](lab, "browserClick", map[string]any{
		"session_id": sessionID, "page_id": pageID,
		"ref": element.Ref, "revision": snapshot.Revision,
	})
	if !result.Changed || result.Ref != element.Ref {
		lab.t.Fatalf("browserClick 没有报告目标交互: %+v", result)
	}
}

func TestRealBrowserFunctionToolLab(t *testing.T) {
	server := httptest.NewServer(toollab.Handler())
	defer server.Close()

	manager, err := NewManager(Config{
		AllowPrivateNetwork: true, AllowEvaluate: true, AllowDevtools: true,
		ScreenshotDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	lab := &toolLabRunner{t: t, ctx: ctx, manager: manager, called: make(map[string]bool)}
	opened := toolLabCall[OpenResponse](lab, "browserOpen", map[string]any{
		"url": server.URL + "/", "viewport": map[string]int{"width": 900, "height": 640},
	})
	if opened.LoadStatus != OpenLoadLoaded || opened.Title != "Tool lab dashboard" {
		t.Fatalf("browserOpen 返回错误页面: %+v", opened)
	}
	sessionID, pageID := opened.SessionID, opened.PageID
	tabs := toolLabCall[TabsResponse](lab, "browserTabs", map[string]any{"session_id": sessionID})
	if len(tabs.Tabs) != 1 || tabs.ActivePageID != pageID {
		t.Fatalf("browserTabs 初始状态错误: %+v", tabs)
	}

	// 分页读取和完整读取都走工具请求，检查正文与交互元素的发现能力。
	first := toolLabCall[SnapshotResponse](lab, "browserSnapshot", map[string]any{
		"session_id": sessionID, "page_id": pageID, "max_elements": 3,
	})
	second := toolLabCall[SnapshotResponse](lab, "browserSnapshot", map[string]any{
		"session_id": sessionID, "page_id": pageID, "max_elements": 3, "offset": first.NextOffset,
	})
	if len(first.Elements) != 3 || len(second.Elements) != 3 || first.Elements[0].Ref == second.Elements[0].Ref || !strings.Contains(first.Text, "Tool lab dashboard") {
		t.Fatalf("browserSnapshot 分页或正文错误: first=%+v second=%+v", first, second)
	}
	snapshot := toolLabCall[SnapshotResponse](lab, "browserSnapshot", map[string]any{
		"session_id": sessionID, "page_id": pageID, "max_elements": 100,
	})
	reference := toolLabElement(t, snapshot, "textbox", "Reference code")
	toolLabCall[TypeResponse](lab, "browserType", map[string]any{
		"session_id": sessionID, "page_id": pageID, "ref": reference.Ref,
		"revision": snapshot.Revision, "text": "Q7",
	})
	toolLabWait(lab, sessionID, pageID, "text_contains", "Reference: Q7")
	toolLabClick(lab, sessionID, pageID, snapshot, "checkbox", "Express delivery")
	toolLabWait(lab, sessionID, pageID, "text_contains", "Express: yes")
	priority := toolLabElement(t, snapshot, "combobox", "Priority")
	if len(priority.Options) != 2 || priority.Options[1].Value != "urgent" || priority.Options[1].Label != "Urgent" {
		t.Fatalf("browserSnapshot 没有提供下拉选项: %+v", priority)
	}
	selected := toolLabCall[SelectOptionResponse](lab, "browserSelectOption", map[string]any{
		"session_id": sessionID, "page_id": pageID, "ref": priority.Ref,
		"revision": snapshot.Revision, "value": "urgent",
	})
	if selected.Value != "urgent" {
		t.Fatalf("browserSelectOption 返回值错误: %+v", selected)
	}
	toolLabWait(lab, sessionID, pageID, "text_contains", "Priority: urgent")
	if err := lab.invoke("browserSelectOption", map[string]any{
		"session_id": sessionID, "page_id": pageID, "ref": priority.Ref,
		"revision": snapshot.Revision, "value": "missing",
	}, new(SelectOptionResponse)); err == nil || !strings.Contains(err.Error(), "option_not_found") {
		t.Fatalf("不存在的下拉选项没有报告错误: %v", err)
	}
	toolLabCall[SelectOptionResponse](lab, "browserSelectOption", map[string]any{
		"session_id": sessionID, "page_id": pageID, "ref": priority.Ref,
		"revision": snapshot.Revision, "label": "Normal",
	})
	toolLabWait(lab, sessionID, pageID, "text_contains", "Priority: normal")

	name := toolLabElement(t, snapshot, "textbox", "Customer name")
	toolLabCall[WaitResponse](lab, "browserWait", map[string]any{
		"session_id": sessionID, "page_id": pageID, "condition": "selector_visible",
		"ref_or_selector": name.Ref, "revision": snapshot.Revision, "timeout_ms": 5_000,
	})
	typed := toolLabCall[TypeResponse](lab, "browserType", map[string]any{
		"session_id": sessionID, "page_id": pageID, "ref": name.Ref,
		"revision": snapshot.Revision, "text": "Ada",
	})
	if typed.TextLength != 3 {
		t.Fatalf("browserType 返回长度错误: %+v", typed)
	}
	toolLabCall[TypeResponse](lab, "browserType", map[string]any{
		"session_id": sessionID, "page_id": pageID, "ref": name.Ref,
		"revision": snapshot.Revision, "text": " Lovelace", "append": true,
	})
	toolLabCall[PressResponse](lab, "browserPress", map[string]any{
		"session_id": sessionID, "page_id": pageID, "key": "Enter",
	})
	toolLabWait(lab, sessionID, pageID, "text_contains", "Saved: Ada Lovelace")

	hoverTarget := toolLabElement(t, snapshot, "button", "Hover menu")
	hover := toolLabCall[HoverResponse](lab, "browserHover", map[string]any{
		"session_id": sessionID, "page_id": pageID,
		"ref": hoverTarget.Ref, "revision": snapshot.Revision,
	})
	if hover.Ref != hoverTarget.Ref {
		t.Fatalf("browserHover 目标错误: %+v", hover)
	}
	toolLabWait(lab, sessionID, pageID, "selector_visible", "#hover-action")
	hoverSnapshot := toolLabCall[SnapshotResponse](lab, "browserSnapshot", map[string]any{
		"session_id": sessionID, "page_id": pageID, "max_elements": 100,
	})
	toolLabClick(lab, sessionID, pageID, hoverSnapshot, "button", "Hover action")
	toolLabWait(lab, sessionID, pageID, "text_contains", "Hover action triggered")

	devtools := toolLabCall[DevtoolsOpenResponse](lab, "browserDevtoolsOpen", map[string]any{
		"session_id": sessionID, "page_id": pageID,
	})
	if devtools.Status != "collecting" || devtools.Generation == 0 {
		t.Fatalf("browserDevtoolsOpen 错误: %+v", devtools)
	}
	toolLabClick(lab, sessionID, pageID, snapshot, "button", "Load items")
	toolLabWait(lab, sessionID, pageID, "text_contains", "Loaded 3 items")
	toolLabWait(lab, sessionID, pageID, "selector_hidden", "#loading")
	assets := toolLabCall[AssetsResponse](lab, "browserAssets", map[string]any{
		"session_id": sessionID, "page_id": pageID, "limit": 100,
	})
	assetPaths := make(map[string]bool)
	for _, asset := range assets.Assets {
		if parsed, parseErr := url.Parse(asset.URL); parseErr == nil {
			assetPaths[parsed.Path] = true
		}
	}
	for _, path := range []string{"/lab.css", "/lab.js", "/pixel.svg", "/api/items"} {
		if !assetPaths[path] {
			t.Fatalf("browserAssets 缺少 %s: %+v", path, assets.Assets)
		}
	}
	labURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	filteredAssets := toolLabCall[AssetsResponse](lab, "browserAssets", map[string]any{
		"session_id": sessionID, "page_id": pageID, "host": labURL.Hostname(), "limit": 100,
	})
	if len(filteredAssets.Assets) == 0 || len(filteredAssets.Assets) > len(assets.Assets) {
		t.Fatalf("browserAssets 按 host 过滤结果错误: all=%d filtered=%d", len(assets.Assets), len(filteredAssets.Assets))
	}
	for _, asset := range filteredAssets.Assets {
		parsed, parseErr := url.Parse(asset.URL)
		if parseErr != nil || parsed.Hostname() != labURL.Hostname() {
			t.Fatalf("browserAssets host 过滤后仍包含其他站点: %+v", asset)
		}
	}
	var records DevtoolsReadResponse
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		records = toolLabCall[DevtoolsReadResponse](lab, "browserDevtoolsRead", map[string]any{
			"session_id": sessionID, "page_id": pageID, "generation": devtools.Generation,
			"limit": 100,
		})
		seenConsole, seenNetwork := false, false
		for _, entry := range records.Entries {
			seenConsole = seenConsole || entry.Kind == "console" && strings.Contains(entry.Text, "lab:items-loaded")
			seenNetwork = seenNetwork || entry.Kind == "network" && strings.Contains(entry.URL, "/api/items")
		}
		if seenConsole && seenNetwork {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	seenConsole, seenNetwork := false, false
	for _, entry := range records.Entries {
		seenConsole = seenConsole || entry.Kind == "console" && strings.Contains(entry.Text, "lab:items-loaded")
		seenNetwork = seenNetwork || entry.Kind == "network" && strings.Contains(entry.URL, "/api/items")
	}
	if !seenConsole || !seenNetwork {
		t.Fatalf("browserDevtoolsRead 未捕获 console 和 fetch: %+v", records.Entries)
	}
	stopped := toolLabCall[DevtoolsCloseResponse](lab, "browserDevtoolsClose", map[string]any{
		"session_id": sessionID, "page_id": pageID,
	})
	if stopped.Status != "stopped" || stopped.Entries == 0 {
		t.Fatalf("browserDevtoolsClose 错误: %+v", stopped)
	}
	retained := toolLabCall[DevtoolsReadResponse](lab, "browserDevtoolsRead", map[string]any{
		"session_id": sessionID, "page_id": pageID, "generation": devtools.Generation,
		"limit": 100,
	})
	if retained.Status != "stopped" || len(retained.Entries) == 0 {
		t.Fatalf("关闭诊断后没有保留记录: %+v", retained)
	}

	toolLabClick(lab, sessionID, pageID, snapshot, "button", "Nested target")
	toolLabWait(lab, sessionID, pageID, "text_contains", "Nested target triggered")
	nested := toolLabCall[EvaluateResponse](lab, "browserEvaluate", map[string]any{
		"session_id": sessionID, "page_id": pageID,
		"code": "document.querySelector('#nested-scroll').scrollTop",
	})
	var nestedTop float64
	if err := json.Unmarshal(nested.Value, &nestedTop); err != nil || nestedTop <= 0 {
		t.Fatalf("browserClick 没有滚动嵌套容器: %s, %v", nested.Value, err)
	}
	container := toolLabCall[EvaluateResponse](lab, "browserEvaluate", map[string]any{
		"session_id": sessionID, "page_id": pageID,
		"code": "(() => { const el = document.querySelector('#nested-scroll'); el.scrollTop = 0; const r = el.getBoundingClientRect(); return {x: r.left + r.width / 2, y: r.top + r.height / 2} })()",
	})
	var wheelPoint struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
	}
	if err := json.Unmarshal(container.Value, &wheelPoint); err != nil {
		t.Fatal(err)
	}
	toolLabCall[ScrollResponse](lab, "browserScroll", map[string]any{
		"session_id": sessionID, "page_id": pageID, "delta_y": 280,
		"x": wheelPoint.X, "y": wheelPoint.Y,
	})
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		position := toolLabCall[EvaluateResponse](lab, "browserEvaluate", map[string]any{
			"session_id": sessionID, "page_id": pageID, "code": "document.querySelector('#nested-scroll').scrollTop",
		})
		if err := json.Unmarshal(position.Value, &nestedTop); err != nil {
			t.Fatal(err)
		}
		if nestedTop > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if nestedTop <= 0 {
		t.Fatal("browserScroll 没有滚动嵌套容器")
	}
	toolLabCall[EvaluateResponse](lab, "browserEvaluate", map[string]any{
		"session_id": sessionID, "page_id": pageID,
		"code": "window.scrollTo(0, 0)",
	})
	wheeled := toolLabCall[ScrollResponse](lab, "browserScroll", map[string]any{
		"session_id": sessionID, "page_id": pageID, "delta_y": 450,
	})
	if wheeled.DeltaY != 450 || wheeled.X != 450 || wheeled.Y != 320 {
		t.Fatalf("browserScroll 默认坐标或滚动量错误: %+v", wheeled)
	}
	deadline = time.Now().Add(2 * time.Second)
	var wheelTop float64
	for time.Now().Before(deadline) {
		position := toolLabCall[EvaluateResponse](lab, "browserEvaluate", map[string]any{
			"session_id": sessionID, "page_id": pageID, "code": "window.scrollY",
		})
		if err := json.Unmarshal(position.Value, &wheelTop); err != nil {
			t.Fatal(err)
		}
		if wheelTop > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if wheelTop <= 0 {
		t.Fatal("browserScroll 没有滚动页面")
	}
	toolLabClick(lab, sessionID, pageID, snapshot, "button", "Far target")
	toolLabWait(lab, sessionID, pageID, "text_contains", "Far target triggered")
	scroll := toolLabCall[EvaluateResponse](lab, "browserEvaluate", map[string]any{
		"session_id": sessionID, "page_id": pageID, "code": "window.scrollY",
	})
	var scrollY float64
	if err := json.Unmarshal(scroll.Value, &scrollY); err != nil || scrollY <= 0 {
		t.Fatalf("browserClick 没有滚动长页面: %s, %v", scroll.Value, err)
	}

	full := toolLabCall[ScreenshotResponse](lab, "browserScreenshot", map[string]any{
		"session_id": sessionID, "page_id": pageID, "full_page": true,
	})
	fullData, err := os.ReadFile(full.Path)
	if err != nil {
		t.Fatal(err)
	}
	fullImage, _, err := image.DecodeConfig(bytes.NewReader(fullData))
	if err != nil || full.Bytes != len(fullData) || fullImage.Height <= 640 {
		t.Fatalf("browserScreenshot 全页结果错误: %+v dimensions=%+v err=%v", full, fullImage, err)
	}
	region := toolLabCall[ScreenshotResponse](lab, "browserScreenshot", map[string]any{
		"session_id": sessionID, "page_id": pageID,
		"region": map[string]any{"x": 20, "y": 30, "width": 64, "height": 48, "viewport_width": 900, "viewport_height": 640},
	})
	regionData, err := os.ReadFile(region.Path)
	if err != nil {
		t.Fatal(err)
	}
	regionImage, _, err := image.DecodeConfig(bytes.NewReader(regionData))
	if err != nil || regionImage.Width != 64 || regionImage.Height != 48 {
		t.Fatalf("browserScreenshot 区域尺寸错误: %+v err=%v", regionImage, err)
	}

	oldTarget := toolLabElement(t, snapshot, "button", "Old target")
	toolLabClick(lab, sessionID, pageID, snapshot, "button", "Replace target")
	toolLabWait(lab, sessionID, pageID, "text_contains", "Target replaced")
	err = lab.invoke("browserClick", map[string]any{
		"session_id": sessionID, "page_id": pageID,
		"ref": oldTarget.Ref, "revision": snapshot.Revision,
	}, new(ClickResponse))
	if err == nil || !strings.Contains(err.Error(), "stale_reference") {
		t.Fatalf("被移除元素的旧 ref 没有报 stale_reference: %v", err)
	}

	toolLabClick(lab, sessionID, pageID, snapshot, "link", "Open popup")
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		tabs = toolLabCall[TabsResponse](lab, "browserTabs", map[string]any{"session_id": sessionID})
		if len(tabs.Tabs) == 2 && tabs.ActivePageID != pageID {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(tabs.Tabs) != 2 || tabs.ActivePageID == pageID {
		t.Fatalf("browserClick 没有打开 popup Tab: %+v", tabs)
	}
	popupID := tabs.ActivePageID
	popup := toolLabCall[SnapshotResponse](lab, "browserSnapshot", map[string]any{"session_id": sessionID})
	if popup.PageID != popupID || popup.Title != "Tool lab popup" {
		t.Fatalf("browserSnapshot 没有同步到 popup: %+v", popup)
	}
	toolLabClick(lab, sessionID, popupID, popup, "button", "Popup action")
	toolLabWait(lab, sessionID, popupID, "text_contains", "Popup clicked")
	activated := toolLabCall[TabResponse](lab, "browserActivateTab", map[string]any{
		"session_id": sessionID, "page_id": pageID,
	})
	if !activated.Active || activated.ActivePageID != pageID {
		t.Fatalf("browserActivateTab 未切回主页面: %+v", activated)
	}
	closedTab := toolLabCall[CloseTabResponse](lab, "browserCloseTab", map[string]any{
		"session_id": sessionID, "page_id": popupID,
	})
	if !closedTab.Closed || closedTab.ActivePageID != pageID {
		t.Fatalf("browserCloseTab 未关闭 popup: %+v", closedTab)
	}

	navigated := toolLabCall[NavigateResponse](lab, "browserNavigate", map[string]any{
		"session_id": sessionID, "page_id": pageID, "url": server.URL + "/detail.html",
	})
	if navigated.PageID != pageID {
		t.Fatalf("browserNavigate 更换了页面 ID: %+v", navigated)
	}
	toolLabWait(lab, sessionID, pageID, "title_contains", "Tool lab detail")
	toolLabWait(lab, sessionID, pageID, "url_contains", "/detail.html")
	toolLabWait(lab, sessionID, pageID, "load_complete", "")
	reused := toolLabCall[OpenResponse](lab, "browserOpen", map[string]any{
		"session_id": sessionID, "url": "/",
	})
	if reused.SessionID != sessionID || reused.PageID != pageID || reused.Title != "Tool lab dashboard" {
		t.Fatalf("browserOpen 复用已有 session 失败: %+v", reused)
	}
	closed := toolLabCall[CloseResponse](lab, "browserClose", map[string]any{"session_id": sessionID})
	if !closed.Closed {
		t.Fatalf("browserClose 没有关闭 session: %+v", closed)
	}
	for _, function := range manager.Functions() {
		if !lab.called[function.Name] {
			t.Errorf("靶场未调用函数 %s", function.Name)
		}
	}
	if len(lab.called) != len(manager.Functions()) {
		t.Errorf("调用数=%d，函数数=%d: %v", len(lab.called), len(manager.Functions()), lab.called)
	}
	t.Logf("靶场验证了 %d 个工具函数，以及分页、异步数据、悬停、嵌套滚动、长页滚动、失效引用和多 Tab", len(lab.called))
}
