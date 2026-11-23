//go:build browser_smoke

package browserkit

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zeoxisca/awesome-browserkit/internal/toollab"
)

var snapshotLabEventPattern = regexp.MustCompile(`EVT-(\d+)`)

func snapshotLabElement(snapshot SnapshotResponse, role, name string) (Element, bool) {
	for _, element := range snapshot.Elements {
		if element.Role == role && element.Name == name {
			return element, true
		}
	}
	return Element{}, false
}

func snapshotLabNames(snapshot SnapshotResponse, prefix string) []string {
	names := make([]string, 0)
	for _, element := range snapshot.Elements {
		if strings.HasPrefix(element.Name, prefix) {
			names = append(names, element.Name)
		}
	}
	return names
}

func snapshotLabMustElement(t *testing.T, snapshot SnapshotResponse, role, name string) Element {
	t.Helper()
	element, ok := snapshotLabElement(snapshot, role, name)
	if !ok {
		t.Fatalf("快照缺少 %s %q", role, name)
	}
	return element
}

func snapshotLabSnapshot(lab *toolLabRunner, sessionID, pageID string, maxElements, offset int) SnapshotResponse {
	lab.t.Helper()
	return toolLabCall[SnapshotResponse](lab, "browserSnapshot", map[string]any{
		"session_id": sessionID, "page_id": pageID,
		"max_chars": 100_000, "max_elements": maxElements, "offset": offset,
	})
}

func snapshotLabEventGeneration(t *testing.T, name string) int {
	t.Helper()
	match := snapshotLabEventPattern.FindStringSubmatch(name)
	if len(match) != 2 {
		t.Fatalf("无法从动态事件名解析代际: %q", name)
	}
	value, err := strconv.Atoi(match[1])
	if err != nil {
		t.Fatal(err)
	}
	return value / 12
}

func TestRealBrowserSnapshotBehaviorLab(t *testing.T) {
	server := httptest.NewServer(toollab.Handler())
	defer server.Close()

	manager, err := NewManager(Config{AllowPrivateNetwork: true, AllowEvaluate: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	lab := &toolLabRunner{t: t, ctx: ctx, manager: manager, called: make(map[string]bool)}
	opened := toolLabCall[OpenResponse](lab, "browserOpen", map[string]any{
		"url": server.URL + "/snapshot.html", "viewport": map[string]int{"width": 1280, "height": 900},
	})
	if opened.LoadStatus != OpenLoadLoaded || opened.Title != "Snapshot behavior lab" {
		t.Fatalf("browserOpen 返回错误页面: %+v", opened)
	}
	sessionID, pageID := opened.SessionID, opened.PageID

	initialStart := time.Now()
	initial := snapshotLabSnapshot(lab, sessionID, pageID, 1_000, 0)
	initialDuration := time.Since(initialStart)
	for _, expected := range []struct{ role, name string }{
		{"textbox", "Account name"}, {"checkbox", "Enable alerts"}, {"combobox", "Region"},
		{"button", "Open review dialog"}, {"button", "Pointer-only action"},
		{"textbox", "Shadow query"}, {"button", "Run shadow search"},
		{"textbox", "Frame code"}, {"button", "Frame action"},
	} {
		snapshotLabMustElement(t, initial, expected.role, expected.name)
	}
	for _, excluded := range []string{"Disabled action", "ARIA disabled action", "Hidden action", "ARIA hidden action", "Confirm review"} {
		for _, element := range initial.Elements {
			if element.Name == excluded {
				t.Fatalf("不可交互或不可见元素不应出现在初始快照中: %q", excluded)
			}
		}
	}
	region := snapshotLabMustElement(t, initial, "combobox", "Region")
	if len(region.Options) != 3 || region.Options[1].Value != "eu-central" || region.Options[1].Label != "EU Central" {
		t.Fatalf("select options 不完整: %+v", region.Options)
	}
	if count := len(snapshotLabNames(initial, "Dense action ")); count != 160 {
		t.Fatalf("完整快照中的密集元素数=%d want=160", count)
	}

	// 状态保真边界：这些断言刻画当前公开 Element 投影遗漏或误分类的信息。
	checked := snapshotLabMustElement(t, initial, "checkbox", "Checked preference")
	checkedJSON, err := json.Marshal(checked)
	if err != nil {
		t.Fatal(err)
	}
	var checkedFields map[string]any
	if err := json.Unmarshal(checkedJSON, &checkedFields); err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"checked", "selected", "pressed", "expanded", "disabled", "value", "href", "bounds"} {
		if _, ok := checkedFields[absent]; ok {
			t.Fatalf("characterization 变化：Element 已包含 %q，需要更新欠缺项分析", absent)
		}
	}
	if _, ok := snapshotLabElement(initial, "switch", "ARIA switch without tabindex"); ok {
		t.Fatal("characterization 变化：无 tabindex 的 ARIA switch 已被发现")
	}
	if _, ok := snapshotLabElement(initial, "button", "Closed shadow action"); ok {
		t.Fatal("characterization 变化：closed Shadow DOM 已被发现")
	}
	editable := snapshotLabMustElement(t, initial, "button", "Editable note")
	if editable.Type != "" {
		t.Fatalf("contenteditable 的当前误分类发生变化: %+v", editable)
	}
	prefilled := snapshotLabMustElement(t, initial, "textbox", "Prefilled account")
	if strings.Contains(string(mustJSON(t, prefilled)), "existing-value") {
		t.Fatal("普通快照不应公开输入值；若产品契约变化需更新分析")
	}
	destination := snapshotLabMustElement(t, initial, "link", "Destination link")
	if strings.Contains(string(mustJSON(t, destination)), "snapshot-fidelity") {
		t.Fatal("characterization 变化：link Element 已公开 href")
	}
	var submitByValue Element
	for _, element := range initial.Elements {
		if element.Type == "submit" {
			submitByValue = element
			break
		}
	}
	if submitByValue.Ref == "" || submitByValue.Name != "" {
		t.Fatalf("input[type=submit] 的 value 名称缺失行为变化: %+v", submitByValue)
	}
	var longName Element
	for _, element := range initial.Elements {
		if strings.HasPrefix(element.Name, "Long accessible action name") {
			longName = element
			break
		}
	}
	if len([]rune(longName.Name)) != 160 {
		t.Fatalf("长名称当前应静默截断到 160 字符: len=%d name=%q", len([]rune(longName.Name)), longName.Name)
	}
	largeSelect := snapshotLabMustElement(t, initial, "combobox", "Large option set")
	if len(largeSelect.Options) != 100 || !largeSelect.OptionsTruncated {
		t.Fatalf("大下拉选项边界错误: options=%d truncated=%t", len(largeSelect.Options), largeSelect.OptionsTruncated)
	}
	multipleSelect := snapshotLabMustElement(t, initial, "combobox", "Multiple regions")
	err = lab.invoke("browserSelectOption", map[string]any{
		"session_id": sessionID, "page_id": pageID, "ref": multipleSelect.Ref,
		"revision": initial.Revision, "value": "west",
	}, new(SelectOptionResponse))
	if err == nil || !strings.Contains(err.Error(), "unsupported_select") {
		t.Fatalf("multiple select 当前应报告 unsupported_select: %v", err)
	}
	for _, name := range []string{"Inert action", "Covered action"} {
		target := snapshotLabMustElement(t, initial, "button", name)
		err = lab.invoke("browserClick", map[string]any{
			"session_id": sessionID, "page_id": pageID, "ref": target.Ref, "revision": initial.Revision,
		}, new(ClickResponse))
		if err == nil || !strings.Contains(err.Error(), "element_not_interactable") {
			t.Fatalf("%s 应先进入快照、再在点击时报告 element_not_interactable: %v", name, err)
		}
	}
	instrumentation := toolLabCall[EvaluateResponse](lab, "browserEvaluate", map[string]any{
		"session_id": sessionID, "page_id": pageID,
		"code": `({global: Boolean(window.__browserkit_page_state_v1__), refs: document.querySelectorAll('[data-browserkit-ref]').length, appValue: document.querySelector('#app-owned-ref').getAttribute('data-browserkit-ref')})`,
	})
	var instrumented struct {
		Global   bool   `json:"global"`
		Refs     int    `json:"refs"`
		AppValue string `json:"appValue"`
	}
	if err := json.Unmarshal(instrumentation.Value, &instrumented); err != nil {
		t.Fatal(err)
	}
	if !instrumented.Global || instrumented.Refs == 0 || instrumented.AppValue == "application-owned-value" {
		t.Fatalf("未观察到快照对 window/DOM 的可见注入: %+v", instrumented)
	}
	if err := lab.invoke("browserWait", map[string]any{
		"session_id": sessionID, "page_id": pageID, "condition": "element_count",
		"ref_or_selector": "button", "timeout_ms": 100,
	}, new(WaitResponse)); err == nil || !strings.Contains(err.Error(), "unsupported_wait") {
		t.Fatalf("未知 wait 条件没有报告 unsupported_wait: %v", err)
	}

	// 对持续替换的列表分两页读取，会把两个不同代际拼成一个逻辑结果集。
	firstFeedIndex := -1
	for index, element := range initial.Elements {
		if strings.HasPrefix(element.Name, "Inspect event EVT-") {
			firstFeedIndex = index
			break
		}
	}
	if firstFeedIndex < 0 {
		t.Fatal("初始快照缺少动态列表")
	}
	feedPageOne := snapshotLabSnapshot(lab, sessionID, pageID, 6, firstFeedIndex)
	time.Sleep(250 * time.Millisecond)
	feedPageTwo := snapshotLabSnapshot(lab, sessionID, pageID, 6, firstFeedIndex+6)
	feedPageOneNames := snapshotLabNames(feedPageOne, "Inspect event EVT-")
	feedPageTwoNames := snapshotLabNames(feedPageTwo, "Inspect event EVT-")
	if len(feedPageOneNames) != 6 || len(feedPageTwoNames) != 6 || snapshotLabEventGeneration(t, feedPageOneNames[0]) == snapshotLabEventGeneration(t, feedPageTwoNames[0]) {
		t.Fatalf("未复现跨 offset 页的动态代际混合: first=%v second=%v", feedPageOneNames, feedPageTwoNames)
	}

	// 同一 document 内的文本变化和 DOM 替换不会提升 revision；同节点 ref
	// 保持稳定，而被替换节点的旧 ref 必须在下一次操作时立即失败。
	mutationBaseline := snapshotLabSnapshot(lab, sessionID, pageID, 1_000, 0)
	inPlace := Element{}
	replaced := Element{}
	for _, element := range mutationBaseline.Elements {
		if element.Role == "button" && strings.HasPrefix(element.Name, "Phase ") {
			inPlace = element
		}
		if element.Role == "button" && strings.HasPrefix(element.Name, "Generation ") {
			replaced = element
			break
		}
	}
	if inPlace.Ref == "" || replaced.Ref == "" {
		t.Fatalf("初始快照缺少变化目标: in_place=%+v replaced=%+v", inPlace, replaced)
	}
	time.Sleep(650 * time.Millisecond)
	err = lab.invoke("browserClick", map[string]any{
		"session_id": sessionID, "page_id": pageID,
		"ref": replaced.Ref, "revision": mutationBaseline.Revision,
	}, new(ClickResponse))
	if err == nil || !strings.Contains(err.Error(), "stale_reference") {
		t.Fatalf("被定时替换元素的旧 ref 没有快速报告 stale_reference: %v", err)
	}
	changed := snapshotLabSnapshot(lab, sessionID, pageID, 1_000, 0)
	if changed.Revision != mutationBaseline.Revision {
		t.Fatalf("同 document 的 DOM 变化不应提升 revision: before=%d after=%d", mutationBaseline.Revision, changed.Revision)
	}
	var changedInPlace, changedReplacement Element
	for _, element := range changed.Elements {
		if strings.HasPrefix(element.Name, "Phase ") {
			changedInPlace = element
		}
		if strings.HasPrefix(element.Name, "Generation ") {
			changedReplacement = element
		}
	}
	if changedInPlace.Ref != inPlace.Ref || changedInPlace.Name == inPlace.Name {
		t.Fatalf("同节点变化的 ref/名称不符合预期: before=%+v after=%+v", inPlace, changedInPlace)
	}
	if changedReplacement.Ref == "" || changedReplacement.Ref == replaced.Ref {
		t.Fatalf("替换节点应取得新 ref: before=%+v after=%+v", replaced, changedReplacement)
	}

	feedBefore := snapshotLabNames(changed, "Inspect event EVT-")
	time.Sleep(250 * time.Millisecond)
	feedChanged := snapshotLabSnapshot(lab, sessionID, pageID, 1_000, 0)
	feedAfter := snapshotLabNames(feedChanged, "Inspect event EVT-")
	if len(feedBefore) != 12 || len(feedAfter) != 12 || strings.Join(feedBefore, "|") == strings.Join(feedAfter, "|") {
		t.Fatalf("高频集合没有被快照观察为不同代: before=%v after=%v", feedBefore, feedAfter)
	}

	// 先冻结动态源，后续分页和异步断言具有确定性。
	toolLabClick(lab, sessionID, pageID, feedChanged, "button", "Pause mutations")
	toolLabClick(lab, sessionID, pageID, feedChanged, "button", "Pause live feed")
	toolLabWait(lab, sessionID, pageID, "text_contains", "Mutations paused")
	toolLabWait(lab, sessionID, pageID, "text_contains", "Feed paused at generation")
	pausedFirst := snapshotLabSnapshot(lab, sessionID, pageID, 1_000, 0)
	time.Sleep(250 * time.Millisecond)
	pausedSecond := snapshotLabSnapshot(lab, sessionID, pageID, 1_000, 0)
	if strings.Join(snapshotLabNames(pausedFirst, "Inspect event EVT-"), "|") != strings.Join(snapshotLabNames(pausedSecond, "Inspect event EVT-"), "|") {
		t.Fatal("暂停后的动态集合仍然变化")
	}
	exactLimit := snapshotLabSnapshot(lab, sessionID, pageID, len(pausedSecond.Elements), 0)
	exactTerminal := snapshotLabSnapshot(lab, sessionID, pageID, len(pausedSecond.Elements), len(pausedSecond.Elements))
	if !exactLimit.Truncated || len(exactTerminal.Elements) != 0 {
		t.Fatalf("未复现 exact-limit truncated 歧义: count=%d truncated=%t terminal=%d", len(pausedSecond.Elements), exactLimit.Truncated, len(exactTerminal.Elements))
	}

	// 关闭的 dialog 不暴露内部操作；打开后快照应发现，关闭后再次消失。
	toolLabClick(lab, sessionID, pageID, pausedSecond, "button", "Open review dialog")
	dialogOpen := snapshotLabSnapshot(lab, sessionID, pageID, 1_000, 0)
	if _, ok := snapshotLabElement(dialogOpen, "button", "Confirm review"); !ok {
		t.Fatal("打开 dialog 后未发现内部按钮")
	}
	_, browserKitKeptModalBackground := snapshotLabElement(dialogOpen, "button", "Open review dialog")
	toolLabClick(lab, sessionID, pageID, dialogOpen, "button", "Close dialog")
	dialogClosed := snapshotLabSnapshot(lab, sessionID, pageID, 1_000, 0)
	if _, ok := snapshotLabElement(dialogClosed, "button", "Confirm review"); ok {
		t.Fatal("关闭 dialog 后仍返回内部按钮")
	}

	// 分阶段请求：立即态、部分态、完成态与错误态都必须可被 wait + snapshot 观察。
	toolLabClick(lab, sessionID, pageID, dialogClosed, "button", "Load async report")
	loading := snapshotLabSnapshot(lab, sessionID, pageID, 1_000, 0)
	if !strings.Contains(loading.Text, "Loading summary") {
		t.Fatalf("未观察到异步立即态: %q", loading.Text)
	}
	if _, ok := snapshotLabElement(loading, "button", "Load async report"); ok {
		t.Fatal("异步加载期间 disabled 按钮不应出现在交互元素中")
	}
	toolLabWait(lab, sessionID, pageID, "text_contains", "Summary ready; loading details")
	partial := snapshotLabSnapshot(lab, sessionID, pageID, 1_000, 0)
	if !strings.Contains(partial.Text, "Summary ready: 37 records owned by Ada") || strings.Contains(partial.Text, "Async complete") {
		t.Fatalf("异步部分态错误: %q", partial.Text)
	}
	toolLabWait(lab, sessionID, pageID, "text_contains", "Async complete")
	complete := snapshotLabSnapshot(lab, sessionID, pageID, 1_000, 0)
	for _, expected := range []string{"A-17: queued", "B-04: running", "C-99: complete"} {
		if !strings.Contains(complete.Text, expected) {
			t.Fatalf("异步完成态缺少 %q", expected)
		}
	}
	toolLabClick(lab, sessionID, pageID, complete, "button", "Load failing report")
	toolLabWait(lab, sessionID, pageID, "text_contains", "Async error: HTTP 503")

	// max_elements + offset 应能无重无漏地恢复 160 个稳定 dense controls。
	denseNames := make(map[string]bool)
	pageCount := 0
	for offset := 0; ; offset += 25 {
		page := snapshotLabSnapshot(lab, sessionID, pageID, 25, offset)
		pageCount++
		for _, name := range snapshotLabNames(page, "Dense action ") {
			if denseNames[name] {
				t.Fatalf("offset 分页重复返回 %q", name)
			}
			denseNames[name] = true
		}
		if len(page.Elements) == 0 {
			break
		}
		if page.NextOffset != offset+len(page.Elements) {
			t.Fatalf("next_offset=%d want=%d", page.NextOffset, offset+len(page.Elements))
		}
	}
	if len(denseNames) != 160 {
		names := make([]string, 0, len(denseNames))
		for name := range denseNames {
			names = append(names, name)
		}
		sort.Strings(names)
		t.Fatalf("offset 分页恢复 dense controls=%d want=160: %v", len(denseNames), names)
	}

	// 记录稳定态快照耗时分布，只做观测不设易受机器负载影响的硬阈值。
	durations := []time.Duration{initialDuration}
	for index := 0; index < 5; index++ {
		started := time.Now()
		snapshotLabSnapshot(lab, sessionID, pageID, 1_000, 0)
		durations = append(durations, time.Since(started))
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	t.Logf("snapshot-lab: elements=%d dense=160 pages=%d duration_min=%s duration_median=%s duration_max=%s revision=%d modal_background=%t",
		len(initial.Elements), pageCount, durations[0], durations[len(durations)/2], durations[len(durations)-1], initial.Revision, browserKitKeptModalBackground)
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
