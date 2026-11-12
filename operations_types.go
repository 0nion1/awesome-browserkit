package browserkit

import (
	"encoding/json"
)

// OpenRequest 是 browserOpen 的输入。
type OpenRequest struct {
	SessionID string   `json:"session_id,omitempty" jsonschema_description:"可选的已有 session_id；省略时创建新浏览器 session，填写时复用该 session 的当前激活 Tab。"`
	URL       string   `json:"url,omitempty" jsonschema_description:"要打开的 HTTP/HTTPS 地址；省略时新建空白页，复用 session 时保留当前地址；复用时支持相对于当前页面的 URL。"`
	InjectJS  string   `json:"inject_js,omitempty" jsonschema_description:"可选的 JavaScript，需宿主启用 allow_evaluate，并受 MaxScriptBytes 限制。新建 session 时在后续 document 创建早期注入；复用时去重注册并立即执行当前 document，不替换或删除已有脚本。"`
	Viewport  Viewport `json:"viewport,omitempty" jsonschema_description:"新建页面的 CSS 视口尺寸；任一维度省略或不大于 0 时使用 1920×1080；复用 session 时忽略此字段。"`
}

// ViewportRequest 描述宿主为指定浏览器页面设置的 CSS 视口。
type ViewportRequest struct {
	SessionID string   `json:"session_id,omitempty"`
	PageID    string   `json:"page_id"`
	Viewport  Viewport `json:"viewport"`
	// Force 表示即使 Manager 记录的视口与请求一致，也要重新下发到页面。
	// 页面可能被浏览器自身调整过实际尺寸，记录值相同不代表远端已经生效。
	Force bool `json:"force,omitempty"`
}

// OpenLoadStatus 是打开或导航页面后加载与空闲等待的结果，通常最多约 4 秒。
type OpenLoadStatus string

const (
	// OpenLoadLoaded 表示页面在等待窗口内进入空闲。
	OpenLoadLoaded OpenLoadStatus = "loaded"
	// OpenLoadTimeout 表示页面已经打开，但在等待窗口内没有进入空闲。
	OpenLoadTimeout OpenLoadStatus = "timeout"
)

// OpenResponse 是 browserOpen 的输出。
type OpenResponse struct {
	SessionID  string         `json:"session_id"`
	PageID     string         `json:"page_id"`
	URL        string         `json:"url"`
	Title      string         `json:"title,omitempty"`
	LoadStatus OpenLoadStatus `json:"load_status"`
	// Snapshot 是加载超时时当前可读取的页面快照，可直接使用其中的元素引用。
	Snapshot *SnapshotResponse `json:"snapshot,omitempty"`
	// Warning 提醒模型当前内容可能不完整，由模型决定是否继续等待。
	Warning string `json:"warning,omitempty"`
	// Dialogs 是打开或复用页面期间出现、等待处理的弹窗。
	Dialogs []Dialog `json:"dialogs,omitempty"`
}

// TabView 是模型和宿主可见的浏览器 Tab 摘要。
type TabView struct {
	PageID       string   `json:"page_id"`
	URL          string   `json:"url,omitempty"`
	Title        string   `json:"title,omitempty"`
	Viewport     Viewport `json:"viewport,omitempty"`
	OpenerPageID string   `json:"opener_page_id,omitempty"`
	Status       string   `json:"status"`
	Active       bool     `json:"active"`
}

// TabActionRequest 是 browserTabs 的输入；action 省略时列出 Tab。
type TabActionRequest struct {
	SessionID string `json:"session_id" jsonschema:"required,minLength=1" jsonschema_description:"browserOpen 返回的 session_id。"`
	Action    string `json:"action,omitempty" jsonschema:"enum=list,enum=activate,enum=close,enum=new,enum=reload" jsonschema_description:"Tab 动作：list 列出、activate 激活、close 关闭、new 新建、reload 重新加载；省略时 list。"`
	PageID    string `json:"page_id,omitempty" jsonschema_description:"目标 Tab 的 page_id；activate/close/reload 需要。"`
	URL       string `json:"url,omitempty" jsonschema_description:"new 动作要打开的 HTTP/HTTPS 地址；省略时新建空白页。"`
}

// TabActionResponse 是 browserTabs 的统一输出；不同 action 只填充相关字段。
type TabActionResponse struct {
	SessionID    string            `json:"session_id"`
	Action       string            `json:"action"`
	ActivePageID string            `json:"active_page_id,omitempty"`
	TabsRevision uint64            `json:"tabs_revision"`
	TabsChanged  bool              `json:"tabs_changed"`
	Tabs         []TabView         `json:"tabs"`
	PageID       string            `json:"page_id,omitempty"`
	URL          string            `json:"url,omitempty"`
	Title        string            `json:"title,omitempty"`
	Closed       bool              `json:"closed,omitempty"`
	LoadStatus   string            `json:"load_status,omitempty"`
	Snapshot     *SnapshotResponse `json:"snapshot,omitempty"`
	Warning      string            `json:"warning,omitempty"`
}

// TabsResponse 是 browserTabs 列出 Tab 的输出。
type TabsResponse struct {
	SessionID    string    `json:"session_id"`
	ActivePageID string    `json:"active_page_id,omitempty"`
	TabsRevision uint64    `json:"tabs_revision"`
	Tabs         []TabView `json:"tabs"`
}

// TabsRequest 是 browserTabs 的输入。
type TabsRequest struct {
	SessionID string `json:"session_id" jsonschema:"required,minLength=1" jsonschema_description:"browserOpen 返回的 session_id。"`
}

// TabRequest 是 Tab 生命周期操作的内部请求。
type TabRequest struct {
	SessionID string `json:"session_id" jsonschema:"required,minLength=1" jsonschema_description:"browserOpen 返回的 session_id。"`
	PageID    string `json:"page_id" jsonschema:"required,minLength=1" jsonschema_description:"目标 Tab 的 page_id。"`
}

// TabResponse 是 browserTabs activate 动作的输出。
type TabResponse struct {
	SessionID    string `json:"session_id"`
	PageID       string `json:"page_id"`
	URL          string `json:"url,omitempty"`
	Title        string `json:"title,omitempty"`
	Active       bool   `json:"active"`
	ActivePageID string `json:"active_page_id,omitempty"`
	TabsRevision uint64 `json:"tabs_revision"`
	TabsChanged  bool   `json:"tabs_changed"`
}

// CloseTabResponse 是 browserTabs close 动作的输出。
type CloseTabResponse struct {
	SessionID    string `json:"session_id"`
	PageID       string `json:"page_id"`
	Closed       bool   `json:"closed"`
	ActivePageID string `json:"active_page_id,omitempty"`
	TabsRevision uint64 `json:"tabs_revision"`
	TabsChanged  bool   `json:"tabs_changed"`
}

// NavigateRequest 是 browserNavigate 的输入。
type NavigateRequest struct {
	SessionID string `json:"session_id,omitempty" jsonschema_description:"browserOpen 返回的 session_id；省略时用活动 session。"`
	PageID    string `json:"page_id" jsonschema:"required,minLength=1" jsonschema_description:"目标 Tab 的 page_id。"`
	URL       string `json:"url,omitempty" jsonschema:"minLength=1" jsonschema_description:"目标 HTTP/HTTPS 地址，也支持相对于该 Tab 当前地址的 URL；与 direction 二选一。导航后原有快照引用失效。"`
	Direction string `json:"direction,omitempty" jsonschema:"enum=back,enum=forward,enum=reload" jsonschema_description:"back/forward 使用浏览器历史前进或后退，reload 重新加载当前地址；与 url 二选一。"`
}

// NavigateResponse 是 browserNavigate 的输出。
type NavigateResponse struct {
	// LoadStatus 表示加载与空闲等待（通常最多约 4 秒）后的状态。
	LoadStatus OpenLoadStatus `json:"load_status"`
	// Snapshot 是加载超时时当前可读取的页面快照，可直接使用其中的元素引用。
	Snapshot *SnapshotResponse `json:"snapshot,omitempty"`
	// Warning 提醒模型当前内容可能不完整，由模型决定是否继续等待。
	Warning string `json:"warning,omitempty"`

	SessionID    string `json:"session_id"`
	PageID       string `json:"page_id"`
	URL          string `json:"url"`
	Title        string `json:"title,omitempty"`
	ActivePageID string `json:"active_page_id,omitempty"`
	TabsRevision uint64 `json:"tabs_revision"`
	TabsChanged  bool   `json:"tabs_changed"`
	CandidateID  string `json:"candidate_id,omitempty"`
	// Dialogs 是导航期间出现、等待处理的弹窗。
	Dialogs []Dialog `json:"dialogs,omitempty"`
}

// ActRequest 是按顺序执行的一组页面动作；所有 ref 使用同一份快照的 revision。
type ActRequest struct {
	SessionID string    `json:"session_id" jsonschema:"required,minLength=1" jsonschema_description:"browserOpen 返回的 session_id。"`
	PageID    string    `json:"page_id" jsonschema:"required,minLength=1" jsonschema_description:"目标 Tab 的 page_id。"`
	Revision  uint64    `json:"revision,omitempty" jsonschema:"minimum=1" jsonschema_description:"生成这些 ref 的 browserSnapshot revision（JSON 整数，不要加引号）；纯坐标步骤可省略。"`
	Steps     []ActStep `json:"steps" jsonschema:"required,minItems=1,maxItems=20" jsonschema_description:"按顺序执行的动作，最多 20 步；任一步失败立即停止。跨页导航后需要重新 browserSnapshot。"`
	// IncludeScreenshot 在批次结束后附带当前视口截图。
	IncludeScreenshot bool `json:"include_screenshot,omitempty" jsonschema_description:"批次结束后是否附带当前视口截图；用于 canvas、图片或视觉校验等无法从 DOM 读取的场景。"`
}

// ActStep 是批量动作中的一步；只填写 op 对应需要的字段。
type ActStep struct {
	Op            string   `json:"op" jsonschema:"required,enum=click,enum=hover,enum=type,enum=select,enum=check,enum=press,enum=scroll,enum=wait,enum=drag,enum=evaluate" jsonschema_description:"动作类型；evaluate 需要宿主启用 allow_evaluate。"`
	Ref           string   `json:"ref,omitempty" jsonschema_description:"click/hover/type/select/check/press 的目标 ref。"`
	Text          string   `json:"text,omitempty" jsonschema_description:"type 要输入的文本，可以为空；特殊输入类型格式同 browserType。"`
	Append        bool     `json:"append,omitempty" jsonschema_description:"type 时追加而不是覆盖。"`
	Value         string   `json:"value,omitempty" jsonschema_description:"select 的选项值；与 label 二选一。"`
	Label         string   `json:"label,omitempty" jsonschema_description:"select 的选项可见标签；与 value 二选一。"`
	Checked       *bool    `json:"checked,omitempty" jsonschema_description:"check 的目标勾选状态。"`
	Key           string   `json:"key,omitempty" jsonschema_description:"press 的按键名称。"`
	Modifiers     []string `json:"modifiers,omitempty" jsonschema:"enum=Control,enum=Meta,enum=Alt,enum=Shift" jsonschema_description:"press 的修饰键。"`
	Button        string   `json:"button,omitempty" jsonschema:"enum=left,enum=right,enum=middle" jsonschema_description:"click 的鼠标按键。"`
	ClickCount    int      `json:"click_count,omitempty" jsonschema:"minimum=1,maximum=2" jsonschema_description:"click 次数：1 或 2。"`
	WaitAfterMS   int      `json:"wait_after_ms,omitempty" jsonschema_description:"click 后的额外等待毫秒数。"`
	Submit        bool     `json:"submit,omitempty" jsonschema_description:"type 后是否发送 Enter。"`
	DeltaX        float64  `json:"delta_x,omitempty" jsonschema_description:"scroll 的水平距离或 drag 的水平位移。"`
	DeltaY        float64  `json:"delta_y,omitempty" jsonschema_description:"scroll 的垂直距离或 drag 的垂直位移。"`
	X             *float64 `json:"x,omitempty" jsonschema_description:"scroll、click 或 drag 起点的视口 CSS 横坐标。"`
	Y             *float64 `json:"y,omitempty" jsonschema_description:"scroll、click 或 drag 起点的视口 CSS 纵坐标。"`
	Condition     string   `json:"condition,omitempty" jsonschema_description:"wait 条件；与 browserWait 相同。"`
	RefOrSelector string   `json:"ref_or_selector,omitempty" jsonschema_description:"wait 的 CSS selector、ref 或匹配文本。"`
	TimeoutMS     int      `json:"timeout_ms,omitempty" jsonschema:"minimum=1,maximum=600000" jsonschema_description:"该步骤的毫秒超时；省略时受浏览器 OperationTimeout 约束。"`
	SourceRef     string   `json:"source_ref,omitempty" jsonschema_description:"drag 的源元素 ref；与 x/y 二选一。"`
	TargetRef     string   `json:"target_ref,omitempty" jsonschema_description:"drag 的目标：同一快照的 ref、唯一 CSS selector；与 delta_x/delta_y 二选一。"`
	Code          string   `json:"code,omitempty" jsonschema_description:"evaluate 要执行的 JavaScript；需要 allow_evaluate。"`
	AwaitPromise  *bool    `json:"await_promise,omitempty" jsonschema_description:"evaluate 是否等待 Promise；省略时等待。"`
}

// ActStepResult 是批量动作中一步的执行结果。
type ActStepResult struct {
	// Index 从 0 开始；ActResponse.FailedStep 为 1 开始的步骤序号。
	Index               int             `json:"index"`
	Op                  string          `json:"op"`
	Success             bool            `json:"success"`
	Changed             bool            `json:"changed,omitempty"`
	Value               string          `json:"value,omitempty"`
	Result              json.RawMessage `json:"result,omitempty"`
	Type                string          `json:"type,omitempty"`
	Kind                string          `json:"kind,omitempty"`
	Recoverable         bool            `json:"recoverable,omitempty"`
	RecommendedNextStep string          `json:"recommended_next_step,omitempty"`
	TextLength          int             `json:"text_length,omitempty"`
	// Warning 记录步骤已成功但附带等待或快照未完成的事实，例如 click 的
	// wait_after_ms 超时；不要因为 warning 重放已发生的动作。
	Warning string `json:"warning,omitempty"`
	Error   string `json:"error,omitempty"`
}

// ActResponse 是批量动作的统一输出；失败时保留已完成步骤和错误。
type ActResponse struct {
	SessionID     string            `json:"session_id"`
	PageID        string            `json:"page_id"`
	Completed     int               `json:"completed"`
	FailedStep    int               `json:"failed_step,omitempty"`
	Steps         []ActStepResult   `json:"steps"`
	Snapshot      *SnapshotResponse `json:"snapshot,omitempty"`
	SnapshotError string            `json:"snapshot_error,omitempty"`
	// Screenshot 是 include_screenshot 请求的批次结束视口截图。
	Screenshot *ScreenshotResponse `json:"screenshot,omitempty"`
	// ScreenshotError 表示截图失败；动作已经完成，不应重放。
	ScreenshotError string `json:"screenshot_error,omitempty"`
	// ActivePageID 和 Tabs 是批次结束后的 Tab 投影，包含中间步骤打开的新 Tab。
	ActivePageID string    `json:"active_page_id,omitempty"`
	TabsRevision uint64    `json:"tabs_revision"`
	Tabs         []TabView `json:"tabs,omitempty"`
	// Dialogs 是整批步骤中出现、等待处理的弹窗。
	Dialogs []Dialog `json:"dialogs,omitempty"`
	// Cancelled 表示批次因 context 取消而中断。
	Cancelled bool `json:"cancelled,omitempty"`
}

// SnapshotRequest 是 browserSnapshot 的输入。
type SnapshotRequest struct {
	SessionID   string `json:"session_id" jsonschema:"required,minLength=1" jsonschema_description:"browserOpen 返回的 session_id。"`
	PageID      string `json:"page_id,omitempty" jsonschema_description:"可选的目标 page_id；省略时先同步 Tab 状态，再使用当前激活 Tab。"`
	MaxChars    int    `json:"max_chars,omitempty" jsonschema:"minimum=1,maximum=1048576" jsonschema_description:"页面正文最多返回的字符数；省略时默认 12000，最大 1048576，在浏览器内截断。"`
	MaxElements int    `json:"max_elements,omitempty" jsonschema:"minimum=1,maximum=1000" jsonschema_description:"本页最多返回的可交互元素数量；省略时默认 100，最大 1000。"`
	Offset      int    `json:"offset,omitempty" jsonschema:"minimum=0,maximum=1000000000" jsonschema_description:"跳过的可交互元素数量；省略时从 0 开始，使用上一页返回的 next_offset 继续读取；最大 1000000000。"`
	// IncludeScreenshot 在同一次观察中附带当前视口截图。
	IncludeScreenshot bool `json:"include_screenshot,omitempty" jsonschema_description:"是否在快照后附带当前视口截图；用于视觉任务，避免再调用 browserScreenshot。"`
	// ScreenshotGrid 在 include_screenshot 的截图上叠加页面 CSS 坐标网格。
	ScreenshotGrid bool `json:"screenshot_grid,omitempty" jsonschema_description:"截图是否叠加 50px 网格和 100px 坐标标签；用于精确读取视觉目标坐标。"`
}

// SnapshotResponse 是 browserSnapshot 的输出；elements 中的 ref 只属于 page_id。
// tabs_changed 表示相对上一次 Snapshot 的 Tab 投影是否变化；首次 Snapshot 为 true。
type SnapshotResponse struct {
	SessionID string    `json:"session_id"`
	PageID    string    `json:"page_id"`
	Revision  uint64    `json:"revision"`
	URL       string    `json:"url"`
	Title     string    `json:"title,omitempty"`
	Text      string    `json:"text"`
	Elements  []Element `json:"elements"`
	Truncated bool      `json:"truncated"`
	// TextTruncated 表示动作后的自动快照只返回了正文预览；完整正文需显式 browserSnapshot。
	TextTruncated bool      `json:"text_truncated,omitempty"`
	NextOffset    int       `json:"next_offset"`
	ActivePageID  string    `json:"active_page_id,omitempty"`
	TabsRevision  uint64    `json:"tabs_revision"`
	TabsChanged   bool      `json:"tabs_changed"`
	Tabs          []TabView `json:"tabs,omitempty"`
	// Dialogs 是当前阻塞页面、等待 browserHandleDialog 处理的 JavaScript 弹窗。
	Dialogs []Dialog `json:"dialogs,omitempty"`
	// Screenshot 是 include_screenshot 请求的视口截图。
	Screenshot *ScreenshotResponse `json:"screenshot,omitempty"`
	// ScreenshotError 表示截图失败；快照本身仍然有效。
	ScreenshotError string `json:"screenshot_error,omitempty"`
}

// HandleDialogRequest 是 browserHandleDialog 的输入。
type HandleDialogRequest struct {
	SessionID  string `json:"session_id" jsonschema:"required,minLength=1" jsonschema_description:"browserOpen 返回的 session_id。"`
	PageID     string `json:"page_id" jsonschema:"required,minLength=1" jsonschema_description:"存在待处理弹窗的 page_id。"`
	DialogID   uint64 `json:"dialog_id" jsonschema:"required,minimum=1" jsonschema_description:"browserSnapshot 返回的 dialogs[0].id；必须匹配当前弹窗。"`
	Action     string `json:"action" jsonschema:"required,enum=accept,enum=dismiss" jsonschema_description:"处理动作；必须属于当前弹窗的 options。"`
	PromptText string `json:"prompt_text,omitempty" jsonschema_description:"接受 prompt 时提交的文本；其他弹窗省略。"`
}

// HandleDialogResponse 是 browserHandleDialog 的输出。
type HandleDialogResponse struct {
	SessionID string            `json:"session_id"`
	PageID    string            `json:"page_id"`
	Dialog    Dialog            `json:"dialog"`
	Snapshot  *SnapshotResponse `json:"snapshot,omitempty"`
	// SnapshotError 只表示弹窗已处理后附加页面读取失败，不应重放处理动作。
	SnapshotError string `json:"snapshot_error,omitempty"`
}

// ClickRequest 是 browserClick 的输入。
type ClickRequest struct {
	SessionID string `json:"session_id" jsonschema:"required,minLength=1" jsonschema_description:"browserOpen 返回的 session_id。"`
	PageID    string `json:"page_id" jsonschema:"required,minLength=1" jsonschema_description:"目标 Tab 的 page_id。"`
	Ref       string `json:"ref,omitempty" jsonschema:"minLength=1" jsonschema_description:"同一快照的 ref，不能跨 Tab 使用；与 x/y 二选一。"`
	Revision  uint64 `json:"revision,omitempty" jsonschema:"minimum=1" jsonschema_description:"browserSnapshot 的 revision（JSON 整数，不要加引号）；使用 ref 时必填，失效后重新快照。"`
	// X 和 Y 是视口 CSS 坐标，用于没有快照 ref 的视觉目标。
	X *float64 `json:"x,omitempty" jsonschema_description:"视口 CSS 横坐标；与 ref 二选一，用于 canvas 或图片中的目标。"`
	Y *float64 `json:"y,omitempty" jsonschema_description:"视口 CSS 纵坐标；与 ref 二选一，用于 canvas 或图片中的目标。"`
	// Button 指定鼠标按键；省略时使用 left。
	Button string `json:"button,omitempty" jsonschema:"enum=left,enum=right,enum=middle" jsonschema_description:"鼠标按键：left、right 或 middle；省略时使用 left。"`
	// ClickCount 指定单击或双击；省略时使用 1。
	ClickCount  int `json:"click_count,omitempty" jsonschema:"minimum=1,maximum=2" jsonschema_description:"点击次数：1 为单击，2 为双击；省略时使用 1。"`
	WaitAfterMS int `json:"wait_after_ms,omitempty" jsonschema_description:"点击成功后额外等待的毫秒数；省略或为 0 时不额外等待，不能为负数，等待计入 timeout_ms 的请求时限。"`
	TimeoutMS   int `json:"timeout_ms,omitempty" jsonschema_description:"操作超时毫秒数；省略或为 0 时不增加请求级时限，仍受宿主 context 和浏览器 OperationTimeout（默认 30 秒）限制；填写正数时取这些时限中的较短者。"`
	// IncludeScreenshot 在点击和快照后附带当前视口截图。
	IncludeScreenshot bool `json:"include_screenshot,omitempty" jsonschema_description:"点击后是否附带当前视口截图；用于 canvas、图片或视觉校验等无法从 DOM 读取的场景。"`
}

// ClickResponse 是 browserClick 的输出。
type ClickResponse struct {
	SessionID    string `json:"session_id"`
	PageID       string `json:"page_id"`
	Ref          string `json:"ref"`
	Changed      bool   `json:"changed"`
	ActivePageID string `json:"active_page_id,omitempty"`
	TabsRevision uint64 `json:"tabs_revision"`
	TabsChanged  bool   `json:"tabs_changed"`
	CandidateID  string `json:"candidate_id,omitempty"`
	// Snapshot 是操作完成后被操作页面的最新快照。
	Snapshot *SnapshotResponse `json:"snapshot,omitempty"`
	// SnapshotError 表示附加快照失败，或 wait_after_ms 被 timeout_ms 截断；
	// 操作本身已完成，只需重新读取页面，不应重放操作。
	SnapshotError string `json:"snapshot_error,omitempty"`
	// Screenshot 是 include_screenshot 请求的视口截图。
	Screenshot *ScreenshotResponse `json:"screenshot,omitempty"`
	// ScreenshotError 表示截图失败；点击已经完成，不应重放。
	ScreenshotError string `json:"screenshot_error,omitempty"`
}

// HoverRequest 是 browserHover 的输入。
type HoverRequest struct {
	SessionID string `json:"session_id" jsonschema:"required,minLength=1" jsonschema_description:"browserOpen 返回的 session_id。"`
	PageID    string `json:"page_id" jsonschema:"required,minLength=1" jsonschema_description:"目标 Tab 的 page_id。"`
	Ref       string `json:"ref" jsonschema:"required,minLength=1" jsonschema_description:"同一快照的 ref，不能跨 Tab 使用。"`
	Revision  uint64 `json:"revision" jsonschema:"required,minimum=1" jsonschema_description:"browserSnapshot 的 revision（JSON 整数，不要加引号）；失效后重新快照。"`
}

// HoverResponse 是 browserHover 的输出。内部鼠标轨迹不作为响应内容暴露。
type HoverResponse struct {
	SessionID    string `json:"session_id"`
	PageID       string `json:"page_id"`
	Ref          string `json:"ref"`
	ActivePageID string `json:"active_page_id,omitempty"`
	TabsRevision uint64 `json:"tabs_revision"`
	TabsChanged  bool   `json:"tabs_changed"`
	// Snapshot 是操作完成后被操作页面的最新快照。
	Snapshot *SnapshotResponse `json:"snapshot,omitempty"`
	// SnapshotError 表示附加快照失败；操作已完成，只需重新读取页面，不应重放操作。
	SnapshotError string `json:"snapshot_error,omitempty"`
}

// TypeRequest 是 browserType 的输入。text 只用于执行，不出现在响应中。
type TypeRequest struct {
	SessionID string `json:"session_id" jsonschema:"required,minLength=1" jsonschema_description:"browserOpen 返回的 session_id。"`
	PageID    string `json:"page_id" jsonschema:"required,minLength=1" jsonschema_description:"目标 Tab 的 page_id。"`
	Ref       string `json:"ref" jsonschema:"required,minLength=1" jsonschema_description:"同一快照的 ref，不能跨 Tab 使用。"`
	Revision  uint64 `json:"revision" jsonschema:"required,minimum=1" jsonschema_description:"browserSnapshot 的 revision（JSON 整数，不要加引号）；失效后重新快照。"`
	Text      string `json:"text" jsonschema_description:"要输入的文本，可以为空；默认先清空原值，append=true 时追加；响应不会回显输入内容。date 用 YYYY-MM-DD，month 用 YYYY-MM，week 用 YYYY-Www，time 用 HH:MM 或 HH:MM:SS，datetime-local 用 YYYY-MM-DDTHH:MM[:SS]，color 用 #rrggbb，range/number 填数字；秒和数值需符合控件 step 与 min/max。"`
	Append    bool   `json:"append,omitempty" jsonschema_description:"是否把文本追加到元素当前值末尾；省略或为 false 时先清空当前值。"`
	Submit    bool   `json:"submit,omitempty" jsonschema_description:"输入完成后是否向页面发送 Enter；省略或为 false 时只输入文本。"`
	// SequenceVariable 在操作序列录制期间指定该输入使用的变量名。
	SequenceVariable string `json:"sequence_variable,omitempty" jsonschema_description:"录制序列时保存该输入的变量名；省略时自动分配。"`
}

// TypeResponse 是 browserType 的输出，不回显输入内容。
type TypeResponse struct {
	SessionID    string `json:"session_id"`
	PageID       string `json:"page_id"`
	Ref          string `json:"ref"`
	TextLength   int    `json:"text_length"`
	Submitted    bool   `json:"submitted"`
	ActivePageID string `json:"active_page_id,omitempty"`
	TabsRevision uint64 `json:"tabs_revision"`
	TabsChanged  bool   `json:"tabs_changed"`
	CandidateID  string `json:"candidate_id,omitempty"`
	// Snapshot 是操作完成后被操作页面的最新快照。
	Snapshot *SnapshotResponse `json:"snapshot,omitempty"`
	// SnapshotError 表示附加快照失败；操作已完成，只需重新读取页面，不应重放操作。
	SnapshotError string `json:"snapshot_error,omitempty"`
}

// SelectOptionRequest 是 browserSelectOption 的输入。value 和 label 二选一。
type SelectOptionRequest struct {
	SessionID string `json:"session_id" jsonschema:"required,minLength=1" jsonschema_description:"browserOpen 返回的 session_id。"`
	PageID    string `json:"page_id" jsonschema:"required,minLength=1" jsonschema_description:"目标 Tab 的 page_id。"`
	Ref       string `json:"ref" jsonschema:"required,minLength=1" jsonschema_description:"同一快照的 ref，不能跨 Tab 使用。"`
	Revision  uint64 `json:"revision" jsonschema:"required,minimum=1" jsonschema_description:"browserSnapshot 的 revision（JSON 整数，不要加引号）；失效后重新快照。"`
	Value     string `json:"value,omitempty" jsonschema_description:"要选择的原生单选下拉框选项值，使用快照 options 中的 value；与 label 二选一，不能为空。"`
	Label     string `json:"label,omitempty" jsonschema_description:"要选择的原生单选下拉框可见标签，使用快照 options 中的 label；与 value 二选一，不能为空。"`
}

// SelectOptionResponse 是 browserSelectOption 的输出。
type SelectOptionResponse struct {
	SessionID    string `json:"session_id"`
	PageID       string `json:"page_id"`
	Ref          string `json:"ref"`
	Value        string `json:"value"`
	ActivePageID string `json:"active_page_id,omitempty"`
	TabsRevision uint64 `json:"tabs_revision"`
	TabsChanged  bool   `json:"tabs_changed"`
	// Snapshot 是操作完成后被操作页面的最新快照。
	Snapshot *SnapshotResponse `json:"snapshot,omitempty"`
	// SnapshotError 表示附加快照失败；操作已完成，只需重新读取页面，不应重放操作。
	SnapshotError string `json:"snapshot_error,omitempty"`
}

// UploadFileRequest 是 browserUploadFile 的输入。
type UploadFileRequest struct {
	SessionID string `json:"session_id" jsonschema:"required,minLength=1" jsonschema_description:"browserOpen 返回的 session_id。"`
	PageID    string `json:"page_id" jsonschema:"required,minLength=1" jsonschema_description:"目标 Tab 的 page_id。"`
	Ref       string `json:"ref" jsonschema:"required,minLength=1" jsonschema_description:"目标原生 file input 的快照 ref；必须来自同一 page_id 的 browserSnapshot。"`
	Revision  uint64 `json:"revision" jsonschema:"required,minimum=1" jsonschema_description:"browserSnapshot 的 revision（JSON 整数，不要加引号）；失效后重新快照。"`
	Path      string `json:"path" jsonschema:"required,minLength=1" jsonschema_description:"浏览器宿主机器上的绝对文件路径；解析符号链接后必须位于宿主授权的 UploadDir 内，文件必须是大小不超过 10 MiB 的普通文件。"`
}

// UploadFileResponse 只返回文件名，不回显宿主路径或内容。
type UploadFileResponse struct {
	SessionID string `json:"session_id"`
	PageID    string `json:"page_id"`
	Ref       string `json:"ref"`
	FileName  string `json:"file_name"`
	Bytes     int64  `json:"bytes"`
	// Snapshot 是操作完成后被操作页面的最新快照。
	Snapshot *SnapshotResponse `json:"snapshot,omitempty"`
	// SnapshotError 表示附加快照失败；操作已完成，只需重新读取页面，不应重放操作。
	SnapshotError string `json:"snapshot_error,omitempty"`
}

// DownloadRequest 是 browserDownload 的输入。
type DownloadRequest struct {
	SessionID string `json:"session_id" jsonschema:"required,minLength=1" jsonschema_description:"browserOpen 返回的 session_id。"`
	PageID    string `json:"page_id" jsonschema:"required,minLength=1" jsonschema_description:"目标 Tab 的 page_id。"`
	Ref       string `json:"ref" jsonschema:"required,minLength=1" jsonschema_description:"触发下载的链接或按钮的快照 ref；必须来自同一 page_id 的 browserSnapshot。"`
	Revision  uint64 `json:"revision" jsonschema:"required,minimum=1" jsonschema_description:"browserSnapshot 的 revision（JSON 整数，不要加引号）；失效后重新快照。"`
	TimeoutMS int    `json:"timeout_ms,omitempty" jsonschema:"minimum=1,maximum=600000" jsonschema_description:"等待下载完成的毫秒数；省略时使用浏览器 OperationTimeout（默认 30 秒）。"`
}

// DownloadResponse 描述本次下载落到 DownloadDir 的文件。
type DownloadResponse struct {
	SessionID string `json:"session_id"`
	PageID    string `json:"page_id"`
	Ref       string `json:"ref"`
	// FileName 是浏览器实际保存的文件名。
	FileName string `json:"file_name"`
	// Path 是浏览器主机 DownloadDir 内的绝对路径。
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// PressRequest 是 browserPress 的输入。
type PressRequest struct {
	// SessionID 和 PageID 明确指定目标页面。
	SessionID string `json:"session_id" jsonschema:"required,minLength=1" jsonschema_description:"browserOpen 返回的 session_id。"`
	PageID    string `json:"page_id" jsonschema:"required,minLength=1" jsonschema_description:"目标 Tab 的 page_id，ref 不能跨页使用。"`
	// Key 是一个字母、数字或支持的编辑导航键。
	Key string `json:"key" jsonschema:"required,minLength=1" jsonschema_description:"单个英文字母、数字，或 Enter、Tab、Escape、Backspace、Delete、ArrowUp、ArrowDown、ArrowLeft、ArrowRight、Space、Home、End、PageUp、PageDown；组合键通过 modifiers 指定。"`
	// Modifiers 是同时按下的修饰键；不允许重复。
	Modifiers []string `json:"modifiers,omitempty" jsonschema:"uniqueItems=true,maxItems=4,enum=Control,enum=Meta,enum=Alt,enum=Shift" jsonschema_description:"同时按下的修饰键列表，可使用 Control、Meta、Alt、Shift，不允许重复；省略时不按修饰键。"`
	// Ref 和 Revision 一起提供时，先聚焦该元素；省略时使用页面当前焦点。
	Ref      string `json:"ref,omitempty" jsonschema_description:"可选的目标元素引用；填写时必须同时提供 revision，先聚焦该元素再发送按键。"`
	Revision uint64 `json:"revision,omitempty" jsonschema:"minimum=1" jsonschema_description:"ref 所属快照的 revision（JSON 整数，不要加引号）；与 ref 一起提供。"`
}

// PressResponse 是 browserPress 的输出。
type PressResponse struct {
	SessionID    string `json:"session_id"`
	PageID       string `json:"page_id"`
	Key          string `json:"key"`
	ActivePageID string `json:"active_page_id,omitempty"`
	TabsRevision uint64 `json:"tabs_revision"`
	TabsChanged  bool   `json:"tabs_changed"`
	// Snapshot 是操作完成后被操作页面的最新快照。
	Snapshot *SnapshotResponse `json:"snapshot,omitempty"`
	// SnapshotError 表示附加快照失败；操作已完成，只需重新读取页面，不应重放操作。
	SnapshotError string `json:"snapshot_error,omitempty"`
}

// ScrollRequest 是 browserScroll 的输入，距离及坐标均为视口 CSS 像素。
type ScrollRequest struct {
	SessionID string   `json:"session_id" jsonschema:"required,minLength=1" jsonschema_description:"browserOpen 返回的 session_id。"`
	PageID    string   `json:"page_id" jsonschema:"required,minLength=1" jsonschema_description:"目标 Tab 的 page_id。"`
	DeltaX    float64  `json:"delta_x,omitempty" jsonschema_description:"水平滚动距离，单位 CSS 像素；正数向右，负数向左，省略为 0；绝对值不超过 10000，与 delta_y 至少一个非零。"`
	DeltaY    float64  `json:"delta_y,omitempty" jsonschema_description:"垂直滚动距离，单位 CSS 像素；正数向下，负数向上，省略为 0；绝对值不超过 10000，与 delta_x 至少一个非零。"`
	X         *float64 `json:"x,omitempty" jsonschema_description:"滚轮事件起点的视口横坐标，单位 CSS 像素；省略时使用视口宽度的一半，必须大于等于 0 且小于视口宽度；可用于指定嵌套滚动区域。"`
	Y         *float64 `json:"y,omitempty" jsonschema_description:"滚轮事件起点的视口纵坐标，单位 CSS 像素；省略时使用视口高度的一半，必须大于等于 0 且小于视口高度；可用于指定嵌套滚动区域。"`
}

// ScrollResponse 表示滚轮事件已由浏览器接收。
type ScrollResponse struct {
	SessionID string  `json:"session_id"`
	PageID    string  `json:"page_id"`
	DeltaX    float64 `json:"delta_x"`
	DeltaY    float64 `json:"delta_y"`
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	// Snapshot 是操作完成后被操作页面的最新快照。
	Snapshot *SnapshotResponse `json:"snapshot,omitempty"`
	// SnapshotError 表示附加快照失败；操作已完成，只需重新读取页面，不应重放操作。
	SnapshotError string `json:"snapshot_error,omitempty"`
}

// WaitRequest 是 browserWait 的输入。
type WaitRequest struct {
	SessionID     string `json:"session_id" jsonschema:"required,minLength=1" jsonschema_description:"browserOpen 返回的 session_id。"`
	PageID        string `json:"page_id" jsonschema:"required,minLength=1" jsonschema_description:"目标 Tab 的 page_id。"`
	Condition     string `json:"condition" jsonschema:"required,enum=selector_visible,enum=selector_hidden,enum=url_contains,enum=title_contains,enum=text_contains,enum=load_complete" jsonschema_description:"等待条件：selector_visible/selector_hidden 等待 CSS selector 或快照 ref 可见/隐藏；url_contains、title_contains、text_contains 分别匹配 URL、标题或正文；load_complete 等待 document.readyState=complete。"`
	RefOrSelector string `json:"ref_or_selector" jsonschema:"maxLength=4096" jsonschema_description:"selector_visible/selector_hidden 时填写 CSS selector 或同一 page_id 的快照 ref；url/title/text_contains 时填写待匹配文本；load_complete 时可省略。ref 一旦离开 DOM 即视为 stale；异步重新挂载的元素请改用 CSS selector。"`
	Revision      uint64 `json:"revision,omitempty" jsonschema:"minimum=1" jsonschema_description:"使用快照 ref 时填写对应 revision（JSON 整数，不要加引号）；CSS selector 可省略。"`
	TimeoutMS     int    `json:"timeout_ms,omitempty" jsonschema:"minimum=1,maximum=600000" jsonschema_description:"等待超时毫秒数，可填 1 到 600000；省略时使用浏览器 OperationTimeout（默认 30 秒）；实际时限取此值、浏览器操作时限和宿主 context 中的较短者。"`
}

// WaitResponse 是 browserWait 的输出。
type WaitResponse struct {
	SessionID string `json:"session_id" jsonschema:"required"`
	PageID    string `json:"page_id" jsonschema:"required"`
	Condition string `json:"condition" jsonschema:"required"`
	Completed bool   `json:"completed" jsonschema:"required"`
}

// EvaluateRequest 是 browserEvaluate 的输入。
type EvaluateRequest struct {
	SessionID    string `json:"session_id" jsonschema:"required,minLength=1" jsonschema_description:"browserOpen 返回的 session_id。"`
	PageID       string `json:"page_id" jsonschema:"required,minLength=1" jsonschema_description:"目标 Tab 的 page_id。"`
	Code         string `json:"code" jsonschema:"required,minLength=1" jsonschema_description:"在目标页面 console 域执行的 JavaScript 表达式或语句，需宿主启用 allow_evaluate，并受 MaxScriptBytes 限制；返回值应可序列化为 JSON，且不超过 MaxResultBytes。"`
	TimeoutMS    int    `json:"timeout_ms,omitempty" jsonschema_description:"操作超时毫秒数；省略或为 0 时不增加请求级时限，仍受宿主 context 和浏览器 OperationTimeout（默认 30 秒）限制；填写正数时取这些时限中的较短者。"`
	AwaitPromise *bool  `json:"await_promise,omitempty" jsonschema_description:"是否等待返回的 Promise 完成；省略时默认为 true，false 时不等待。"`
}

// EvaluateResponse 是 browserEvaluate 的输出。
type EvaluateResponse struct {
	SessionID string          `json:"session_id"`
	PageID    string          `json:"page_id"`
	Type      string          `json:"type"`
	Value     json.RawMessage `json:"value"`
	// Dialogs 是执行期间出现、等待 browserHandleDialog 处理的 JavaScript 弹窗。
	Dialogs []Dialog `json:"dialogs,omitempty"`
}

// ScreenshotRequest 是 browserScreenshot 的输入。
type ScreenshotRequest struct {
	SessionID string `json:"session_id" jsonschema:"required,minLength=1" jsonschema_description:"browserOpen 返回的 session_id。"`
	PageID    string `json:"page_id" jsonschema:"required,minLength=1" jsonschema_description:"目标 Tab 的 page_id。"`
	FullPage  bool   `json:"full_page,omitempty" jsonschema_description:"是否截取完整可滚动页面；省略或为 false 时截取当前视口；不能与 region 同时启用。"`
	Grid      bool   `json:"grid,omitempty" jsonschema_description:"是否叠加 50px 网格和 100px 页面 CSS 坐标标签；用于视觉目标定位，不能与 full_page 同用。"`
	// Region 使用截图时的页面视口 CSS 坐标；省略时由 FullPage 决定截图范围。
	Region *ScreenshotRegion `json:"region,omitempty" jsonschema_description:"可选的视口裁剪区域；填写截图时的 CSS 坐标及视口尺寸，区域必须完全位于视口内，且 full_page 必须为 false。"`
}

// ScreenshotRegion 描述待截取的页面视口区域及当时的视口尺寸。
type ScreenshotRegion struct {
	X              float64 `json:"x" jsonschema_description:"裁剪区域左上角相对于视口左边缘的 CSS 像素坐标，必须大于等于 0。"`
	Y              float64 `json:"y" jsonschema_description:"裁剪区域左上角相对于视口上边缘的 CSS 像素坐标，必须大于等于 0。"`
	Width          float64 `json:"width" jsonschema_description:"裁剪区域宽度，单位 CSS 像素；必须大于 0，且 x + width 不超过 viewport_width。"`
	Height         float64 `json:"height" jsonschema_description:"裁剪区域高度，单位 CSS 像素；必须大于 0，且 y + height 不超过 viewport_height。"`
	ViewportWidth  float64 `json:"viewport_width" jsonschema_description:"截图时完整视口的 CSS 像素宽度，必须大于 0；用于把 CSS 坐标换算为截图像素坐标。"`
	ViewportHeight float64 `json:"viewport_height" jsonschema_description:"截图时完整视口的 CSS 像素高度，必须大于 0；用于把 CSS 坐标换算为截图像素坐标。"`
}

// ScreenshotResponse 是 browserScreenshot 的输出。
type ScreenshotResponse struct {
	// Data 保存完整截图，供宿主写入自己的存储。
	Data      []byte `json:"-"`
	SessionID string `json:"session_id"`
	PageID    string `json:"page_id"`
	Path      string `json:"path"`
	Format    string `json:"format"`
	Bytes     int    `json:"bytes"`
	// Width 和 Height 是图像像素尺寸。
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
	// ViewportWidth 和 ViewportHeight 是截图时页面视口的 CSS 像素尺寸。
	ViewportWidth  int    `json:"viewport_width,omitempty"`
	ViewportHeight int    `json:"viewport_height,omitempty"`
	Namespace      string `json:"namespace,omitempty"`
}

// CloseRequest 是 browserClose 的输入。
type CloseRequest struct {
	SessionID string `json:"session_id" jsonschema:"required,minLength=1" jsonschema_description:"要关闭的 browserOpen session_id；释放该 session 的全部页面和资源；外部浏览器只断开连接，已关闭的 session 可重复关闭。"`
}

// CloseResponse 是 browserClose 的输出。
type CloseResponse struct {
	SessionID string `json:"session_id"`
	Closed    bool   `json:"closed"`
}

// SetCheckedRequest 是 browserSetChecked 的输入。
type SetCheckedRequest struct {
	// SessionID 和 PageID 明确指定目标页面。
	SessionID string `json:"session_id" jsonschema:"required,minLength=1" jsonschema_description:"browserOpen 或 browserTabs 返回的 session_id。"`
	PageID    string `json:"page_id" jsonschema:"required,minLength=1" jsonschema_description:"目标 page_id，必须与元素引用来自同一快照。"`
	// Ref 和 Revision 定位同一快照中的控件。
	Ref      string `json:"ref" jsonschema:"required,minLength=1" jsonschema_description:"原生 checkbox/radio 或 ARIA checkbox/radio/switch 的元素引用。"`
	Revision uint64 `json:"revision" jsonschema:"required,minimum=1" jsonschema_description:"browserSnapshot 的 revision（JSON 整数，不要加引号）。"`
	// Checked 是目标勾选状态；radio 仅允许设为 true。
	Checked bool `json:"checked" jsonschema:"required" jsonschema_description:"目标勾选状态；已满足时不重复点击，radio 只允许设为 true。"`
}

// SetCheckedResponse 是 browserSetChecked 的输出。
type SetCheckedResponse struct {
	// SessionID、PageID 和 Ref 定位被操作的控件。
	SessionID string `json:"session_id"`
	PageID    string `json:"page_id"`
	Ref       string `json:"ref"`
	// Checked 是已确认的目标状态，Changed 表示本次是否发生点击。
	Checked bool `json:"checked"`
	Changed bool `json:"changed"`
	// ActivePageID、TabsRevision 和 TabsChanged 描述操作后的标签状态。
	ActivePageID string `json:"active_page_id,omitempty"`
	TabsRevision uint64 `json:"tabs_revision"`
	TabsChanged  bool   `json:"tabs_changed"`
	// Snapshot 是操作完成后被操作页面的最新快照。
	Snapshot *SnapshotResponse `json:"snapshot,omitempty"`
	// SnapshotError 表示附加快照失败；操作已完成，不应重放操作。
	SnapshotError string `json:"snapshot_error,omitempty"`
}

// DragRequest 是 browserDrag 的输入。
type DragRequest struct {
	// SessionID 和 PageID 明确指定目标页面。
	SessionID string `json:"session_id" jsonschema:"required,minLength=1" jsonschema_description:"browserOpen 或 browserTabs 返回的 session_id。"`
	PageID    string `json:"page_id" jsonschema:"required,minLength=1" jsonschema_description:"拖放所在的 page_id；源与目标必须属于同一页面。"`
	// SourceRef 来自当前页面快照；FromX/FromY 用于没有 ref 的视觉源点。
	SourceRef string `json:"source_ref,omitempty" jsonschema:"minLength=1" jsonschema_description:"拖拽源元素的快照 ref；与 from_x/from_y 二选一。"`
	// FromX 和 FromY 是视口 CSS 坐标，用于滑块手柄等没有快照 ref 的目标。
	FromX *float64 `json:"from_x,omitempty" jsonschema_description:"拖拽起点视口 CSS 横坐标；与 source_ref 二选一。"`
	FromY *float64 `json:"from_y,omitempty" jsonschema_description:"拖拽起点视口 CSS 纵坐标；与 source_ref 二选一。"`
	// TargetRef 可以是 ref 或唯一 selector；与 delta_x/delta_y 二选一。
	TargetRef string `json:"target_ref,omitempty" jsonschema:"minLength=1" jsonschema_description:"放置目标 ref 或唯一 CSS selector；与 delta_x/delta_y 二选一。"`
	// DeltaX 和 DeltaY 是从源点出发的视口 CSS 位移；用于滑动验证码等没有目标元素的场景。
	DeltaX   float64 `json:"delta_x,omitempty" jsonschema_description:"拖拽水平位移（CSS 像素）；与 target_ref 二选一。"`
	DeltaY   float64 `json:"delta_y,omitempty" jsonschema_description:"拖拽垂直位移（CSS 像素）；与 target_ref 二选一。"`
	Revision uint64  `json:"revision,omitempty" jsonschema:"minimum=1" jsonschema_description:"source_ref 的 browserSnapshot revision（JSON 整数，不要加引号）；使用 source_ref 时必填。"`
	// IncludeScreenshot 在拖拽后附带当前视口截图。
	IncludeScreenshot bool `json:"include_screenshot,omitempty" jsonschema_description:"拖拽后是否附带当前视口截图；用于确认视觉验证结果。"`
}

// DragResponse 是 browserDrag 的输出，表示拖放输入已经完成。
type DragResponse struct {
	// SessionID、PageID、SourceRef 和 TargetRef 定位本次拖放。
	SessionID string `json:"session_id"`
	PageID    string `json:"page_id"`
	SourceRef string `json:"source_ref"`
	TargetRef string `json:"target_ref"`
	// ActivePageID、TabsRevision 和 TabsChanged 描述操作后的标签状态。
	ActivePageID string `json:"active_page_id,omitempty"`
	TabsRevision uint64 `json:"tabs_revision"`
	TabsChanged  bool   `json:"tabs_changed"`
	// Snapshot 是操作完成后被操作页面的最新快照。
	Snapshot *SnapshotResponse `json:"snapshot,omitempty"`
	// SnapshotError 表示附加快照失败；操作已完成，不应重放操作。
	SnapshotError string `json:"snapshot_error,omitempty"`
	// DeltaX 和 DeltaY 是本次位移拖拽的视口 CSS 位移；元素拖拽时为零。
	DeltaX float64 `json:"delta_x,omitempty"`
	DeltaY float64 `json:"delta_y,omitempty"`
	// Screenshot 是 include_screenshot 请求的视口截图。
	Screenshot *ScreenshotResponse `json:"screenshot,omitempty"`
	// ScreenshotError 表示截图失败；拖拽已经完成，不应重放。
	ScreenshotError string `json:"screenshot_error,omitempty"`
}
