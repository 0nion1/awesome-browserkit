package browserkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

const sequenceSchemaVersion = 1

const maxSequenceSteps = 100

var sequenceVariablePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// SequenceVariable 是操作序列使用的可覆盖输入。
type SequenceVariable struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Default     string `json:"default,omitempty"`
	Sensitive   bool   `json:"sensitive,omitempty"`
}

// SequencePage 使用逻辑别名描述序列涉及的页面。
type SequencePage struct {
	Alias   string `json:"alias"`
	Opener  string `json:"opener,omitempty"`
	URLHint string `json:"url_hint,omitempty"`
}

// SequenceProviderCall 描述回放期间需要宿主外部能力介入的步骤。
type SequenceProviderCall struct {
	Capability    string         `json:"capability"`
	Instruction   string         `json:"instruction,omitempty"`
	Variable      string         `json:"variable"`
	SourceLocator ElementLocator `json:"source_locator,omitempty"`
	TargetLocator ElementLocator `json:"target_locator"`
}

// SequenceStep 是可验证和回放的单个浏览器动作。
type SequenceStep struct {
	ID             string                `json:"id"`
	Type           string                `json:"type"`
	Page           string                `json:"page"`
	URL            string                `json:"url,omitempty"`
	Locator        ElementLocator        `json:"locator,omitempty"`
	ValueTemplate  string                `json:"value_template,omitempty"`
	Append         bool                  `json:"append,omitempty"`
	Submit         bool                  `json:"submit,omitempty"`
	WaitAfterMS    int                   `json:"wait_after_ms,omitempty"`
	ScriptTemplate string                `json:"script_template,omitempty"`
	ScriptMode     string                `json:"script_mode,omitempty"`
	Provider       *SequenceProviderCall `json:"provider,omitempty"`
	Group          string                `json:"group,omitempty"`
}

// SequenceGroup 保存模型清洗后的逻辑分组，不改变组内动作内容或顺序。
type SequenceGroup struct {
	ID      string   `json:"id"`
	Purpose string   `json:"purpose,omitempty"`
	Steps   []string `json:"steps"`
}

// BrowserSequence 是可跨 session 保存和回放的版本化操作序列。
type BrowserSequence struct {
	SchemaVersion          int                `json:"schema_version"`
	Goal                   string             `json:"goal"`
	EnvironmentRequirement string             `json:"environment_requirement"`
	Variables              []SequenceVariable `json:"variables,omitempty"`
	Pages                  []SequencePage     `json:"pages"`
	Groups                 []SequenceGroup    `json:"groups,omitempty"`
	Steps                  []SequenceStep     `json:"steps"`
}

// SequenceCandidate 是探索阶段已经实际执行过的候选动作。
type SequenceCandidate struct {
	CandidateID string       `json:"candidate_id"`
	Outcome     string       `json:"outcome"`
	Step        SequenceStep `json:"step"`
}

// SequenceStartRequest 开始收集一次探索产生的候选步骤。
type SequenceStartRequest struct {
	SessionID string             `json:"session_id"`
	PageID    string             `json:"page_id"`
	Goal      string             `json:"goal"`
	Variables []SequenceVariable `json:"variables,omitempty"`
}

// SequenceReadRequest 读取当前 session 的候选步骤。
type SequenceReadRequest struct {
	SessionID string `json:"session_id"`
}

// SequenceDraftResponse 返回当前草稿，不回显临时元素 ref。
type SequenceDraftResponse struct {
	SessionID  string              `json:"session_id"`
	Goal       string              `json:"goal"`
	Variables  []SequenceVariable  `json:"variables,omitempty"`
	Pages      []SequencePage      `json:"pages"`
	Candidates []SequenceCandidate `json:"candidates"`
}

// SequenceSelectionGroup 由模型选择候选步骤组成最终逻辑分组。
type SequenceSelectionGroup struct {
	CandidateIDs []string `json:"candidate_ids"`
	Purpose      string   `json:"purpose,omitempty"`
}

// SequenceFinishRequest 完成候选清洗。候选只能删除和分组，不能重排或改写。
type SequenceFinishRequest struct {
	SessionID  string                   `json:"session_id"`
	Groups     []SequenceSelectionGroup `json:"groups"`
	OutputPath string                   `json:"output_path,omitempty"`
}

// SequenceResponse 返回最终操作序列。
type SequenceResponse struct {
	SessionID string `json:"session_id"`
	// Path 是已保存 JSON 文件的绝对路径。
	Path     string          `json:"path"`
	Sequence BrowserSequence `json:"sequence"`
}

// SequenceRecordingStartRequest 开启用户手动操作录制。
type SequenceRecordingStartRequest struct {
	PageID string `json:"page_id"`
	Goal   string `json:"goal,omitempty"`
}

// SequenceRecordingFinishRequest 停止手动录制并保存最终序列。
type SequenceRecordingFinishRequest struct {
	OutputPath string `json:"output_path,omitempty"`
}

// SequenceRecordingState 是 BrowserView 使用的手动录制状态。
type SequenceRecordingState struct {
	SessionID      string `json:"session_id,omitempty"`
	Recording      bool   `json:"recording"`
	CandidateCount int    `json:"candidate_count"`
}

// SequenceScriptRequest 显式执行并记录一段序列脚本。
type SequenceScriptRequest struct {
	SessionID    string `json:"session_id"`
	PageID       string `json:"page_id"`
	CodeTemplate string `json:"code_template"`
	Mode         string `json:"mode,omitempty"`
	TimeoutMS    int    `json:"timeout_ms,omitempty"`
	AwaitPromise *bool  `json:"await_promise,omitempty"`
}

// SequenceScriptResponse 返回脚本结果及候选 ID。
type SequenceScriptResponse struct {
	SessionID   string          `json:"session_id"`
	PageID      string          `json:"page_id"`
	Type        string          `json:"type"`
	Value       json.RawMessage `json:"value"`
	CandidateID string          `json:"candidate_id"`
}

// SequenceProviderCallRequest 记录一次已经由宿主求值的外部能力调用。
type SequenceProviderCallRequest struct {
	SessionID      string `json:"session_id"`
	PageID         string `json:"page_id"`
	TargetRef      string `json:"target_ref"`
	TargetRevision uint64 `json:"target_revision"`
	SourceRef      string `json:"source_ref,omitempty"`
	SourceRevision uint64 `json:"source_revision,omitempty"`
	Capability     string `json:"capability"`
	Instruction    string `json:"instruction,omitempty"`
	Variable       string `json:"variable"`
	Value          string `json:"value"`
}

// SequenceProviderCallResponse 返回已填写的外部结果及候选 ID。
type SequenceProviderCallResponse struct {
	SessionID   string `json:"session_id"`
	PageID      string `json:"page_id"`
	CandidateID string `json:"candidate_id"`
	TextLength  int    `json:"text_length"`
}

// SequenceReplayRequest 在全新临时浏览器环境回放一个序列。
type SequenceReplayRequest struct {
	Sequence  BrowserSequence   `json:"sequence"`
	Variables map[string]string `json:"variables,omitempty"`
}

// SequenceResumeRequest 为暂停的 provider-call 提交结果并续播。
type SequenceResumeRequest struct {
	ReplayID   string `json:"replay_id"`
	Token      string `json:"token"`
	Value      string `json:"value,omitempty"`
	UseDefault bool   `json:"use_default,omitempty"`
}

// SequenceProviderRequest 是暂停时交给宿主处理的外部能力请求。
type SequenceProviderRequest struct {
	Capability    string         `json:"capability"`
	Instruction   string         `json:"instruction,omitempty"`
	Variable      string         `json:"variable"`
	Default       string         `json:"default,omitempty"`
	Sensitive     bool           `json:"sensitive,omitempty"`
	SourceLocator ElementLocator `json:"source_locator,omitempty"`
}

// SequenceReplayResponse 描述完成、暂停或失败的回放进度。
type SequenceReplayResponse struct {
	ReplayID       string                   `json:"replay_id"`
	Status         string                   `json:"status"`
	SessionID      string                   `json:"session_id,omitempty"`
	PageID         string                   `json:"page_id,omitempty"`
	CompletedSteps int                      `json:"completed_steps"`
	TotalSteps     int                      `json:"total_steps"`
	CurrentStepID  string                   `json:"current_step_id,omitempty"`
	Error          string                   `json:"error,omitempty"`
	ResumeToken    string                   `json:"resume_token,omitempty"`
	Provider       *SequenceProviderRequest `json:"provider,omitempty"`
}

type sequenceDraft struct {
	goal          string
	manual        bool
	variables     []SequenceVariable
	variableIndex map[string]int
	pages         []SequencePage
	pageAliases   map[string]string
	candidates    []SequenceCandidate
	manualInputs  map[string]manualSequenceInput
	nextCandidate int
	nextInput     int
}

type manualSequenceInput struct {
	candidateIndex int
	variableIndex  int
}

type sequenceReplayState struct {
	mu           sync.Mutex
	id           string
	sequence     BrowserSequence
	variables    map[string]string
	sessionID    string
	pageAliases  map[string]string
	nextStep     int
	resumeToken  string
	lastToken    string
	lastResponse SequenceReplayResponse
}

func (manager *Manager) forgetSequenceReplays(sessionID string) {
	if manager == nil || sessionID == "" {
		return
	}
	manager.replayMu.Lock()
	defer manager.replayMu.Unlock()
	for replayID, replay := range manager.replays {
		if replay.sessionID == sessionID {
			delete(manager.replays, replayID)
		}
	}
}

// StartSequence 开始记录当前页面上的探索候选动作。
func (manager *Manager) StartSequence(ctx context.Context, request SequenceStartRequest) (SequenceDraftResponse, error) {
	return manager.startSequence(ctx, request, false)
}

func (manager *Manager) startSequence(ctx context.Context, request SequenceStartRequest, manual bool) (SequenceDraftResponse, error) {
	if strings.TrimSpace(request.Goal) == "" {
		return SequenceDraftResponse{}, errors.New("sequenceStart 需要 goal")
	}
	current, currentPage, err := manager.get(ctx, request.SessionID, request.PageID)
	if err != nil {
		return SequenceDraftResponse{}, err
	}
	info, err := currentPage.page.Info(ctx)
	if err != nil {
		return SequenceDraftResponse{}, err
	}
	draft := &sequenceDraft{goal: strings.TrimSpace(request.Goal), manual: manual, variableIndex: map[string]int{}, pageAliases: map[string]string{currentPage.id: "main"}, manualInputs: map[string]manualSequenceInput{}}
	for _, variable := range request.Variables {
		variable.Name = strings.TrimSpace(variable.Name)
		if !sequenceVariablePattern.MatchString(variable.Name) {
			return SequenceDraftResponse{}, fmt.Errorf("序列变量名 %q 无效", variable.Name)
		}
		if _, exists := draft.variableIndex[variable.Name]; exists {
			return SequenceDraftResponse{}, fmt.Errorf("序列变量 %q 重复", variable.Name)
		}
		draft.variableIndex[variable.Name] = len(draft.variables)
		draft.variables = append(draft.variables, variable)
	}
	draft.pages = append(draft.pages, SequencePage{Alias: "main", URLHint: info.URL})
	draft.appendCandidate(SequenceStep{Type: "navigate", Page: "main", URL: info.URL}, "succeeded")
	current.mu.Lock()
	if current.sequenceDraft != nil {
		current.mu.Unlock()
		return SequenceDraftResponse{}, errors.New("当前 session 已经在生成操作序列")
	}
	current.sequenceDraft = draft
	response := draft.response(current.id)
	current.mu.Unlock()
	return response, nil
}

// SequenceRecordingState 返回当前活动 session 的手动录制状态。
func (manager *Manager) SequenceRecordingState(ctx context.Context) (SequenceRecordingState, error) {
	current := manager.activeSession()
	if current == nil {
		return SequenceRecordingState{}, nil
	}
	if err := contextErr(ctx); err != nil {
		return SequenceRecordingState{}, err
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	state := SequenceRecordingState{SessionID: current.id}
	if current.sequenceDraft != nil && current.sequenceDraft.manual {
		state.Recording = true
		state.CandidateCount = len(current.sequenceDraft.candidates)
	}
	return state, nil
}

// StartSequenceRecording 从当前页面开始记录用户通过 BrowserView 产生的语义动作。
func (manager *Manager) StartSequenceRecording(ctx context.Context, request SequenceRecordingStartRequest) (SequenceRecordingState, error) {
	goal := strings.TrimSpace(request.Goal)
	if goal == "" {
		goal = "用户手动录制的浏览器操作"
	}
	draft, err := manager.startSequence(ctx, SequenceStartRequest{PageID: request.PageID, Goal: goal}, true)
	if err != nil {
		return SequenceRecordingState{}, err
	}
	manager.emit(ctx, Event{Kind: "sequence_recording_changed", SessionID: draft.SessionID, PageID: request.PageID})
	return SequenceRecordingState{SessionID: draft.SessionID, Recording: true, CandidateCount: len(draft.Candidates)}, nil
}

// FinishSequenceRecording 等待已接收的页面输入完成，并保存全部手动录制候选。
func (manager *Manager) FinishSequenceRecording(ctx context.Context, request SequenceRecordingFinishRequest) (SequenceResponse, error) {
	current := manager.activeSession()
	if current == nil {
		return SequenceResponse{}, errors.New("当前没有打开的浏览器 session")
	}
	if err := manager.waitLiveInputBarrier(ctx, current); err != nil {
		return SequenceResponse{}, err
	}
	current.mu.Lock()
	draft := current.sequenceDraft
	if draft == nil || !draft.manual {
		current.mu.Unlock()
		return SequenceResponse{}, errors.New("当前 session 没有开启手动录制")
	}
	ids := make([]string, 0, len(draft.candidates))
	for _, candidate := range draft.candidates {
		ids = append(ids, candidate.CandidateID)
	}
	current.mu.Unlock()
	response, err := manager.FinishSequence(ctx, SequenceFinishRequest{SessionID: current.id, OutputPath: request.OutputPath, Groups: []SequenceSelectionGroup{{CandidateIDs: ids, Purpose: "用户手动录制"}}})
	if err == nil {
		manager.emit(ctx, Event{Kind: "sequence_recording_changed", SessionID: current.id})
	}
	return response, err
}

// ReadSequence 读取当前候选动作草稿。
func (manager *Manager) ReadSequence(ctx context.Context, request SequenceReadRequest) (SequenceDraftResponse, error) {
	current, _, err := manager.get(ctx, request.SessionID, "")
	if err != nil {
		return SequenceDraftResponse{}, err
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	if current.sequenceDraft == nil {
		return SequenceDraftResponse{}, errors.New("当前 session 没有正在生成的操作序列")
	}
	return current.sequenceDraft.response(current.id), nil
}

// FinishSequence 根据候选 ID 生成不可篡改动作内容的最终序列。
func (manager *Manager) FinishSequence(ctx context.Context, request SequenceFinishRequest) (SequenceResponse, error) {
	current, _, err := manager.get(ctx, request.SessionID, "")
	if err != nil {
		return SequenceResponse{}, err
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	draft := current.sequenceDraft
	if draft == nil {
		return SequenceResponse{}, errors.New("当前 session 没有正在生成的操作序列")
	}
	if len(request.Groups) == 0 {
		return SequenceResponse{}, errors.New("sequenceFinish 需要至少一个候选分组")
	}
	positions := make(map[string]int, len(draft.candidates))
	byID := make(map[string]SequenceCandidate, len(draft.candidates))
	for index, candidate := range draft.candidates {
		positions[candidate.CandidateID] = index
		byID[candidate.CandidateID] = candidate
	}
	sequence := BrowserSequence{SchemaVersion: sequenceSchemaVersion, Goal: draft.goal, EnvironmentRequirement: "fresh", Variables: append([]SequenceVariable(nil), draft.variables...), Pages: append([]SequencePage(nil), draft.pages...)}
	seen := map[string]struct{}{}
	lastPosition := -1
	for groupIndex, selection := range request.Groups {
		if len(selection.CandidateIDs) == 0 {
			return SequenceResponse{}, fmt.Errorf("第 %d 个序列分组为空", groupIndex+1)
		}
		groupID := fmt.Sprintf("group_%d", groupIndex+1)
		group := SequenceGroup{ID: groupID, Purpose: strings.TrimSpace(selection.Purpose)}
		for _, candidateID := range selection.CandidateIDs {
			candidate, ok := byID[candidateID]
			if !ok {
				return SequenceResponse{}, fmt.Errorf("候选步骤 %q 不存在", candidateID)
			}
			if _, duplicate := seen[candidateID]; duplicate {
				return SequenceResponse{}, fmt.Errorf("候选步骤 %q 被重复选择", candidateID)
			}
			if positions[candidateID] <= lastPosition {
				return SequenceResponse{}, errors.New("候选步骤不能重排")
			}
			seen[candidateID] = struct{}{}
			lastPosition = positions[candidateID]
			step := candidate.Step
			step.ID = fmt.Sprintf("step_%d", len(sequence.Steps)+1)
			step.Group = groupID
			sequence.Steps = append(sequence.Steps, step)
			group.Steps = append(group.Steps, step.ID)
		}
		sequence.Groups = append(sequence.Groups, group)
	}
	if err := validateBrowserSequence(sequence); err != nil {
		return SequenceResponse{}, err
	}
	path, err := saveBrowserSequence(sequence, request.OutputPath)
	if err != nil {
		return SequenceResponse{}, err
	}
	current.sequenceDraft = nil
	return SequenceResponse{SessionID: current.id, Path: path, Sequence: sequence}, nil
}

func saveBrowserSequence(sequence BrowserSequence, outputPath string) (string, error) {
	data, err := json.MarshalIndent(sequence, "", "  ")
	if err != nil {
		return "", fmt.Errorf("编码操作序列: %w", err)
	}
	data = append(data, '\n')
	if strings.TrimSpace(outputPath) == "" {
		file, err := os.CreateTemp("", "browserkit-sequence-*.json")
		if err != nil {
			return "", fmt.Errorf("创建操作序列临时文件: %w", err)
		}
		path, err := filepath.Abs(file.Name())
		if err != nil {
			_ = file.Close()
			_ = os.Remove(file.Name())
			return "", fmt.Errorf("解析操作序列临时文件路径: %w", err)
		}
		if err := writeNewSequenceFile(file, data); err != nil {
			_ = os.Remove(path)
			return "", err
		}
		return path, nil
	}
	path, err := filepath.Abs(strings.TrimSpace(outputPath))
	if err != nil {
		return "", fmt.Errorf("解析操作序列文件路径: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("操作序列文件已存在: %s", path)
		}
		return "", fmt.Errorf("创建操作序列文件: %w", err)
	}
	if err := writeNewSequenceFile(file, data); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

func writeNewSequenceFile(file *os.File, data []byte) error {
	path := file.Name()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("写入操作序列文件 %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("关闭操作序列文件 %s: %w", path, err)
	}
	return nil
}

func (draft *sequenceDraft) appendCandidate(step SequenceStep, outcome string) string {
	draft.nextCandidate++
	id := fmt.Sprintf("candidate_%d", draft.nextCandidate)
	step.ID = id
	draft.candidates = append(draft.candidates, SequenceCandidate{CandidateID: id, Outcome: outcome, Step: step})
	return id
}

func (draft *sequenceDraft) response(sessionID string) SequenceDraftResponse {
	return SequenceDraftResponse{SessionID: sessionID, Goal: draft.goal, Variables: append([]SequenceVariable(nil), draft.variables...), Pages: append([]SequencePage(nil), draft.pages...), Candidates: append([]SequenceCandidate(nil), draft.candidates...)}
}

func (draft *sequenceDraft) pageAlias(value *page, current *session) string {
	if alias := draft.pageAliases[value.id]; alias != "" {
		return alias
	}
	alias := fmt.Sprintf("page_%d", len(draft.pages)+1)
	opener := draft.pageAliases[value.openerTabID]
	draft.pageAliases[value.id] = alias
	draft.pages = append(draft.pages, SequencePage{Alias: alias, Opener: opener, URLHint: value.info.URL})
	return alias
}

func (draft *sequenceDraft) variableFor(value string, requested string, sensitive bool) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested != "" {
		if !sequenceVariablePattern.MatchString(requested) {
			return "", fmt.Errorf("序列变量名 %q 无效", requested)
		}
		if index, ok := draft.variableIndex[requested]; ok {
			if draft.variables[index].Default == "" {
				draft.variables[index].Default = value
			}
			draft.variables[index].Sensitive = draft.variables[index].Sensitive || sensitive
			return requested, nil
		}
		draft.variableIndex[requested] = len(draft.variables)
		draft.variables = append(draft.variables, SequenceVariable{Name: requested, Default: value, Sensitive: sensitive})
		return requested, nil
	}
	for index := range draft.variables {
		if draft.variables[index].Default == value {
			draft.variables[index].Sensitive = draft.variables[index].Sensitive || sensitive
			return draft.variables[index].Name, nil
		}
	}
	for {
		draft.nextInput++
		name := fmt.Sprintf("input_%d", draft.nextInput)
		if _, exists := draft.variableIndex[name]; exists {
			continue
		}
		draft.variableIndex[name] = len(draft.variables)
		draft.variables = append(draft.variables, SequenceVariable{Name: name, Default: value, Sensitive: sensitive})
		return name, nil
	}
}

func recordSequenceStep(current *session, currentPage *page, step SequenceStep, outcome string) string {
	if current == nil || currentPage == nil {
		return ""
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	if current.sequenceDraft == nil {
		return ""
	}
	step.Page = current.sequenceDraft.pageAlias(currentPage, current)
	return current.sequenceDraft.appendCandidate(step, outcome)
}

func recordSequenceInput(current *session, currentPage *page, element Element, request TypeRequest) string {
	if current == nil || currentPage == nil {
		return ""
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	draft := current.sequenceDraft
	if draft == nil {
		return ""
	}
	variable, err := draft.variableFor(request.Text, request.SequenceVariable, element.Type == "password")
	if err != nil {
		return ""
	}
	step := SequenceStep{Type: "input", Page: draft.pageAlias(currentPage, current), Locator: locatorForSequenceElement(element), ValueTemplate: "{{" + variable + "}}", Append: request.Append, Submit: request.Submit}
	return draft.appendCandidate(step, "succeeded")
}

func (manager *Manager) manualSequenceElementAt(ctx context.Context, current *session, currentPage *page, x, y float64) (Element, error) {
	snapshot, err := currentPage.page.Snapshot(ctx, SnapshotOptions{MaxChars: 1, MaxElements: maxSnapshotElements})
	if err != nil {
		return Element{}, err
	}
	current.mu.Lock()
	rememberSnapshotRefs(currentPage, snapshot)
	current.mu.Unlock()
	inspector, ok := currentPage.page.(pointInspector)
	if !ok {
		return Element{}, errors.New("当前浏览器页面不支持点击录制")
	}
	inspected, err := inspector.InspectPoint(ctx, x, y)
	if err != nil {
		return Element{}, err
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	element := currentPage.elements[inspected.Ref]
	if element.Ref != "" {
		return element, nil
	}
	return Element{}, errors.New("点击位置没有可录制元素")
}

func (manager *Manager) recordManualSequenceInput(ctx context.Context, current *session, currentPage *page, event InputEvent, clicked Element) {
	if event.Kind == "mouse" && event.Action == "up" && event.Button == "left" && clicked.Ref != "" {
		recordSequenceStep(current, currentPage, SequenceStep{Type: "click", Locator: locatorForSequenceElement(clicked)}, "succeeded")
		manager.emit(ctx, Event{Kind: "sequence_recording_changed", SessionID: current.id, PageID: currentPage.id})
		return
	}
	inputChanged := event.Kind == "text" || event.Kind == "composition" || event.Kind == "key" && event.Action == "down" && (event.Text != "" || event.Key == "Backspace" || event.Key == "Delete") || event.Kind == "shortcut" && (event.Action == "paste" || event.Action == "select_all")
	if !inputChanged {
		return
	}
	snapshot, err := currentPage.page.Snapshot(ctx, SnapshotOptions{MaxChars: 1, MaxElements: maxSnapshotElements})
	if err != nil || snapshot.ActiveElementRef == "" {
		return
	}
	current.mu.Lock()
	rememberSnapshotRefs(currentPage, snapshot)
	draft := current.sequenceDraft
	if draft == nil || !draft.manual {
		current.mu.Unlock()
		return
	}
	element := currentPage.elements[snapshot.ActiveElementRef]
	if element.Ref == "" {
		current.mu.Unlock()
		return
	}
	key := currentPage.id + "\x00" + element.Ref
	if existing, ok := draft.manualInputs[key]; ok {
		draft.variables[existing.variableIndex].Default = snapshot.ActiveElementValue
		draft.candidates[existing.candidateIndex].Step.ValueTemplate = "{{" + draft.variables[existing.variableIndex].Name + "}}"
		current.mu.Unlock()
		manager.emit(ctx, Event{Kind: "sequence_recording_changed", SessionID: current.id, PageID: currentPage.id})
		return
	}
	variable, err := draft.variableFor(snapshot.ActiveElementValue, "", element.Type == "password")
	if err != nil {
		current.mu.Unlock()
		return
	}
	variableIndex := draft.variableIndex[variable]
	draft.appendCandidate(SequenceStep{Type: "input", Page: draft.pageAlias(currentPage, current), Locator: locatorForSequenceElement(element), ValueTemplate: "{{" + variable + "}}"}, "succeeded")
	draft.manualInputs[key] = manualSequenceInput{candidateIndex: len(draft.candidates) - 1, variableIndex: variableIndex}
	current.mu.Unlock()
	manager.emit(ctx, Event{Kind: "sequence_recording_changed", SessionID: current.id, PageID: currentPage.id})
}

func locatorForSequenceElement(element Element) ElementLocator {
	locator := element.Locator
	if locator.Role == "" {
		locator.Role = element.Role
	}
	if locator.Name == "" {
		locator.Name = element.Name
	}
	if locator.Type == "" {
		locator.Type = element.Type
	}
	if locator.FrameURL == "" {
		locator.FrameURL = element.FrameURL
	}
	return locator
}

func updateSequenceCandidateOutcome(current *session, candidateID, outcome string) {
	if current == nil || candidateID == "" {
		return
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	if current.sequenceDraft == nil {
		return
	}
	for index := range current.sequenceDraft.candidates {
		if current.sequenceDraft.candidates[index].CandidateID == candidateID {
			current.sequenceDraft.candidates[index].Outcome = outcome
			return
		}
	}
}

func sequenceElement(current *session, currentPage *page, ref string) (Element, bool) {
	current.mu.Lock()
	defer current.mu.Unlock()
	element, ok := currentPage.elements[ref]
	return element, ok
}

func sequenceElementAtRevision(current *session, currentPage *page, ref string, revision uint64) (Element, error) {
	current.mu.Lock()
	defer current.mu.Unlock()
	if err := validatePageRef(currentPage, ref, revision); err != nil {
		return Element{}, err
	}
	element, ok := currentPage.elements[ref]
	if !ok {
		return Element{}, browserError("invalid_reference", "当前 Tab 没有该元素的序列定位信息", "在目标 page_id 上重新调用 browserSnapshot", true)
	}
	return element, nil
}

func resolveSequenceTemplate(template string, variables map[string]string) (string, error) {
	result := template
	for name, value := range variables {
		result = strings.ReplaceAll(result, "{{"+name+"}}", value)
	}
	if strings.Contains(result, "{{") {
		return "", errors.New("序列模板包含未解析变量")
	}
	return result, nil
}

func sequenceVariables(definitions []SequenceVariable, overrides map[string]string) (map[string]string, error) {
	result := make(map[string]string, len(definitions))
	for _, variable := range definitions {
		if !sequenceVariablePattern.MatchString(variable.Name) {
			return nil, fmt.Errorf("序列变量名 %q 无效", variable.Name)
		}
		result[variable.Name] = variable.Default
	}
	for name, value := range overrides {
		if _, ok := result[name]; !ok {
			return nil, fmt.Errorf("序列没有声明变量 %q", name)
		}
		result[name] = value
	}
	return result, nil
}

func validateBrowserSequence(sequence BrowserSequence) error {
	if sequence.SchemaVersion != sequenceSchemaVersion {
		return fmt.Errorf("不支持操作序列版本 %d", sequence.SchemaVersion)
	}
	if sequence.EnvironmentRequirement != "fresh" {
		return errors.New("当前只支持 fresh 回放环境")
	}
	if len(sequence.Steps) == 0 || len(sequence.Steps) > maxSequenceSteps {
		return fmt.Errorf("操作序列步骤数必须位于 1 到 %d", maxSequenceSteps)
	}
	if sequence.Steps[0].Type != "navigate" || strings.TrimSpace(sequence.Steps[0].URL) == "" {
		return errors.New("操作序列首步必须是 navigate")
	}
	variables := map[string]struct{}{}
	defaults := make(map[string]string, len(sequence.Variables))
	for _, variable := range sequence.Variables {
		if !sequenceVariablePattern.MatchString(variable.Name) {
			return fmt.Errorf("序列变量名 %q 无效", variable.Name)
		}
		if _, duplicate := variables[variable.Name]; duplicate {
			return fmt.Errorf("序列变量 %q 重复", variable.Name)
		}
		variables[variable.Name] = struct{}{}
		defaults[variable.Name] = variable.Default
	}
	pages := map[string]struct{}{}
	for _, page := range sequence.Pages {
		page.Alias = strings.TrimSpace(page.Alias)
		if page.Alias == "" {
			return errors.New("操作序列页面别名不能为空")
		}
		if _, duplicate := pages[page.Alias]; duplicate {
			return fmt.Errorf("操作序列页面别名 %q 重复", page.Alias)
		}
		pages[page.Alias] = struct{}{}
	}
	stepIDs := map[string]struct{}{}
	for _, step := range sequence.Steps {
		if strings.TrimSpace(step.ID) == "" {
			return errors.New("操作序列步骤 ID 不能为空")
		}
		if _, duplicate := stepIDs[step.ID]; duplicate {
			return fmt.Errorf("操作序列步骤 ID %q 重复", step.ID)
		}
		stepIDs[step.ID] = struct{}{}
		if _, ok := pages[step.Page]; !ok {
			return fmt.Errorf("步骤 %s 使用了未知页面 %q", step.ID, step.Page)
		}
		switch step.Type {
		case "navigate":
			if strings.TrimSpace(step.URL) == "" {
				return fmt.Errorf("步骤 %s 缺少 url", step.ID)
			}
			if _, err := resolveSequenceTemplate(step.URL, defaults); err != nil {
				return fmt.Errorf("步骤 %s 的 URL 模板无效: %w", step.ID, err)
			}
		case "click", "input":
			if locatorEmpty(step.Locator) {
				return fmt.Errorf("步骤 %s 缺少定位器", step.ID)
			}
			if step.Type == "input" {
				if _, err := resolveSequenceTemplate(step.ValueTemplate, defaults); err != nil {
					return fmt.Errorf("步骤 %s 的输入模板无效: %w", step.ID, err)
				}
			}
		case "script":
			if strings.TrimSpace(step.ScriptTemplate) == "" {
				return fmt.Errorf("步骤 %s 缺少脚本", step.ID)
			}
			if _, err := resolveSequenceTemplate(step.ScriptTemplate, defaults); err != nil {
				return fmt.Errorf("步骤 %s 的脚本模板无效: %w", step.ID, err)
			}
			if step.ScriptMode != "" && step.ScriptMode != "action" && step.ScriptMode != "assertion" {
				return fmt.Errorf("步骤 %s 的脚本模式 %q 无效", step.ID, step.ScriptMode)
			}
		case "provider-call":
			if step.Provider == nil || strings.TrimSpace(step.Provider.Capability) == "" || strings.TrimSpace(step.Provider.Variable) == "" || locatorEmpty(step.Provider.TargetLocator) {
				return fmt.Errorf("步骤 %s 的 provider-call 无效", step.ID)
			}
			if _, ok := variables[step.Provider.Variable]; !ok {
				return fmt.Errorf("步骤 %s 使用了未声明变量 %q", step.ID, step.Provider.Variable)
			}
		default:
			return fmt.Errorf("步骤 %s 类型 %q 不受支持", step.ID, step.Type)
		}
	}
	return nil
}

func locatorEmpty(locator ElementLocator) bool {
	return locator.Selector == "" && locator.XPath == "" && locator.AbsoluteXPath == "" && (locator.Role == "" || locator.Name == "")
}

func sequenceVariableDefinition(sequence BrowserSequence, name string) SequenceVariable {
	for _, variable := range sequence.Variables {
		if variable.Name == name {
			return variable
		}
	}
	return SequenceVariable{Name: name}
}

// ExecuteSequenceScript 执行并记录显式脚本动作。
func (manager *Manager) ExecuteSequenceScript(ctx context.Context, request SequenceScriptRequest) (SequenceScriptResponse, error) {
	if !manager.config.AllowEvaluate {
		return SequenceScriptResponse{}, browserError("javascript_blocked", "宿主未启用 Browser JavaScript 能力", "在 Browser 配置中显式设置 allow_evaluate", true)
	}
	current, currentPage, err := manager.get(ctx, request.SessionID, request.PageID)
	if err != nil {
		return SequenceScriptResponse{}, err
	}
	current.mu.Lock()
	draft := current.sequenceDraft
	if draft == nil {
		current.mu.Unlock()
		return SequenceScriptResponse{}, errors.New("sequenceScript 只能在 sequenceStart 之后调用")
	}
	variables := make(map[string]string, len(draft.variables))
	for _, variable := range draft.variables {
		variables[variable.Name] = variable.Default
	}
	current.mu.Unlock()
	code, err := resolveSequenceTemplate(request.CodeTemplate, variables)
	if err != nil {
		return SequenceScriptResponse{}, err
	}
	mode := strings.TrimSpace(request.Mode)
	if mode == "" {
		mode = "action"
	}
	if mode != "action" && mode != "assertion" {
		return SequenceScriptResponse{}, errors.New("sequenceScript mode 只支持 action 或 assertion")
	}
	evaluated, err := manager.Evaluate(ctx, EvaluateRequest{SessionID: request.SessionID, PageID: request.PageID, Code: code, TimeoutMS: request.TimeoutMS, AwaitPromise: request.AwaitPromise})
	if err != nil {
		return SequenceScriptResponse{}, err
	}
	if mode == "assertion" && string(evaluated.Value) != "true" {
		return SequenceScriptResponse{}, errors.New("sequenceScript assertion 没有返回 true")
	}
	id := recordSequenceStep(current, currentPage, SequenceStep{Type: "script", ScriptTemplate: request.CodeTemplate, ScriptMode: mode}, "succeeded")
	return SequenceScriptResponse{SessionID: current.id, PageID: currentPage.id, Type: evaluated.Type, Value: evaluated.Value, CandidateID: id}, nil
}

// ExecuteSequenceProviderCall 使用宿主已取得的值完成输入并记录外部能力步骤。
func (manager *Manager) ExecuteSequenceProviderCall(ctx context.Context, request SequenceProviderCallRequest) (SequenceProviderCallResponse, error) {
	if strings.TrimSpace(request.Capability) == "" || strings.TrimSpace(request.Variable) == "" {
		return SequenceProviderCallResponse{}, errors.New("sequenceProviderCall 需要 capability 和 variable")
	}
	current, currentPage, err := manager.get(ctx, request.SessionID, request.PageID)
	if err != nil {
		return SequenceProviderCallResponse{}, err
	}
	target, err := sequenceElementAtRevision(current, currentPage, request.TargetRef, request.TargetRevision)
	if err != nil {
		return SequenceProviderCallResponse{}, fmt.Errorf("sequenceProviderCall target_ref 无效: %w", err)
	}
	var source Element
	if request.SourceRef != "" {
		source, err = sequenceElementAtRevision(current, currentPage, request.SourceRef, request.SourceRevision)
		if err != nil {
			return SequenceProviderCallResponse{}, fmt.Errorf("sequenceProviderCall source_ref 无效: %w", err)
		}
	}
	current.mu.Lock()
	if current.sequenceDraft == nil {
		current.mu.Unlock()
		return SequenceProviderCallResponse{}, errors.New("sequenceProviderCall 只能在 sequenceStart 之后调用")
	}
	variable, variableErr := current.sequenceDraft.variableFor(request.Value, request.Variable, target.Type == "password")
	current.mu.Unlock()
	if variableErr != nil {
		return SequenceProviderCallResponse{}, variableErr
	}
	typed, err := manager.typeElement(ctx, TypeRequest{SessionID: request.SessionID, PageID: request.PageID, Ref: request.TargetRef, Revision: request.TargetRevision, Text: request.Value}, false)
	if err != nil {
		return SequenceProviderCallResponse{}, err
	}
	provider := &SequenceProviderCall{Capability: strings.TrimSpace(request.Capability), Instruction: strings.TrimSpace(request.Instruction), Variable: variable, SourceLocator: locatorForSequenceElement(source), TargetLocator: locatorForSequenceElement(target)}
	id := recordSequenceStep(current, currentPage, SequenceStep{Type: "provider-call", Provider: provider}, "succeeded")
	return SequenceProviderCallResponse{SessionID: current.id, PageID: currentPage.id, CandidateID: id, TextLength: typed.TextLength}, nil
}

// ReplaySequence 在全新的本地临时浏览器 profile 中开始回放。
func (manager *Manager) ReplaySequence(ctx context.Context, request SequenceReplayRequest) (SequenceReplayResponse, error) {
	if err := validateBrowserSequence(request.Sequence); err != nil {
		return SequenceReplayResponse{}, err
	}
	variables, err := sequenceVariables(request.Sequence.Variables, request.Variables)
	if err != nil {
		return SequenceReplayResponse{}, err
	}
	firstURL, err := resolveSequenceTemplate(request.Sequence.Steps[0].URL, variables)
	if err != nil {
		return SequenceReplayResponse{}, err
	}
	if err := manager.validateAddress(firstURL, ""); err != nil {
		return SequenceReplayResponse{}, err
	}
	opened, err := manager.openFreshReplay(ctx, firstURL)
	if err != nil {
		return SequenceReplayResponse{}, err
	}
	state := &sequenceReplayState{id: manager.newID("replay"), sequence: request.Sequence, variables: variables, sessionID: opened.SessionID, pageAliases: map[string]string{request.Sequence.Steps[0].Page: opened.PageID}, nextStep: 1}
	manager.replayMu.Lock()
	manager.replays[state.id] = state
	manager.replayMu.Unlock()
	state.mu.Lock()
	defer state.mu.Unlock()
	return manager.continueSequenceReplay(ctx, state)
}

// ResumeSequence 为当前 provider-call 提交结果并继续回放。
func (manager *Manager) ResumeSequence(ctx context.Context, request SequenceResumeRequest) (SequenceReplayResponse, error) {
	manager.replayMu.Lock()
	state := manager.replays[request.ReplayID]
	manager.replayMu.Unlock()
	if state == nil {
		return SequenceReplayResponse{}, errors.New("sequenceResume 的 replay_id 不存在")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if request.Token == state.lastToken && request.Token != "" {
		return state.lastResponse, nil
	}
	if request.Token == "" || request.Token != state.resumeToken {
		return SequenceReplayResponse{}, errors.New("sequenceResume token 无效或已经过期")
	}
	if state.nextStep >= len(state.sequence.Steps) || state.sequence.Steps[state.nextStep].Type != "provider-call" {
		return SequenceReplayResponse{}, errors.New("当前回放没有等待 provider-call")
	}
	step := state.sequence.Steps[state.nextStep]
	value := request.Value
	if request.UseDefault {
		value = state.variables[step.Provider.Variable]
	}
	state.lastToken = request.Token
	state.resumeToken = ""
	pageID, err := manager.resolveReplayPage(ctx, state, step.Page)
	if err != nil {
		return manager.failReplay(state, step, err), nil
	}
	ref, revision, err := manager.resolveSequenceLocator(ctx, state.sessionID, pageID, step.Provider.TargetLocator)
	if err != nil {
		return manager.failReplay(state, step, err), nil
	}
	if _, err := manager.typeElement(ctx, TypeRequest{SessionID: state.sessionID, PageID: pageID, Ref: ref, Revision: revision, Text: value}, false); err != nil {
		return manager.failReplay(state, step, err), nil
	}
	state.nextStep++
	response, err := manager.continueSequenceReplay(ctx, state)
	if err == nil {
		state.lastResponse = response
	}
	return response, err
}

func (manager *Manager) openFreshReplay(ctx context.Context, address string) (OpenResponse, error) {
	config := manager.config.Session
	config.RemoteAddress = ""
	if isRemoteAddress(config.ExecPath) {
		config.ExecPath = ""
	}
	config.UserDataDir = ""
	config.CleanupUserDataDir = true
	return manager.openNewWithConfig(ctx, OpenRequest{URL: address}, config)
}

func (manager *Manager) continueSequenceReplay(ctx context.Context, state *sequenceReplayState) (SequenceReplayResponse, error) {
	for state.nextStep < len(state.sequence.Steps) {
		step := state.sequence.Steps[state.nextStep]
		if step.Type == "provider-call" {
			definition := sequenceVariableDefinition(state.sequence, step.Provider.Variable)
			token := manager.newID("resume")
			state.resumeToken = token
			pageID, err := manager.resolveReplayPage(ctx, state, step.Page)
			if err != nil {
				return manager.failReplay(state, step, err), nil
			}
			response := SequenceReplayResponse{ReplayID: state.id, Status: "needs_provider", SessionID: state.sessionID, PageID: pageID, CompletedSteps: state.nextStep, TotalSteps: len(state.sequence.Steps), CurrentStepID: step.ID, ResumeToken: token, Provider: &SequenceProviderRequest{Capability: step.Provider.Capability, Instruction: step.Provider.Instruction, Variable: step.Provider.Variable, Default: definition.Default, Sensitive: definition.Sensitive, SourceLocator: step.Provider.SourceLocator}}
			state.lastResponse = response
			return response, nil
		}
		if err := manager.executeSequenceStep(ctx, state, step); err != nil {
			return manager.failReplay(state, step, err), nil
		}
		state.nextStep++
	}
	response := SequenceReplayResponse{ReplayID: state.id, Status: "completed", SessionID: state.sessionID, CompletedSteps: state.nextStep, TotalSteps: len(state.sequence.Steps)}
	if pageID, err := manager.resolveReplayPage(ctx, state, "main"); err == nil {
		response.PageID = pageID
	}
	state.lastResponse = response
	return response, nil
}

func (manager *Manager) executeSequenceStep(ctx context.Context, state *sequenceReplayState, step SequenceStep) error {
	pageID, err := manager.resolveReplayPage(ctx, state, step.Page)
	if err != nil {
		return err
	}
	switch step.Type {
	case "navigate":
		urlValue, err := resolveSequenceTemplate(step.URL, state.variables)
		if err != nil {
			return err
		}
		_, err = manager.Navigate(ctx, NavigateRequest{SessionID: state.sessionID, PageID: pageID, URL: urlValue})
		return err
	case "click":
		ref, revision, err := manager.resolveSequenceLocator(ctx, state.sessionID, pageID, step.Locator)
		if err != nil {
			return err
		}
		_, err = manager.Click(ctx, ClickRequest{SessionID: state.sessionID, PageID: pageID, Ref: ref, Revision: revision, WaitAfterMS: step.WaitAfterMS})
		return err
	case "input":
		value, err := resolveSequenceTemplate(step.ValueTemplate, state.variables)
		if err != nil {
			return err
		}
		ref, revision, err := manager.resolveSequenceLocator(ctx, state.sessionID, pageID, step.Locator)
		if err != nil {
			return err
		}
		_, err = manager.typeElement(ctx, TypeRequest{SessionID: state.sessionID, PageID: pageID, Ref: ref, Revision: revision, Text: value, Append: step.Append, Submit: step.Submit}, false)
		return err
	case "script":
		code, err := resolveSequenceTemplate(step.ScriptTemplate, state.variables)
		if err != nil {
			return err
		}
		result, err := manager.Evaluate(ctx, EvaluateRequest{SessionID: state.sessionID, PageID: pageID, Code: code})
		if err != nil {
			return err
		}
		if step.ScriptMode == "assertion" && string(result.Value) != "true" {
			return errors.New("序列脚本断言没有返回 true")
		}
		return nil
	default:
		return fmt.Errorf("不支持序列步骤 %q", step.Type)
	}
}

func (manager *Manager) resolveReplayPage(ctx context.Context, state *sequenceReplayState, alias string) (string, error) {
	if pageID := state.pageAliases[alias]; pageID != "" {
		return pageID, nil
	}
	current, _, err := manager.get(ctx, state.sessionID, "")
	if err != nil {
		return "", err
	}
	if err := manager.reconcileTabs(ctx, current); err != nil {
		return "", err
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	used := make(map[string]struct{}, len(state.pageAliases))
	for _, pageID := range state.pageAliases {
		used[pageID] = struct{}{}
	}
	if current.activeTab != "" {
		if _, exists := used[current.activeTab]; !exists {
			state.pageAliases[alias] = current.activeTab
			return current.activeTab, nil
		}
	}
	for _, pageID := range current.tabOrder {
		if _, exists := used[pageID]; !exists {
			state.pageAliases[alias] = pageID
			return pageID, nil
		}
	}
	return "", fmt.Errorf("尚未出现序列页面 %q", alias)
}

func (manager *Manager) resolveSequenceLocator(ctx context.Context, sessionID, pageID string, locator ElementLocator) (string, uint64, error) {
	var matches []Element
	var revision uint64
	for offset := 0; offset < maxSnapshotElements; {
		snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: sessionID, PageID: pageID, MaxElements: 200, Offset: offset})
		if err != nil {
			return "", 0, err
		}
		revision = snapshot.Revision
		for _, element := range snapshot.Elements {
			if locatorMatches(locator, element.Locator) {
				matches = append(matches, element)
			}
		}
		if len(matches) > 1 {
			return "", 0, errors.New("序列定位器匹配到多个元素")
		}
		if !snapshot.Truncated || len(snapshot.Elements) == 0 {
			break
		}
		offset = snapshot.NextOffset
	}
	if len(matches) == 1 {
		return matches[0].Ref, revision, nil
	}
	return "", 0, errors.New("序列定位器没有匹配到可交互元素")
}

func locatorMatches(want, actual ElementLocator) bool {
	if want.FrameURL != "" && want.FrameURL != actual.FrameURL {
		return false
	}
	if want.Selector != "" && want.Selector == actual.Selector {
		return true
	}
	if want.XPath != "" && want.XPath == actual.XPath {
		return true
	}
	if want.AbsoluteXPath != "" && want.AbsoluteXPath == actual.AbsoluteXPath {
		return true
	}
	return want.Role != "" && want.Name != "" && want.Role == actual.Role && want.Name == actual.Name && (want.Type == "" || want.Type == actual.Type)
}

func (manager *Manager) failReplay(state *sequenceReplayState, step SequenceStep, err error) SequenceReplayResponse {
	state.resumeToken = ""
	response := SequenceReplayResponse{ReplayID: state.id, Status: "failed", SessionID: state.sessionID, CompletedSteps: state.nextStep, TotalSteps: len(state.sequence.Steps), CurrentStepID: step.ID, Error: err.Error()}
	if pageID := state.pageAliases[step.Page]; pageID != "" {
		response.PageID = pageID
	}
	state.lastResponse = response
	return response
}
