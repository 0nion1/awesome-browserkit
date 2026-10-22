package browserkit

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Viewport 描述页面视口尺寸。
type Viewport struct {
	Width  int `json:"width"`
	Height int `json:"height"`
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

// Element 描述一个可交互元素，不包含字段值。
type Element struct {
	Ref              string          `json:"ref"`
	Role             string          `json:"role"`
	Name             string          `json:"name,omitempty"`
	Type             string          `json:"type,omitempty"`
	FrameURL         string          `json:"frame_url,omitempty"`
	Locator          ElementLocator  `json:"locator,omitempty"`
	Options          []ElementOption `json:"options,omitempty"`
	OptionsTruncated bool            `json:"options_truncated,omitempty"`
}

// ElementLocator 描述可跨页面重载重新解析的元素定位信息。
// Ref 仍用于当前 document 内的安全操作；Locator 只用于操作序列回放。
type ElementLocator struct {
	Selector      string `json:"selector,omitempty"`
	XPath         string `json:"xpath,omitempty"`
	AbsoluteXPath string `json:"absolute_xpath,omitempty"`
	Role          string `json:"role,omitempty"`
	Name          string `json:"name,omitempty"`
	Type          string `json:"type,omitempty"`
	FrameURL      string `json:"frame_url,omitempty"`
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

// UploadFilePage 支持为原生文件输入控件设置一个本地文件。
type UploadFilePage interface {
	UploadFile(context.Context, ElementRef, string) error
}

// SnapshotOptions 限制快照大小。
type SnapshotOptions struct {
	MaxChars    int
	MaxElements int
	// Offset 跳过前面的可交互候选元素，用于分页读取。
	Offset int
}

// Snapshot 是页面文本和可交互元素的结构化摘要。
type Snapshot struct {
	Info      PageInfo  `json:"info"`
	Revision  uint64    `json:"revision"`
	Text      string    `json:"text"`
	Elements  []Element `json:"elements"`
	Truncated bool      `json:"truncated"`
	// ActiveElementRef 和 ActiveElementValue 只供手动操作序列录制合并输入，
	// 不进入 Snapshot 的公开 JSON，避免把页面字段值暴露给普通快照调用方。
	ActiveElementRef   string `json:"-"`
	ActiveElementValue string `json:"-"`
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

// Page 是浏览器页面的最小稳定操作接口。
type Page interface {
	Info(context.Context) (PageInfo, error)
	Navigate(context.Context, string) (PageInfo, error)
	Snapshot(context.Context, SnapshotOptions) (Snapshot, error)
	Click(context.Context, ElementRef) error
	Type(context.Context, ElementRef, string, bool) error
	Press(context.Context, string) error
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

// WaitOptions 描述浏览器页面的有限等待条件。
type WaitOptions struct {
	Condition string
	Selector  string
}
