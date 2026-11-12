package browserkit

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Function 描述一个可由宿主封装成工具的浏览器函数。
// 请求与响应沿用根包的命名结构体，宿主可据此生成并校验 JSON Schema。
type Function struct {
	// Name 是模型调用时使用的稳定名称。
	Name string
	// Description 是供模型理解用途和约束的中文说明。
	Description string
	// Handler 的实际类型总是 func(context.Context, Request) (Response, error)。
	// Request 和 Response 均为导出的命名结构体；可交给宿主的函数工具构造器。
	Handler any
}

// Functions 返回绑定当前 Manager 的浏览器函数清单。
// 默认提供 26 个函数；AllowEvaluate 增加普通与序列脚本执行，AllowDevtools 增加三个诊断函数，UploadDir 增加文件上传。
// 每次返回独立切片，宿主可以筛选；同一 Manager 的操作与 HTTP/React 共用 session 和 Tab。
// 请求解码、输入/输出 schema 校验、审批、进度和模型协议由宿主工具框架处理。
func (manager *Manager) Functions() []Function {
	if manager == nil {
		return nil
	}
	functions := []Function{
		{Name: "browserOpen", Description: "启动浏览器 session 并打开 HTTP/HTTPS 页面；默认总计等待最多 3 秒加载并进入空闲，随后返回 session_id、page_id、当前 snapshot 和 load_status（loaded 或 timeout）；超时不要重复打开。需要图片时再调用 browserScreenshot。", Handler: manager.openFunction},
		{Name: "browserTabs", Description: "管理 Tab：action=list/activate/close/new/reload，省略为 list；activate/close/reload 需要 page_id。", Handler: manager.tabActionFunction},
		{Name: "browserNavigate", Description: "在已有页面打开 url，或用 direction=back/forward/reload 导航；url 与 direction 二选一。默认总计等待最多 3 秒加载并进入空闲，随后返回当前 snapshot 和 load_status；超时不要重复导航。需要图片时再调用 browserScreenshot。", Handler: manager.Navigate},
		{Name: "browserSnapshot", Description: "返回页面文本、控件状态和绑定 revision 的 ref；含 disabled/checked/value/readonly/id/placeholder 等，敏感值除外并按码点截断；跨域 iframe 的正文以 [frame URL] 标注，其控件会给出 f 前缀 ref；同站点与跨站 OOPIF 都支持。省略 page_id 用当前 Tab；max_chars/max_elements/offset 分页，下一页用 next_offset。视觉任务建议同时设置 include_screenshot=true 和 screenshot_grid=true，一次取得页面结构和带坐标的截图；截图已随结果返回，不要紧接着重复 browserScreenshot。ref 只在同一 page_id 和 revision 内有效。", Handler: manager.Snapshot},
		{Name: "browserHandleDialog", Description: "处理 browserSnapshot 返回的当前 JavaScript 弹窗；使用 dialogs[0] 的 id，并按 options 选择 accept 或 dismiss。prompt 接受时可填写 prompt_text。处理成功后页面才会继续执行。", Handler: manager.HandleDialog},
		{Name: "browserAct", Description: "按顺序执行一组页面动作：click/hover/type/select/check 用 ref，click 也可用 x/y，press 的 ref 可选，scroll/evaluate 不需要 ref，wait 用 ref_or_selector，drag 可用 source_ref/from_x+from_y 和 target_ref/delta_x+delta_y（各自二选一，坐标起点只能配位移）。只返回最终 snapshot，适合表单和动态等待。任一步失败即停止并返回 failed_step；附加等待超时会 warning，后续步骤可能运行在未稳定页面上，不要因此重放。整批非原子，跨页导航后重新 browserSnapshot。", Handler: manager.Act},
		{Name: "browserClick", Description: "点击快照元素或视口坐标：ref+revision 或 x/y 二选一；button 支持 left/right/middle，click_count 为 1 或 2。返回最新 snapshot；snapshot_error 表示附加读取失败或 wait_after_ms 被 timeout_ms 截断，操作已完成，不要重放。需要视觉结果设 include_screenshot=true，截图已随结果返回，不要重复截图；异步加载用 browserWait。", Handler: manager.Click},
		{Name: "browserHover", Description: "把真实指针移到快照元素，可展开悬停菜单；需要 ref 和 revision。返回最新 snapshot；snapshot_error 仅指附加读取失败，不要重放。", Handler: manager.Hover},
		{Name: "browserType", Description: "向快照元素输入文本，默认覆盖；append 追加，submit 发送 Enter。支持文本、number 和 date/month/week/time/datetime-local/color/range；非法或越界返回 invalid_value 并恢复原值。需要 ref 和 revision；返回最新 snapshot，snapshot_error 仅指附加读取失败，不要重放。", Handler: manager.Type},
		{Name: "browserSelectOption", Description: "按 value 或 label 选择原生单选下拉框；需要 ref 和 revision。返回最新 snapshot；snapshot_error 仅指附加读取失败，不要重放。", Handler: manager.SelectOption},
		{Name: "browserSetChecked", Description: "设置原生 checkbox/radio 或 ARIA checkbox/radio/switch 的 checked；radio 只能为 true，已是目标状态时不重复点击。需要 ref 和 revision；返回最新 snapshot，snapshot_error 仅指附加读取失败，不要重放。", Handler: manager.SetChecked},
		{Name: "browserDrag", Description: "拖拽：source_ref 或 from_x/from_y 指定起点，target_ref（ref 或唯一 CSS selector）或 delta_x/delta_y 指定目标，两组都只能二选一；from_x/from_y 只能配 delta_x/delta_y。没有 ref 的滑块先用 browserScreenshot 的 grid=true 读取起点和缺口坐标；include_screenshot=true 已含拖拽后截图，确认时不要重复截图。支持指针与 HTML5 拖放；include_screenshot=true 时附带截图。返回最新 snapshot；snapshot_error 仅指附加读取失败，不要重放。", Handler: manager.Drag},
		{Name: "browserPress", Description: "发送按键或组合键（modifiers：Control/Meta/Alt/Shift）；提供 ref 和 revision 时先聚焦，省略时用当前焦点。返回最新 snapshot；snapshot_error 仅指附加读取失败，不要重放。", Handler: manager.Press},
		{Name: "browserScroll", Description: "发送真实滚轮事件，delta_x/delta_y 为 CSS 像素；默认从视口中心开始，可用 x/y 指定嵌套区域。返回最新 snapshot；snapshot_error 仅指附加读取失败，不要重放。", Handler: manager.Scroll},
		{Name: "browserWait", Description: "等待页面状态：selector_visible/selector_hidden（ref 或 CSS selector）、url_contains、title_contains、text_contains、load_complete。", Handler: manager.Wait},
		{Name: "browserScreenshot", Description: "截取页面并保存为宿主可读取的 PNG；可指定 full_page、region 或 grid。grid 叠加 50px 网格和 100px 页面 CSS 坐标标签。", Handler: manager.Screenshot},
		{Name: "browserAssets", Description: "列出当前页已观测的资源（脚本、样式、图片、字体、XHR/Fetch）及状态、大小、耗时，可按 host 过滤；不会主动爬取。", Handler: manager.ListAssets},
		{Name: "browserClose", Description: "关闭 session 并释放页面；仅在用户要求或 session 无法继续时调用，任务完成后不要关闭用户正在观看的页面。", Handler: manager.CloseSession},
		{Name: "browserActivateTab", Description: "激活指定浏览器 Tab，让后续直播画面和输入切换到该 Tab。", Handler: manager.activateTabFunction},
		{Name: "browserCloseTab", Description: "关闭浏览器 session 中指定的网页 Tab。", Handler: manager.closeTabFunction},
		{Name: "sequenceStart", Description: "开始记录当前 session 中由宿主 AI 探索产生的操作候选；保存用户目标和变量默认值，并以当前页面生成首个导航候选。", Handler: manager.StartSequence},
		{Name: "sequenceRead", Description: "读取当前操作序列草稿的候选步骤、执行结果、页面别名和变量；模型应据此删除试错步骤并保持原执行顺序。", Handler: manager.ReadSequence},
		{Name: "sequenceProviderCall", Description: "使用宿主已取得的外部能力结果填写目标元素，并记录可在回放时暂停求值的 provider-call 候选。", Handler: manager.ExecuteSequenceProviderCall},
		{Name: "sequenceFinish", Description: "按候选 ID 删除和分组探索步骤，生成版本化可回放序列并保存为 JSON 文件；output_path 为空时创建临时文件，指定路径时拒绝覆盖已有文件，响应 path 始终为绝对路径。不能重排或改写候选动作内容。", Handler: manager.FinishSequence},
		{Name: "sequenceReplay", Description: "在全新本地临时 Chrome profile 中回放版本化操作序列；遇到 provider-call 时返回暂停状态和续播 token。", Handler: manager.ReplaySequence},
		{Name: "sequenceResume", Description: "向暂停的 provider-call 提交外部结果并从断点继续操作序列回放。", Handler: manager.ResumeSequence},
	}
	if !manager.config.AllowEvaluate {
		for index := range functions {
			if functions[index].Name == "browserAct" {
				functions[index].Description = strings.Replace(functions[index].Description, "scroll/evaluate 不需要 ref", "scroll 不需要 ref", 1)
			}
		}
	}
	if manager.config.AllowEvaluate {
		functions = append(functions, Function{Name: "browserEvaluate", Description: "在页面 console 执行 JavaScript 并返回有限 JSON；用表达式或 IIFE，不要写顶层 return 或顶层 await（await 需放进 async 函数），返回 Promise 会自动等待。", Handler: manager.Evaluate})
		functions = append(functions, Function{Name: "sequenceScript", Description: "显式执行并记录一个脚本候选；支持 action 和必须返回 true 的 assertion，只在启用 JavaScript 能力时提供。", Handler: manager.ExecuteSequenceScript})
	}
	if manager.config.UploadDir != "" {
		functions = append(functions, Function{Name: "browserUploadFile", Description: "把 UploadDir 内文件设置到 file input；需要 ref、revision 和绝对路径。返回最新 snapshot，snapshot_error 仅指附加读取失败，不要重放。", Handler: manager.UploadFile})
	}
	if manager.config.DownloadDir != "" {
		functions = append(functions, Function{Name: "browserDownload", Description: "点击链接/按钮并等待下载到 DownloadDir；返回文件名、路径和字节数。不要用它打开普通网页。", Handler: manager.Download})
	}
	if manager.config.AllowDevtools {
		functions = append(functions,
			Function{Name: "browserDevtoolsOpen", Description: "为指定 page_id 按需开启 Console、运行时异常和 Network 元数据采集；重复调用不会清空当前采集。", Handler: manager.OpenDevtools},
			Function{Name: "browserDevtoolsRead", Description: "读取指定 page_id 的有界开发诊断记录；支持 generation、after_sequence、kinds 和 limit 增量读取。", Handler: manager.ReadDevtools},
			Function{Name: "browserDevtoolsClose", Description: "停止指定 page_id 的开发诊断采集，并保留当前有界记录供最后读取。", Handler: manager.CloseDevtools},
		)
	}
	return functions
}

// 启动、导航和等待共用一次操作超时。
func (manager *Manager) openFunction(ctx context.Context, request OpenRequest) (OpenResponse, error) {
	if err := contextErr(ctx); err != nil {
		return OpenResponse{}, err
	}
	timeout := manager.config.Session.OperationTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return manager.Open(ctx, request)
}

// 宿主 UI 可以省略目标；模型函数必须明确 session，避免误操作另一个会话。
func (manager *Manager) tabActionFunction(ctx context.Context, request TabActionRequest) (TabActionResponse, error) {
	if err := contextErr(ctx); err != nil {
		return TabActionResponse{}, err
	}
	if request.SessionID == "" {
		return TabActionResponse{}, errors.New("browserTabs 需要 session_id")
	}
	action := strings.TrimSpace(request.Action)
	if action == "" {
		action = "list"
	}
	switch action {
	case "list":
		response, err := manager.ListTabs(ctx, TabsRequest{SessionID: request.SessionID})
		tabs := response.Tabs
		if tabs == nil {
			tabs = []TabView{}
		}
		return TabActionResponse{SessionID: response.SessionID, Action: action, ActivePageID: response.ActivePageID, TabsRevision: response.TabsRevision, Tabs: tabs}, err
	case "activate":
		if err := requirePage(request.SessionID, request.PageID, "browserTabs activate"); err != nil {
			return TabActionResponse{}, err
		}
		response, err := manager.ActivateTab(ctx, TabRequest{SessionID: request.SessionID, PageID: request.PageID})
		return TabActionResponse{SessionID: response.SessionID, Action: action, ActivePageID: response.ActivePageID, TabsRevision: response.TabsRevision, TabsChanged: response.TabsChanged, PageID: response.PageID, URL: response.URL, Title: response.Title, Tabs: []TabView{}}, err
	case "close":
		if err := requirePage(request.SessionID, request.PageID, "browserTabs close"); err != nil {
			return TabActionResponse{}, err
		}
		response, err := manager.CloseTab(ctx, TabRequest{SessionID: request.SessionID, PageID: request.PageID})
		return TabActionResponse{SessionID: response.SessionID, Action: action, ActivePageID: response.ActivePageID, TabsRevision: response.TabsRevision, TabsChanged: response.TabsChanged, PageID: response.PageID, Closed: response.Closed, Tabs: []TabView{}}, err
	case "new":
		response, err := manager.NewTab(ctx, OpenRequest{SessionID: request.SessionID, URL: request.URL})
		result := TabActionResponse{SessionID: response.SessionID, Action: action, PageID: response.PageID, URL: response.URL, Title: response.Title, LoadStatus: string(response.LoadStatus), Snapshot: response.Snapshot, Warning: response.Warning}
		if tabs, tabsErr := manager.ListTabs(ctx, TabsRequest{SessionID: request.SessionID}); tabsErr == nil {
			result.ActivePageID = tabs.ActivePageID
			result.TabsRevision = tabs.TabsRevision
			result.TabsChanged = true
			result.Tabs = tabs.Tabs
		}
		return result, err
	case "reload":
		if err := requirePage(request.SessionID, request.PageID, "browserTabs reload"); err != nil {
			return TabActionResponse{}, err
		}
		response, err := manager.Reload(ctx, TabRequest{SessionID: request.SessionID, PageID: request.PageID})
		return TabActionResponse{SessionID: response.SessionID, Action: action, ActivePageID: response.ActivePageID, TabsRevision: response.TabsRevision, TabsChanged: response.TabsChanged, PageID: response.PageID, URL: response.URL, Title: response.Title, LoadStatus: string(response.LoadStatus), Snapshot: response.Snapshot, Warning: response.Warning, Tabs: []TabView{}}, err
	default:
		return TabActionResponse{}, newError("invalid_action", "不支持的 Tab 动作", "使用 list、activate、close、new 或 reload", true)
	}
}

func (manager *Manager) activateTabFunction(ctx context.Context, request TabRequest) (TabResponse, error) {
	if err := contextErr(ctx); err != nil {
		return TabResponse{}, err
	}
	if err := requirePage(request.SessionID, request.PageID, "browserActivateTab"); err != nil {
		return TabResponse{}, err
	}
	return manager.ActivateTab(ctx, request)
}

func (manager *Manager) closeTabFunction(ctx context.Context, request TabRequest) (CloseTabResponse, error) {
	if err := contextErr(ctx); err != nil {
		return CloseTabResponse{}, err
	}
	if err := requirePage(request.SessionID, request.PageID, "browserCloseTab"); err != nil {
		return CloseTabResponse{}, err
	}
	return manager.CloseTab(ctx, request)
}
