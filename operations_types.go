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
}

// OpenLoadStatus 是 browserOpen 完成页面空闲等待后的状态。
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

// TabsResponse 是 browserTabs 的输出。
type TabsResponse struct {
	SessionID    string    `json:"session_id"`
	ActivePageID string    `json:"active_page_id,omitempty"`
	TabsRevision uint64    `json:"tabs_revision"`
	Tabs         []TabView `json:"tabs"`
}

// TabsRequest 是 browserTabs 的输入。
type TabsRequest struct {
	SessionID string `json:"session_id" jsonschema_description:"浏览器 session 标识；原样使用 browserOpen 或 browserTabs 返回的 session_id。"`
}

// TabRequest 是 browserActivateTab 的输入。
type TabRequest struct {
	SessionID string `json:"session_id" jsonschema_description:"浏览器 session 标识；原样使用 browserOpen 或 browserTabs 返回的 session_id。"`
	PageID    string `json:"page_id" jsonschema_description:"目标网页 Tab 标识；原样使用 browserOpen、browserTabs 或 browserSnapshot 返回的 page_id。"`
}

// TabResponse 是 browserActivateTab 的输出。
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

// CloseTabResponse 是 browserCloseTab 的输出。
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
	SessionID string `json:"session_id" jsonschema_description:"浏览器 session 标识；原样使用 browserOpen 或 browserTabs 返回的 session_id。留空时使用当前活动 session。"`
	PageID    string `json:"page_id" jsonschema_description:"目标网页 Tab 标识；原样使用 browserOpen、browserTabs 或 browserSnapshot 返回的 page_id。"`
	URL       string `json:"url" jsonschema_description:"目标 HTTP/HTTPS 地址，也支持相对于该 Tab 当前地址的 URL；导航后原有快照引用失效。"`
}

// NavigateResponse 是 browserNavigate 的输出。
type NavigateResponse struct {
	SessionID    string `json:"session_id"`
	PageID       string `json:"page_id"`
	URL          string `json:"url"`
	Title        string `json:"title,omitempty"`
	ActivePageID string `json:"active_page_id,omitempty"`
	TabsRevision uint64 `json:"tabs_revision"`
	TabsChanged  bool   `json:"tabs_changed"`
}

// SnapshotRequest 是 browserSnapshot 的输入。
type SnapshotRequest struct {
	SessionID   string `json:"session_id" jsonschema:"required,minLength=1" jsonschema_description:"浏览器 session 标识；原样使用 browserOpen 或 browserTabs 返回的 session_id。"`
	PageID      string `json:"page_id,omitempty" jsonschema_description:"可选的目标 page_id；省略时先同步 Tab 状态，再使用当前激活 Tab。"`
	MaxChars    int    `json:"max_chars,omitempty" jsonschema:"minimum=1,maximum=1048576" jsonschema_description:"页面正文最多返回的字符数；省略时默认 12000，最大 1048576，在浏览器内截断。"`
	MaxElements int    `json:"max_elements,omitempty" jsonschema:"minimum=1,maximum=1000" jsonschema_description:"本页最多返回的可交互元素数量；省略时默认 100，最大 1000。"`
	Offset      int    `json:"offset,omitempty" jsonschema:"minimum=0,maximum=1000000000" jsonschema_description:"跳过的可交互元素数量；省略时从 0 开始，使用上一页返回的 next_offset 继续读取；最大 1000000000。"`
}

// SnapshotResponse 是 browserSnapshot 的输出；elements 中的 ref 只属于 page_id。
// tabs_changed 表示相对上一次 Snapshot 的 Tab 投影是否变化；首次 Snapshot 为 true。
type SnapshotResponse struct {
	SessionID    string    `json:"session_id"`
	PageID       string    `json:"page_id"`
	Revision     uint64    `json:"revision"`
	URL          string    `json:"url"`
	Title        string    `json:"title,omitempty"`
	Text         string    `json:"text"`
	Elements     []Element `json:"elements"`
	Truncated    bool      `json:"truncated"`
	NextOffset   int       `json:"next_offset"`
	ActivePageID string    `json:"active_page_id,omitempty"`
	TabsRevision uint64    `json:"tabs_revision"`
	TabsChanged  bool      `json:"tabs_changed"`
	Tabs         []TabView `json:"tabs"`
}

// ClickRequest 是 browserClick 的输入。
type ClickRequest struct {
	SessionID   string `json:"session_id" jsonschema_description:"浏览器 session 标识；原样使用 browserOpen 或 browserTabs 返回的 session_id。"`
	PageID      string `json:"page_id" jsonschema_description:"目标网页 Tab 标识；原样使用 browserOpen、browserTabs 或 browserSnapshot 返回的 page_id。"`
	Ref         string `json:"ref" jsonschema_description:"目标元素引用；使用目标 page_id 的 browserSnapshot 返回的 ref，不能跨 Tab 使用。"`
	Revision    uint64 `json:"revision" jsonschema_description:"生成 ref 的同一次 browserSnapshot 返回的 revision，必须大于 0；引用失效时重新获取快照。"`
	WaitAfterMS int    `json:"wait_after_ms,omitempty" jsonschema_description:"点击成功后额外等待的毫秒数；省略或为 0 时不额外等待，不能为负数，等待计入 timeout_ms 的请求时限。"`
	TimeoutMS   int    `json:"timeout_ms,omitempty" jsonschema_description:"操作超时毫秒数；省略或为 0 时不增加请求级时限，仍受宿主 context 和浏览器 OperationTimeout（默认 30 秒）限制；填写正数时取这些时限中的较短者。"`
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
}

// HoverRequest 是 browserHover 的输入。
type HoverRequest struct {
	SessionID string `json:"session_id" jsonschema_description:"浏览器 session 标识；原样使用 browserOpen 或 browserTabs 返回的 session_id。"`
	PageID    string `json:"page_id" jsonschema_description:"目标网页 Tab 标识；原样使用 browserOpen、browserTabs 或 browserSnapshot 返回的 page_id。"`
	Ref       string `json:"ref" jsonschema_description:"目标元素引用；使用目标 page_id 的 browserSnapshot 返回的 ref，不能跨 Tab 使用。"`
	Revision  uint64 `json:"revision" jsonschema_description:"生成 ref 的同一次 browserSnapshot 返回的 revision，必须大于 0；引用失效时重新获取快照。"`
}

// HoverResponse 是 browserHover 的输出。内部鼠标轨迹不作为响应内容暴露。
type HoverResponse struct {
	SessionID    string `json:"session_id"`
	PageID       string `json:"page_id"`
	Ref          string `json:"ref"`
	ActivePageID string `json:"active_page_id,omitempty"`
	TabsRevision uint64 `json:"tabs_revision"`
	TabsChanged  bool   `json:"tabs_changed"`
}

// TypeRequest 是 browserType 的输入。text 只用于执行，不出现在响应中。
type TypeRequest struct {
	SessionID string `json:"session_id" jsonschema_description:"浏览器 session 标识；原样使用 browserOpen 或 browserTabs 返回的 session_id。"`
	PageID    string `json:"page_id" jsonschema_description:"目标网页 Tab 标识；原样使用 browserOpen、browserTabs 或 browserSnapshot 返回的 page_id。"`
	Ref       string `json:"ref" jsonschema_description:"目标元素引用；使用目标 page_id 的 browserSnapshot 返回的 ref，不能跨 Tab 使用。"`
	Revision  uint64 `json:"revision" jsonschema_description:"生成 ref 的同一次 browserSnapshot 返回的 revision，必须大于 0；引用失效时重新获取快照。"`
	Text      string `json:"text" jsonschema_description:"要输入的文本，可以为空；默认先清空原值，append=true 时追加；响应不会回显输入内容。"`
	Append    bool   `json:"append,omitempty" jsonschema_description:"是否把文本追加到元素当前值末尾；省略或为 false 时先清空当前值。"`
	Submit    bool   `json:"submit,omitempty" jsonschema_description:"输入完成后是否向页面发送 Enter；省略或为 false 时只输入文本。"`
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
}

// SelectOptionRequest 是 browserSelectOption 的输入。value 和 label 二选一。
type SelectOptionRequest struct {
	SessionID string `json:"session_id" jsonschema_description:"浏览器 session 标识；原样使用 browserOpen 或 browserTabs 返回的 session_id。"`
	PageID    string `json:"page_id" jsonschema_description:"目标网页 Tab 标识；原样使用 browserOpen、browserTabs 或 browserSnapshot 返回的 page_id。"`
	Ref       string `json:"ref" jsonschema_description:"目标元素引用；使用目标 page_id 的 browserSnapshot 返回的 ref，不能跨 Tab 使用。"`
	Revision  uint64 `json:"revision" jsonschema_description:"生成 ref 的同一次 browserSnapshot 返回的 revision，必须大于 0；引用失效时重新获取快照。"`
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
}

// UploadFileRequest 是 browserUploadFile 的输入。
type UploadFileRequest struct {
	SessionID string `json:"session_id" jsonschema_description:"浏览器 session 标识；原样使用 browserOpen 或 browserTabs 返回的 session_id。"`
	PageID    string `json:"page_id" jsonschema_description:"目标网页 Tab 标识；原样使用 browserOpen、browserTabs 或 browserSnapshot 返回的 page_id。"`
	Ref       string `json:"ref" jsonschema_description:"目标原生 file input 的快照 ref；必须来自同一 page_id 的 browserSnapshot。"`
	Revision  uint64 `json:"revision" jsonschema_description:"生成 ref 的同一次 browserSnapshot 返回的 revision，必须大于 0；引用失效时重新获取快照。"`
	Path      string `json:"path" jsonschema_description:"浏览器宿主机器上的绝对文件路径；解析符号链接后必须位于宿主授权的 UploadDir 内，文件必须是大小不超过 10 MiB 的普通文件。"`
}

// UploadFileResponse 只返回文件名，不回显宿主路径或内容。
type UploadFileResponse struct {
	SessionID string `json:"session_id"`
	PageID    string `json:"page_id"`
	Ref       string `json:"ref"`
	FileName  string `json:"file_name"`
	Bytes     int64  `json:"bytes"`
}

// PressRequest 是 browserPress 的输入。
type PressRequest struct {
	SessionID string `json:"session_id" jsonschema_description:"浏览器 session 标识；原样使用 browserOpen 或 browserTabs 返回的 session_id。"`
	PageID    string `json:"page_id" jsonschema_description:"目标网页 Tab 标识；原样使用 browserOpen、browserTabs 或 browserSnapshot 返回的 page_id。"`
	Key       string `json:"key" jsonschema_description:"发送到页面当前焦点的按键；仅支持 Enter、Tab、Escape、Backspace、ArrowUp、ArrowDown、ArrowLeft、ArrowRight、Space，名称区分大小写。"`
}

// PressResponse 是 browserPress 的输出。
type PressResponse struct {
	SessionID    string `json:"session_id"`
	PageID       string `json:"page_id"`
	Key          string `json:"key"`
	ActivePageID string `json:"active_page_id,omitempty"`
	TabsRevision uint64 `json:"tabs_revision"`
	TabsChanged  bool   `json:"tabs_changed"`
}

// ScrollRequest 是 browserScroll 的输入，距离及坐标均为视口 CSS 像素。
type ScrollRequest struct {
	SessionID string   `json:"session_id" jsonschema_description:"浏览器 session 标识；原样使用 browserOpen 或 browserTabs 返回的 session_id。"`
	PageID    string   `json:"page_id" jsonschema_description:"目标网页 Tab 标识；原样使用 browserOpen、browserTabs 或 browserSnapshot 返回的 page_id。"`
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
}

// WaitRequest 是 browserWait 的输入。
type WaitRequest struct {
	SessionID     string `json:"session_id" jsonschema:"required,minLength=1" jsonschema_description:"浏览器 session 标识；原样使用 browserOpen 或 browserTabs 返回的 session_id。"`
	PageID        string `json:"page_id" jsonschema:"required,minLength=1" jsonschema_description:"目标网页 Tab 标识；原样使用 browserOpen、browserTabs 或 browserSnapshot 返回的 page_id。"`
	Condition     string `json:"condition" jsonschema:"required,enum=selector_visible,enum=selector_hidden,enum=url_contains,enum=title_contains,enum=text_contains,enum=load_complete" jsonschema_description:"等待条件：selector_visible/selector_hidden 等待 CSS selector 或快照 ref 可见/隐藏；url_contains、title_contains、text_contains 分别匹配 URL、标题或正文；load_complete 等待 document.readyState=complete。"`
	RefOrSelector string `json:"ref_or_selector" jsonschema:"maxLength=4096" jsonschema_description:"selector_visible/selector_hidden 时填写 CSS selector 或同一 page_id 的快照 ref；url/title/text_contains 时填写待匹配文本；load_complete 时可省略。"`
	Revision      uint64 `json:"revision,omitempty" jsonschema:"minimum=1" jsonschema_description:"仅在使用快照 ref 等待时可填写对应 browserSnapshot 的 revision，用于检查引用过期；使用 CSS selector 时省略。"`
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
	SessionID    string `json:"session_id" jsonschema_description:"浏览器 session 标识；原样使用 browserOpen 或 browserTabs 返回的 session_id。"`
	PageID       string `json:"page_id" jsonschema_description:"目标网页 Tab 标识；原样使用 browserOpen、browserTabs 或 browserSnapshot 返回的 page_id。"`
	Code         string `json:"code" jsonschema_description:"在目标页面 console 域执行的 JavaScript 表达式或语句，需宿主启用 allow_evaluate，并受 MaxScriptBytes 限制；返回值应可序列化为 JSON，且不超过 MaxResultBytes。"`
	TimeoutMS    int    `json:"timeout_ms,omitempty" jsonschema_description:"操作超时毫秒数；省略或为 0 时不增加请求级时限，仍受宿主 context 和浏览器 OperationTimeout（默认 30 秒）限制；填写正数时取这些时限中的较短者。"`
	AwaitPromise *bool  `json:"await_promise,omitempty" jsonschema_description:"是否等待返回的 Promise 完成；省略时默认为 true，false 时不等待。"`
}

// EvaluateResponse 是 browserEvaluate 的输出。
type EvaluateResponse struct {
	SessionID string          `json:"session_id"`
	PageID    string          `json:"page_id"`
	Type      string          `json:"type"`
	Value     json.RawMessage `json:"value"`
}

// ScreenshotRequest 是 browserScreenshot 的输入。
type ScreenshotRequest struct {
	SessionID string `json:"session_id" jsonschema_description:"浏览器 session 标识；原样使用 browserOpen 或 browserTabs 返回的 session_id。"`
	PageID    string `json:"page_id" jsonschema_description:"目标网页 Tab 标识；原样使用 browserOpen、browserTabs 或 browserSnapshot 返回的 page_id。"`
	FullPage  bool   `json:"full_page,omitempty" jsonschema_description:"是否截取完整可滚动页面；省略或为 false 时截取当前视口；不能与 region 同时启用。"`
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
	Namespace string `json:"namespace,omitempty"`
}

// CloseRequest 是 browserClose 的输入。
type CloseRequest struct {
	SessionID string `json:"session_id" jsonschema_description:"要关闭的 browserOpen session_id；释放该 session 的全部页面和资源；外部浏览器只断开连接，已关闭的 session 可重复关闭。"`
}

// CloseResponse 是 browserClose 的输出。
type CloseResponse struct {
	SessionID string `json:"session_id"`
	Closed    bool   `json:"closed"`
}
