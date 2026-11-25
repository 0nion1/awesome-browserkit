package browserkit

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Viewport 描述页面视口尺寸。
type Viewport struct {
	Width  int `json:"width" jsonschema_description:"视口宽度（CSS px），1-16384，宽高乘积不超过 67108864。"`
	Height int `json:"height" jsonschema_description:"视口高度（CSS px），1-16384，宽高乘积不超过 67108864。"`
}

const maxViewportDimension = 16_384
const maxViewportPixels = 64 * 1024 * 1024

// Validate 校验页面视口，避免异常尺寸造成浏览器截图和渲染资源失控。
func (viewport Viewport) Validate() error {
	if viewport.Width <= 0 || viewport.Height <= 0 {
		return errors.New("浏览器 viewport 无效")
	}
	if viewport.Width > maxViewportDimension || viewport.Height > maxViewportDimension ||
		int64(viewport.Width)*int64(viewport.Height) > maxViewportPixels {
		return errors.New("浏览器 viewport 超出允许范围")
	}
	return nil
}

// PageOptions 描述新页面选项。
type PageOptions struct {
	Viewport Viewport
	// InjectScript 会在后续每个 document 创建早期于主 world 执行。
	InjectScript string
}

// PageInfo 描述页面当前的公开状态。
type PageInfo struct {
	URL   string `json:"url"`
	Title string `json:"title"`
	// IsDownload 仅由 Navigate 返回，表示浏览器开始下载而没有替换当前文档。
	IsDownload bool `json:"is_download,omitempty"`
}

// Dialog 是页面当前弹窗或一次显式处理结果。
type Dialog struct {
	// ID 在同一页面内单调递增，browserHandleDialog 用它避免处理错弹窗。
	ID      uint64 `json:"id"`
	Type    string `json:"type"`
	Message string `json:"message,omitempty"`
	URL     string `json:"url,omitempty"`
	// Options 是当前弹窗允许的显式动作。
	Options  []string `json:"options"`
	Accepted bool     `json:"accepted"`
	// Handled 表示 browserHandleDialog 已成功处理该弹窗。
	Handled    bool   `json:"handled"`
	PromptText string `json:"prompt_text,omitempty"`
}

// DialogHandleOptions 描述一次明确的原生 JavaScript 弹窗处理。
type DialogHandleOptions struct {
	ID         uint64
	Action     string
	PromptText string
}

// DialogPage 暴露页面当前弹窗以及显式处理入口。
type DialogPage interface {
	Page
	PendingDialog() *Dialog
	DialogChanges() <-chan struct{}
	HandleDialog(context.Context, DialogHandleOptions) (Dialog, error)
}

// Asset 描述当前页面已经观测到的一项网络资源。
type Asset struct {
	Name           string  `json:"name"`
	URL            string  `json:"url"`
	InitiatorType  string  `json:"initiator_type,omitempty"`
	ResponseStatus int     `json:"response_status,omitempty"`
	TransferBytes  int64   `json:"transfer_bytes,omitempty"`
	EncodedBytes   int64   `json:"encoded_bytes,omitempty"`
	DecodedBytes   int64   `json:"decoded_bytes,omitempty"`
	DurationMS     float64 `json:"duration_ms,omitempty"`
}

// ListAssetsOptions 描述资源列表的过滤和数量限制。
type ListAssetsOptions struct {
	// Host 非空时只返回该主机的资源。
	Host string
	// Limit 限制返回资源数量，必须位于 1 到 500。
	Limit int
}

// ListAssetsResult 是页面资源的有界读取结果。
type ListAssetsResult struct {
	// Assets 按页面性能记录顺序返回最近的资源。
	Assets []Asset
	// Truncated 表示更早的资源因 Limit 未返回。
	Truncated bool
}

// AssetsPage 提供当前页面已观测资源的只读列表。
type AssetsPage interface {
	ListAssets(context.Context, ListAssetsOptions) (ListAssetsResult, error)
}

// ElementRef 是快照生成的、绑定 Page（即一个 Tab）及 document revision 的元素引用。
// 同一 document 内的局部 DOM 更新不会改变 revision；节点被替换或移除时，
// 点击执行阶段会根据仍存在的 data-browserkit-ref 判断该节点是否 stale。
type ElementRef struct {
	Ref      string `json:"ref"`
	Revision uint64 `json:"revision"`
}

// Element 描述一个可交互元素及用于核对写入的表单状态。
type Element struct {
	Ref      string `json:"ref"`
	Role     string `json:"role"`
	Name     string `json:"name,omitempty"`
	Type     string `json:"type,omitempty"`
	FrameURL string `json:"frame_url,omitempty"`
	// ID 是元素自身的 id；用于区分无障碍名称相同的元素。
	ID string `json:"id,omitempty"`
	// Placeholder 是输入框占位文本；有 label 时仍保留，便于定位未标注控件。
	Placeholder string `json:"placeholder,omitempty"`
	// Value 是表单控件或 contenteditable 的当前值（密码、掩码、文件等控件除外），
	// 按码点截断到 120 字符并受快照总量预算限制；用于确认输入是否生效。
	Value string `json:"value,omitempty"`
	// ValueTruncated 表示 Value 被截断或受总量预算限制。
	ValueTruncated bool `json:"value_truncated,omitempty"`
	// Checked 是可勾选控件的当前状态；其他元素省略。
	Checked *bool `json:"checked,omitempty"`
	// Indeterminate 表示原生或 ARIA 控件处于混合状态。
	Indeterminate bool `json:"indeterminate,omitempty"`
	// Disabled 表示控件当前禁用，不能交互。
	Disabled bool `json:"disabled,omitempty"`
	// ReadOnly 表示输入控件只读；browserType 不应把它报告为成功。
	ReadOnly bool `json:"readonly,omitempty"`
	// ContentEditable 表示元素是顶层 contenteditable 编辑区。
	ContentEditable bool `json:"contenteditable,omitempty"`
	// Multiline 表示文本编辑控件支持多行输入。
	Multiline bool `json:"multiline,omitempty"`
	// Pressed 是 ARIA pressed 状态；普通按钮省略。
	Pressed *bool `json:"pressed,omitempty"`
	// PressedMixed 表示 ARIA pressed 处于 mixed 状态。
	PressedMixed bool `json:"pressed_mixed,omitempty"`
	// Expanded 是可展开控件的当前展开状态。
	Expanded *bool `json:"expanded,omitempty"`
	// Draggable 表示元素支持原生 HTML5 拖放。
	Draggable        bool            `json:"draggable,omitempty"`
	Locator          ElementLocator  `json:"locator,omitempty"`
	Options          []ElementOption `json:"options,omitempty"`
	OptionsTruncated bool            `json:"options_truncated,omitempty"`
}

// ElementLocator 描述可跨页面重载重新解析的元素定位信息。
// Ref 仍用于当前 document 内的安全操作；Locator 只用于操作序列回放。
type ElementLocator struct {
	Selector      string `json:"selector,omitempty" jsonschema_description:"跨页面重新定位使用的 CSS selector。"`
	XPath         string `json:"xpath,omitempty" jsonschema_description:"优先使用的 XPath。"`
	AbsoluteXPath string `json:"absolute_xpath,omitempty" jsonschema_description:"CSS 与 XPath 失效时回退使用的绝对 XPath。"`
	Role          string `json:"role,omitempty" jsonschema_description:"元素的无障碍角色。"`
	Name          string `json:"name,omitempty" jsonschema_description:"用于核对目标元素的无障碍名称。"`
	Type          string `json:"type,omitempty" jsonschema_description:"用于核对目标元素的输入控件类型。"`
	FrameURL      string `json:"frame_url,omitempty" jsonschema_description:"元素所在 iframe 的 URL。"`
}

// ElementOption 是原生下拉框中可供选择的选项。
type ElementOption struct {
	Value    string `json:"value"`
	Label    string `json:"label"`
	Selected bool   `json:"selected,omitempty"`
}

// SelectOptionPage 提供对原生 select 控件的精确选项选择。
type SelectOptionPage interface {
	SelectOption(context.Context, ElementRef, string, string) (string, error)
}

// SelectOptionsPage 支持替换原生 select 的选中项，返回控件中的实际值。
type SelectOptionsPage interface {
	SelectOptions(ctx context.Context, ref ElementRef, matches []string, byLabel bool) ([]string, error)
}

// UploadFilePage 支持为原生文件输入控件设置一个本地文件。
type UploadFilePage interface {
	UploadFile(context.Context, ElementRef, string) error
}

// UploadFilesPage 支持为原生文件输入框设置文件，或点击元素触发文件选择器。
type UploadFilesPage interface {
	UploadFiles(ctx context.Context, ref ElementRef, paths []string, chooser bool) error
}

// SnapshotOptions 限制快照大小。
type SnapshotOptions struct {
	MaxChars    int
	MaxElements int
	// Offset 跳过前面的可交互候选元素，用于分页读取。
	Offset int
}

// FrameCoverage 描述快照包含或遗漏的文档。ShadowDOM 为 open_only，
// 明确快照只遍历开放 Shadow DOM，不覆盖闭合根。
type FrameCoverage struct {
	FrameID   string `json:"frame_id"`
	URL       string `json:"url,omitempty"`
	Status    string `json:"status"`
	Reason    string `json:"reason,omitempty"`
	ShadowDOM string `json:"shadow_dom"`
}

// Snapshot 是页面文本和可交互元素的结构化摘要。
type Snapshot struct {
	FrameCoverage []FrameCoverage `json:"frame_coverage,omitempty"`
	Info          PageInfo        `json:"info"`
	Revision      uint64          `json:"revision"`
	Text          string          `json:"text"`
	Elements      []Element       `json:"elements"`
	Truncated     bool            `json:"truncated"`
	// ActiveElementRef 和 ActiveElementValue 只供手动操作序列录制合并输入，
	// 不进入 Snapshot 的公开 JSON，避免把页面字段值暴露给普通快照调用方。
	ActiveElementRef   string `json:"-"`
	ActiveElementValue string `json:"-"`
	// HasMore 表示当前元素分页之后仍有可交互元素；仅用于计算 next_offset。
	HasMore bool `json:"has_more,omitempty"`
	// TextTruncated 表示正文因 max_chars 被截断，无法通过元素分页继续。
	TextTruncated bool `json:"-"`
	// PaginationKnown 表示 Page 实现已经显式给出 HasMore；自定义 Page 可省略以使用兼容推断。
	PaginationKnown bool `json:"-"`
	// ActiveRefs 是当前 DOM 中仍存在的内部 ref，供宿主回收已删除节点引用。
	ActiveRefs []string `json:"-"`
}

// ScreenshotOptions 描述截图选项。
type ScreenshotOptions struct {
	FullPage bool
}

// ScreencastOptions 描述 CDP 连续画面的编码参数。
type ScreencastOptions struct {
	Quality       int
	MaxWidth      int
	MaxHeight     int
	EveryNthFrame int
}

// Screenshot 是截图二进制结果。
type Screenshot struct {
	Data   []byte
	Width  int
	Height int
	Format string
}

// ScreencastPage 提供可选的 CDP 连续画面能力。
// 普通 Page 实现可以不支持；宿主应回退到定时截图。
type ScreencastPage interface {
	StartScreencast(context.Context, ScreencastOptions) (<-chan Screenshot, func(), error)
}

// InputEvent 是宿主通过 CDP 发送给真实浏览器页面的输入。
// 坐标和滚动距离均使用远程页面 viewport 的 CSS 像素。
// shortcut 类型只接受 select_all、copy、paste，Modifiers 可为 Ctrl(2)、Meta(4)
// 或由运行平台决定的零值。
type InputEvent struct {
	Kind           string            `json:"kind"`
	Action         string            `json:"action,omitempty"`
	X              float64           `json:"x,omitempty"`
	Y              float64           `json:"y,omitempty"`
	DeltaX         float64           `json:"delta_x,omitempty"`
	DeltaY         float64           `json:"delta_y,omitempty"`
	Button         string            `json:"button,omitempty"`
	Buttons        int               `json:"buttons,omitempty"`
	ClickCount     int               `json:"click_count,omitempty"`
	PointerType    string            `json:"pointer_type,omitempty"`
	Touches        []InputTouchPoint `json:"touches,omitempty"`
	Modifiers      int               `json:"modifiers,omitempty"`
	Key            string            `json:"key,omitempty"`
	Code           string            `json:"code,omitempty"`
	KeyCode        int               `json:"key_code,omitempty"`
	Location       int               `json:"location,omitempty"`
	AutoRepeat     bool              `json:"auto_repeat,omitempty"`
	Text           string            `json:"text,omitempty"`
	SelectionStart int               `json:"selection_start,omitempty"`
	SelectionEnd   int               `json:"selection_end,omitempty"`
}

// InputTouchPoint 描述一个活动触点，坐标使用远程 viewport 的 CSS 像素。
type InputTouchPoint struct {
	ID      int     `json:"id"`
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
	RadiusX float64 `json:"radius_x,omitempty"`
	RadiusY float64 `json:"radius_y,omitempty"`
	Force   float64 `json:"force,omitempty"`
}

// InputPage 提供基于 CDP Input 域的真实浏览器输入能力。
type InputPage interface {
	DispatchInput(context.Context, InputEvent) error
}

// HoverPage 提供基于真实指针坐标的元素悬停能力。
type HoverPage interface {
	Hover(context.Context, ElementRef) error
}

// EvaluateOptions 描述一次 console 域 JavaScript 执行。
type EvaluateOptions struct {
	AwaitPromise bool
	Timeout      time.Duration
}

// EvaluateResult 是只包含可 JSON 序列化值的 JavaScript 返回值。
type EvaluateResult struct {
	Value json.RawMessage `json:"value,omitempty"`
	Type  string          `json:"type"`
}

// ClickOptions 描述元素点击的鼠标按键和次数。
type ClickOptions struct {
	// Button 允许 left、right、middle；空值使用 left。
	Button string
	// ClickCount 允许 1 或 2；零值使用 1。
	ClickCount int
}

// PointClickPage 支持按视口 CSS 坐标点击没有快照 ref 的视觉目标。
type PointClickPage interface {
	ClickPoint(context.Context, float64, float64, ClickOptions) error
}

// PressOptions 描述一次完整按键或组合键。
type PressOptions struct {
	// Key 是字母、数字或编辑导航键。
	Key string
	// Modifiers 允许 Control、Meta、Alt、Shift，不允许重复。
	Modifiers []string
	// Ref 非空时先聚焦目标元素，否则使用当前焦点。
	Ref *ElementRef
}

// Page 是浏览器页面的最小稳定操作接口。
type Page interface {
	Info(context.Context) (PageInfo, error)
	Navigate(context.Context, string) (PageInfo, error)
	Snapshot(context.Context, SnapshotOptions) (Snapshot, error)
	Click(context.Context, ElementRef, ClickOptions) error
	Type(context.Context, ElementRef, string, bool) error
	Press(context.Context, PressOptions) error
	// SetChecked 确认目标勾选状态，返回是否发生点击。
	SetChecked(context.Context, ElementRef, bool) (bool, error)
	// Drag 在同一页面内完成元素到元素的拖放。
	Drag(context.Context, ElementRef, ElementRef) error
	Wait(context.Context, WaitOptions) error
	// WaitIdle 等待页面下一次 requestIdleCallback，超时由调用方控制。
	WaitIdle(context.Context, time.Duration) error
	Screenshot(context.Context, ScreenshotOptions) (Screenshot, error)
	Evaluate(context.Context, string, EvaluateOptions) (EvaluateResult, error)
	AddScriptToEvaluateOnNewDocument(context.Context, string) error
	Close(context.Context) error
}

// TargetPage 是能提供底层 target 身份的 Page。身份只供 Browser Manager
// 绑定已有 Tab 使用，不应进入模型或 Web 协议。
type TargetPage interface {
	Page
	TargetID() string
}

// DownloadPage 可尝试一次触发下载的原生点击。点击不能证明下载事件的因果归属，
// 因此返回 Triggered 和 download_unconfirmed，不返回猜测的下载身份或文件。
// dir 必须使用 session 配置的 DownloadDir。
type DownloadPage interface {
	Page
	Download(context.Context, ElementRef, string) (DownloadInfo, error)
}

// DragTargetPage 允许拖放目标使用 CSS selector；源元素仍使用快照 ref。
type DragTargetPage interface {
	Page
	DragTo(context.Context, ElementRef, string) error
}

// DragByPage 允许从快照元素按视口 CSS 位移拖拽。
type DragByPage interface {
	Page
	DragBy(context.Context, ElementRef, float64, float64) error
}

// DragPointPage 允许从视口 CSS 坐标按位移拖拽。
type DragPointPage interface {
	Page
	DragPoint(context.Context, float64, float64, float64, float64) error
}

// RefAwarePage 能判断快照 ref 是否仍存在于当前 DOM。
// wait 在 selector_visible 前用它区分“元素尚未出现”和“ref 已失效”。
type RefAwarePage interface {
	Page
	HasRef(context.Context, string) (bool, error)
}

// HistoryEntry 描述一个可供导航的浏览器历史项。
type HistoryEntry struct {
	ID  int
	URL string

	// fromIndex/fromURL 仅供内置 Page 判断导航是否真正提交；外部实现可忽略。
	fromIndex int
	fromURL   string
}

// HistoryPage 支持浏览器历史前进和后退。
type HistoryPage interface {
	Page
	// HistoryTarget 返回 back 或 forward 对应的历史项，供宿主做地址策略校验。
	HistoryTarget(context.Context, string) (HistoryEntry, error)
	// NavigateHistory 导航到指定历史项，返回导航后的页面信息。
	NavigateHistory(context.Context, HistoryEntry) (PageInfo, error)
}

// DownloadSession 提供会话内自动下载的查询、续等和真正取消能力。
// WaitDownload 受 OperationTimeout 和 ctx 限制；超时保留下载及部分事实。
type DownloadSession interface {
	Session
	Downloads(context.Context) ([]DownloadInfo, error)
	DownloadStatus(context.Context, string) (DownloadInfo, error)
	WaitDownload(context.Context, string) (DownloadInfo, error)
	CancelDownload(context.Context, string) (DownloadInfo, error)
}

// DownloadInfo 描述下载事实或一次触发回执。只有 completed 带完整文件引用。
type DownloadInfo struct {
	// Triggered 仅用于 Download 触发回执：true 表示点击已确认完成；false 不证明未执行。
	Triggered  bool   `json:"triggered,omitempty"`
	ID         string `json:"download_id"`
	State      string `json:"state"`
	FrameID    string `json:"frame_id"`
	TargetID   string `json:"target_id,omitempty"`
	URL        string `json:"url"`
	Filename   string `json:"file_name,omitempty"`
	Path       string `json:"path,omitempty"`
	Bytes      int64  `json:"bytes"`
	TotalBytes int64  `json:"total_bytes"`
	Error      string `json:"error,omitempty"`
}

// WaitOptions 描述浏览器页面的有限等待条件。
type WaitOptions struct {
	Condition string
	Selector  string
}
