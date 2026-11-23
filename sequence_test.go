package browserkit

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type manualRecordingPage struct {
	*fakePage
	mu              sync.Mutex
	value           string
	dispatched      chan struct{}
	snapshotStarted chan struct{}
	snapshotRelease chan struct{}
}

type manualRecordingSession struct {
	page   Page
	closed bool
}

func (session *manualRecordingSession) NewPage(context.Context, PageOptions) (Page, error) {
	return session.page, nil
}
func (session *manualRecordingSession) Cookies() ([]Cookie, error) { return nil, nil }
func (session *manualRecordingSession) Close() error               { session.closed = true; return nil }
func (session *manualRecordingSession) Closed() bool               { return session.closed }

func (page *manualRecordingPage) Snapshot(context.Context, SnapshotOptions) (Snapshot, error) {
	if page.snapshotStarted != nil {
		select {
		case page.snapshotStarted <- struct{}{}:
		default:
		}
	}
	if page.snapshotRelease != nil {
		<-page.snapshotRelease
	}
	page.mu.Lock()
	value := page.value
	page.mu.Unlock()
	return Snapshot{Info: page.info, Revision: 1, ActiveElementRef: "e1", ActiveElementValue: value, Elements: []Element{
		{Ref: "e1", Role: "textbox", Name: "Customer name", Locator: ElementLocator{Selector: "#customer", Role: "textbox", Name: "Customer name"}},
		{Ref: "e2", Role: "button", Name: "Save customer", Locator: ElementLocator{Selector: "#save", Role: "button", Name: "Save customer"}},
	}}, nil
}

func (page *manualRecordingPage) InspectPoint(_ context.Context, x, _ float64) (InspectedElement, error) {
	ref, role, name := "e1", "textbox", "Customer name"
	if x >= 100 {
		ref, role, name = "e2", "button", "Save customer"
	}
	return InspectedElement{Ref: ref, Revision: 1, Role: role, Name: name, URL: page.info.URL}, nil
}

func (page *manualRecordingPage) DispatchInput(ctx context.Context, input InputEvent) error {
	if input.Kind == "text" {
		page.mu.Lock()
		page.value += input.Text
		page.mu.Unlock()
	}
	if page.dispatched != nil {
		page.dispatched <- struct{}{}
	}
	return page.fakePage.DispatchInput(ctx, input)
}

func TestSequenceToolsDropOnlySequenceBrowserPrefix(t *testing.T) {
	manager, _ := fakeManager(t, Config{AllowEvaluate: true})
	defer manager.Close(context.Background())
	names := map[string]bool{}
	for _, function := range manager.Functions() {
		names[function.Name] = true
	}
	for _, name := range []string{"browserOpen", "browserClick", "browserEvaluate", "sequenceStart", "sequenceRead", "sequenceScript", "sequenceProviderCall", "sequenceFinish", "sequenceReplay", "sequenceResume"} {
		if !names[name] {
			t.Fatalf("缺少 Function Tool %s", name)
		}
	}
	for name := range names {
		if strings.HasPrefix(name, "browserSequence") {
			t.Fatalf("序列工具仍带 browser 前缀: %s", name)
		}
	}
}

func TestSequenceDraftRecordsAndCleansExecutedCandidates(t *testing.T) {
	manager, _ := fakeManager(t, Config{})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com/login"})
	if err != nil {
		t.Fatal(err)
	}
	started, err := manager.StartSequence(context.Background(), SequenceStartRequest{SessionID: opened.SessionID, PageID: opened.PageID, Goal: "登录", Variables: []SequenceVariable{{Name: "username", Default: "alice"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(started.Candidates) != 1 || started.Candidates[0].Step.Type != "navigate" {
		t.Fatalf("初始候选错误: %+v", started)
	}
	snapshot, err := manager.Snapshot(context.Background(), SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	typed, err := manager.Type(context.Background(), TypeRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: "e1", Revision: snapshot.Revision, Text: "alice", SequenceVariable: "username"})
	if err != nil {
		t.Fatal(err)
	}
	clicked, err := manager.Click(context.Background(), ClickRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: "e1", Revision: snapshot.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if typed.CandidateID == "" || clicked.CandidateID == "" {
		t.Fatalf("操作没有返回候选 ID: %+v %+v", typed, clicked)
	}
	finished, err := manager.FinishSequence(context.Background(), SequenceFinishRequest{SessionID: opened.SessionID, Groups: []SequenceSelectionGroup{{CandidateIDs: []string{"candidate_1", typed.CandidateID}, Purpose: "只保留导航和输入"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(finished.Sequence.Steps) != 2 || finished.Sequence.Steps[1].ValueTemplate != "{{username}}" {
		t.Fatalf("清洗后的序列错误: %+v", finished.Sequence)
	}
	if finished.Sequence.Steps[1].Locator.Selector != "#save" {
		t.Fatalf("没有固化定位器: %+v", finished.Sequence.Steps[1].Locator)
	}
	assertSavedSequence(t, finished)
}

func TestSequenceFinishSavesToRequestedFileAndDoesNotOverwrite(t *testing.T) {
	t.Chdir(t.TempDir())
	manager, _ := fakeManager(t, Config{})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	start := func() {
		if _, err := manager.StartSequence(context.Background(), SequenceStartRequest{SessionID: opened.SessionID, PageID: opened.PageID, Goal: "保存序列"}); err != nil {
			t.Fatal(err)
		}
	}
	start()
	path := "saved-sequence.json"
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	finished, err := manager.FinishSequence(context.Background(), SequenceFinishRequest{SessionID: opened.SessionID, OutputPath: path, Groups: []SequenceSelectionGroup{{CandidateIDs: []string{"candidate_1"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if finished.Path != absolutePath {
		t.Fatalf("指定保存路径错误: got %q want %q", finished.Path, absolutePath)
	}
	assertSavedSequence(t, finished)

	start()
	_, err = manager.FinishSequence(context.Background(), SequenceFinishRequest{SessionID: opened.SessionID, OutputPath: path, Groups: []SequenceSelectionGroup{{CandidateIDs: []string{"candidate_1"}}}})
	if err == nil || !strings.Contains(err.Error(), "已存在") {
		t.Fatalf("没有拒绝覆盖已有序列文件: %v", err)
	}
	if _, err := manager.ReadSequence(context.Background(), SequenceReadRequest{SessionID: opened.SessionID}); err != nil {
		t.Fatalf("保存失败后没有保留草稿: %v", err)
	}
}

func TestManualSequenceRecordingTurnsLiveInputsIntoSemanticSteps(t *testing.T) {
	page := &manualRecordingPage{fakePage: &fakePage{info: PageInfo{URL: "https://example.com/form"}}}
	manager, err := NewManager(Config{Factory: func(context.Context, SessionConfig) (Session, error) {
		return &manualRecordingSession{page: page}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: page.info.URL})
	if err != nil {
		t.Fatal(err)
	}
	started, err := manager.StartSequenceRecording(context.Background(), SequenceRecordingStartRequest{PageID: opened.PageID})
	if err != nil || !started.Recording || started.CandidateCount != 1 {
		t.Fatalf("开启手动录制失败: %+v %v", started, err)
	}
	if err := manager.DispatchLiveInput(context.Background(), opened.PageID, InputEvent{Kind: "text", Text: "Ada Lovelace"}); err != nil {
		t.Fatal(err)
	}
	if err := manager.DispatchLiveInput(context.Background(), opened.PageID, InputEvent{Kind: "mouse", Action: "up", X: 140, Y: 20, Button: "left", ClickCount: 1}); err != nil {
		t.Fatal(err)
	}
	finished, err := manager.FinishSequenceRecording(context.Background(), SequenceRecordingFinishRequest{})
	if err != nil {
		t.Fatal(err)
	}
	assertSavedSequence(t, finished)
	if len(finished.Sequence.Steps) != 3 || finished.Sequence.Steps[0].Type != "navigate" || finished.Sequence.Steps[1].Type != "input" || finished.Sequence.Steps[2].Type != "click" {
		t.Fatalf("低层输入没有归并为语义步骤: %+v", finished.Sequence.Steps)
	}
	if len(finished.Sequence.Variables) != 1 || finished.Sequence.Variables[0].Default != "Ada Lovelace" || finished.Sequence.Steps[1].Locator.Selector != "#customer" || finished.Sequence.Steps[2].Locator.Selector != "#save" {
		t.Fatalf("手动录制变量或定位器错误: %+v", finished.Sequence)
	}
	state, err := manager.SequenceRecordingState(context.Background())
	if err != nil || state.Recording {
		t.Fatalf("保存后仍处于录制状态: %+v %v", state, err)
	}
}

func TestManualSequenceRecordingDoesNotBlockLiveInputOnSnapshot(t *testing.T) {
	page := &manualRecordingPage{fakePage: &fakePage{info: PageInfo{URL: "https://example.com/form"}}, dispatched: make(chan struct{}, 2)}
	manager, err := NewManager(Config{Factory: func(context.Context, SessionConfig) (Session, error) {
		return &manualRecordingSession{page: page}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: page.info.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.StartSequenceRecording(context.Background(), SequenceRecordingStartRequest{PageID: opened.PageID}); err != nil {
		t.Fatal(err)
	}
	page.snapshotStarted = make(chan struct{}, 1)
	page.snapshotRelease = make(chan struct{})
	if err := manager.DispatchLiveInput(context.Background(), opened.PageID, InputEvent{Kind: "text", Text: "A"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-page.dispatched:
	case <-time.After(time.Second):
		t.Fatal("第一条输入没有送达页面")
	}
	select {
	case <-page.snapshotStarted:
	case <-time.After(time.Second):
		t.Fatal("录制快照没有开始")
	}
	if err := manager.DispatchLiveInput(context.Background(), opened.PageID, InputEvent{Kind: "text", Text: "B"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-page.dispatched:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("录制快照阻塞了后续浏览器输入")
	}
	close(page.snapshotRelease)
	finished, err := manager.FinishSequenceRecording(context.Background(), SequenceRecordingFinishRequest{})
	if err != nil {
		t.Fatal(err)
	}
	assertSavedSequence(t, finished)
	if len(finished.Sequence.Variables) != 1 || finished.Sequence.Variables[0].Default != "AB" {
		t.Fatalf("异步录制没有保存最终输入值: %+v", finished.Sequence.Variables)
	}
}

func TestManualSequenceRecordingStateSurvivesTabProjection(t *testing.T) {
	manager, _ := fakeManager(t, Config{})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com/one"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.StartSequenceRecording(context.Background(), SequenceRecordingStartRequest{PageID: opened.PageID}); err != nil {
		t.Fatal(err)
	}
	second, err := manager.NewTab(context.Background(), OpenRequest{SessionID: opened.SessionID, URL: "https://example.com/two"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ActivateTab(context.Background(), TabRequest{SessionID: opened.SessionID, PageID: second.PageID}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ActivateTab(context.Background(), TabRequest{SessionID: opened.SessionID, PageID: opened.PageID}); err != nil {
		t.Fatal(err)
	}
	tabs, err := manager.ListTabs(context.Background(), TabsRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	view := BrowserStateFromTabs(tabs)
	if !tabs.Recording || !view.Recording || tabs.RecordedSteps < 1 || view.RecordedSteps != tabs.RecordedSteps {
		t.Fatalf("切换 Tab 后录制状态丢失: tabs=%+v view=%+v", tabs, view)
	}
}

func assertSavedSequence(t *testing.T, response SequenceResponse) {
	t.Helper()
	if response.Path == "" {
		t.Fatal("sequenceFinish 没有返回保存路径")
	}
	if !filepath.IsAbs(response.Path) {
		t.Fatalf("sequenceFinish 返回了非绝对路径: %q", response.Path)
	}
	if strings.HasPrefix(filepath.Base(response.Path), "browserkit-sequence-") {
		t.Cleanup(func() { _ = os.Remove(response.Path) })
	}
	info, err := os.Stat(response.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("序列文件权限错误: %o", info.Mode().Perm())
	}
	data, err := os.ReadFile(response.Path)
	if err != nil {
		t.Fatal(err)
	}
	var saved BrowserSequence
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("序列文件不是有效 JSON: %v", err)
	}
	if !reflect.DeepEqual(saved, response.Sequence) {
		t.Fatalf("保存的序列与响应不一致:\n保存=%+v\n响应=%+v", saved, response.Sequence)
	}
}

func TestSequenceFinishRejectsCandidateReorder(t *testing.T) {
	manager, _ := fakeManager(t, Config{})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.StartSequence(context.Background(), SequenceStartRequest{SessionID: opened.SessionID, PageID: opened.PageID, Goal: "测试"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Navigate(context.Background(), NavigateRequest{SessionID: opened.SessionID, PageID: opened.PageID, URL: "https://example.com/next"}); err != nil {
		t.Fatal(err)
	}
	_, err = manager.FinishSequence(context.Background(), SequenceFinishRequest{SessionID: opened.SessionID, Groups: []SequenceSelectionGroup{{CandidateIDs: []string{"candidate_2", "candidate_1"}}}})
	if err == nil || !strings.Contains(err.Error(), "不能重排") {
		t.Fatalf("没有拒绝候选重排: %v", err)
	}
}

func TestSequenceReplayUsesFreshConfigAndVariableOverride(t *testing.T) {
	var configs []SessionConfig
	var replayPage *fakePage
	manager, err := NewManager(Config{MaxSessions: 2, Session: SessionConfig{RemoteAddress: "http://127.0.0.1:9222", UserDataDir: "/tmp/browserkit-existing"}, Factory: func(_ context.Context, config SessionConfig) (Session, error) {
		configs = append(configs, config)
		replayPage = &fakePage{info: PageInfo{URL: "about:blank"}}
		return &fakeSession{page: replayPage}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	sequence := BrowserSequence{SchemaVersion: 1, Goal: "输入", EnvironmentRequirement: "fresh", Variables: []SequenceVariable{{Name: "query", Default: "default"}}, Pages: []SequencePage{{Alias: "main"}}, Steps: []SequenceStep{
		{ID: "step_1", Type: "navigate", Page: "main", URL: "https://example.com"},
		{ID: "step_2", Type: "input", Page: "main", Locator: ElementLocator{Selector: "#save"}, ValueTemplate: "{{query}}"},
	}}
	response, err := manager.ReplaySequence(context.Background(), SequenceReplayRequest{Sequence: sequence, Variables: map[string]string{"query": "override"}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != "completed" || replayPage.lastText != "override" {
		t.Fatalf("回放结果错误: %+v text=%q", response, replayPage.lastText)
	}
	if len(configs) != 1 || configs[0].RemoteAddress != "" || configs[0].UserDataDir != "" || !configs[0].CleanupUserDataDir {
		t.Fatalf("回放没有使用 fresh 配置: %+v", configs)
	}
	manager.mu.Lock()
	replaySession := manager.sessions[response.SessionID]
	manager.mu.Unlock()
	if replaySession == nil || replaySession.external {
		t.Fatalf("fresh 回放 session 被误判为外部浏览器: %+v", replaySession)
	}
}

func TestSequenceProviderCallPausesAndResumesIdempotently(t *testing.T) {
	var replayPage *fakePage
	manager, err := NewManager(Config{Factory: func(_ context.Context, _ SessionConfig) (Session, error) {
		replayPage = &fakePage{info: PageInfo{URL: "about:blank"}}
		return &fakeSession{page: replayPage}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	sequence := BrowserSequence{SchemaVersion: 1, Goal: "验证码", EnvironmentRequirement: "fresh", Variables: []SequenceVariable{{Name: "captcha", Default: "1234"}}, Pages: []SequencePage{{Alias: "main"}}, Steps: []SequenceStep{
		{ID: "step_1", Type: "navigate", Page: "main", URL: "https://example.com"},
		{ID: "step_2", Type: "provider-call", Page: "main", Provider: &SequenceProviderCall{Capability: "captcha", Variable: "captcha", SourceLocator: ElementLocator{Selector: "#captcha"}, TargetLocator: ElementLocator{Selector: "#save"}}},
	}}
	paused, err := manager.ReplaySequence(context.Background(), SequenceReplayRequest{Sequence: sequence})
	if err != nil {
		t.Fatal(err)
	}
	if paused.Status != "needs_provider" || paused.ResumeToken == "" || paused.Provider == nil || paused.Provider.SourceLocator.Selector != "#captcha" {
		t.Fatalf("没有暂停等待 provider: %+v", paused)
	}
	resumed, err := manager.ResumeSequence(context.Background(), SequenceResumeRequest{ReplayID: paused.ReplayID, Token: paused.ResumeToken, Value: "9876"})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != "completed" || replayPage.lastText != "9876" {
		t.Fatalf("续播失败: %+v text=%q", resumed, replayPage.lastText)
	}
	repeated, err := manager.ResumeSequence(context.Background(), SequenceResumeRequest{ReplayID: paused.ReplayID, Token: paused.ResumeToken, Value: "changed"})
	if err != nil {
		t.Fatal(err)
	}
	if repeated.Status != "completed" || replayPage.lastText != "9876" {
		t.Fatalf("重复 token 不幂等: %+v text=%q", repeated, replayPage.lastText)
	}
}

func TestSequenceReplayRejectsMalformedDefinitionsBeforeOpeningBrowser(t *testing.T) {
	openCount := 0
	manager, err := NewManager(Config{Factory: func(_ context.Context, _ SessionConfig) (Session, error) {
		openCount++
		return &fakeSession{page: &fakePage{info: PageInfo{URL: "about:blank"}}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())

	valid := func() BrowserSequence {
		return BrowserSequence{SchemaVersion: 1, Goal: "测试", EnvironmentRequirement: "fresh", Variables: []SequenceVariable{{Name: "value"}}, Pages: []SequencePage{{Alias: "main"}}, Steps: []SequenceStep{
			{ID: "step_1", Type: "navigate", Page: "main", URL: "https://example.com"},
			{ID: "step_2", Type: "input", Page: "main", Locator: ElementLocator{Selector: "#save"}, ValueTemplate: "{{value}}"},
		}}
	}
	tests := []struct {
		name   string
		mutate func(*BrowserSequence)
	}{
		{name: "重复变量", mutate: func(sequence *BrowserSequence) {
			sequence.Variables = append(sequence.Variables, SequenceVariable{Name: "value"})
		}},
		{name: "重复页面", mutate: func(sequence *BrowserSequence) { sequence.Pages = append(sequence.Pages, SequencePage{Alias: "main"}) }},
		{name: "重复步骤 ID", mutate: func(sequence *BrowserSequence) { sequence.Steps[1].ID = "step_1" }},
		{name: "未知模板变量", mutate: func(sequence *BrowserSequence) { sequence.Steps[1].ValueTemplate = "{{missing}}" }},
		{name: "未声明 provider 变量", mutate: func(sequence *BrowserSequence) {
			sequence.Steps[1] = SequenceStep{ID: "step_2", Type: "provider-call", Page: "main", Provider: &SequenceProviderCall{Capability: "captcha", Variable: "missing", TargetLocator: ElementLocator{Selector: "#save"}}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sequence := valid()
			test.mutate(&sequence)
			if _, err := manager.ReplaySequence(context.Background(), SequenceReplayRequest{Sequence: sequence}); err == nil {
				t.Fatal("无效序列没有被拒绝")
			}
		})
	}
	if openCount != 0 {
		t.Fatalf("校验失败前不应启动浏览器，实际启动 %d 次", openCount)
	}
}
