# keycode 浏览器增强迁移

本次以 keycode 提交 `9f5c208c7f06e701c15cddf044ed97665203ea7d` 为来源，
回迁 `plugin/browser/sdk` 与 `plugin/browser/frontend/view` 的浏览器能力。
keycode 同步的原始 BrowserKit 基线为 `1d22c4ff7e0981c00ccbf756ceb704e1ad57d7b5`。
合并按该基线做三方比较，保留本项目的独立 HTTP 服务、CLI、React 包与序列功能。
原生依赖版本不变，运行时不依赖 keycode、Agent、插件加载器或宿主 UI。

## 2026-11-24 复核

重新 fetch keycode `origin/main`，当前提交为 `e31e178c1ed69fc378f1cc1c129294d65bb9c00e`。
该提交的整个 `plugin/browser` Git tree 与上次来源 `9f5c208c` 相同，
tree ID 均为 `8057db0deaf0f8339f7b31cd21e66e26f67d8191`。
因此 main 合入后的浏览器代码没有新增待回迁修改。

逐文件检查 SDK，转换包名和测试导入后，原有 70 个 Go 文件中 55 个完全相同，
其余 15 个差异为 BrowserKit 保留的序列录制回放、定位信息、独立宿主适配和测试适配。
iframe/OOPIF 实现 `page_frame.go`、批量动作 `operations_batch.go`、
弹窗接口 `operations_dialog.go`、页面动作 `page_actions.go` 等文件与 keycode main 一致。
主文档快照 `page.go` 的差异只增加序列定位器及内部焦点采样，
保留 keycode 的 open Shadow DOM、同源 iframe 遍历和原生弹窗处理逻辑。

新增真实 Chrome 组合回归 `TestRealBrowserNestedShadowDialog`：
分别在主文档、同源 iframe 中创建两层 open Shadow DOM，
通过快照 ref 输入文本、点击按钮、读取待处理 prompt，
再用 `HandleDialog` 提交文本，并断言 Shadow DOM 正文中的最终结果。
加载中弹窗的时间测试先建立浏览器 session，避免将 Chrome 冷启动耗时误判为弹窗等待。

此处原迁移基线的限制已由宿主 SDK 增强替换：跨域 frame 复用主文档的开放
Shadow DOM 采集脚本，嵌套 OOPIF 沿父 session 链操作，不再限制为 3 个 frame。
当前实现、覆盖回执与验证边界见 [frame 说明](frame-tool-design.md)。

迁移提交推送在 `origin/feature/keycode-browser-improvements`。
复核时 `origin/main` 为 `ec8605b`，`github/main` 为 `e31b3fa`，
两者均未包含迁移提交 `f07329c`；直接使用这些 main 分支仍然缺少本次增强。

## 页面操作

- `browserAct` 顺序执行最多 20 个动作，只在批次结束时读取最终快照。
  失败返回已完成步骤与 `failed_step`，不回滚或重放已经执行的动作。
- 单独动作附带快照，自动快照正文保留最多约 2000 个码点的首尾预览；
  完整正文仍使用 `browserSnapshot`。
  `snapshot_error` 表示动作已完成，但附加观察或等待失败。
- 支持坐标点击、左右键、双击、带焦点与修饰键的按键、勾选状态、指针与 HTML5 拖放。
  特化输入支持 date、month、week、time、datetime-local、color 和 range，非法值不报告为成功。
- `browserNavigate` 支持 `direction=back/forward/reload`。
  打开与导航默认共用最多 3 秒加载和空闲等待，超时返回当前可读快照，不自动重复导航。
- `browserTabs` 支持 `action=list/activate/close/new/reload`，省略 action 仍列出 Tab。
  `browserActivateTab`、`browserCloseTab` 和全部 `sequence*` 工具继续提供。
- alert、confirm、prompt 与 beforeunload 通过 `snapshot.dialogs` 暴露。
  使用 `browserHandleDialog` 和当前弹窗 ID 明确处理；不默认替用户确认。
- 设置 `Config.DownloadDir` 后提供 `browserDownload`。
  完成状态来自 CDP 下载事件；下载超时不表示点击未发生。
  本地目录只在下载期间允许，不支持远程 CDP 会话的本地下载目录。
  CLI 使用 `--download-dir`，上传使用 `--upload-dir`。

## 快照与 frame

正文收集达到预算后停止，元素分页明确报告是否还有下一页。
快照包含控件的 id、placeholder、value、checked、disabled、readonly、pressed、expanded 等状态。
值按码点和总预算截断，密码、掩码、文件及敏感 autocomplete 字段不输出值。
隐藏 iframe 宿主的正文和值也会被排除。

快照按整个 Tab 的文字/元素预算聚合嵌套 iframe/OOPIF 与开放 Shadow DOM，
使用 `frame_coverage` 说明未覆盖范围，元素可按 `next_offset` 继续读取。
frame 元素使用包含文档身份的独立 `f` 前缀 ref。
点击、悬停、输入、下拉、勾选、按键、拖放、上传和等待可路由到 frame。
仿射旋转、缩放、镜像和 skew 使用 CDP quad 映射；透视、退化几何及无法核实
命中点的遮挡/裁剪返回明确错误。闭合 Shadow DOM 不在快照及 ref 操作范围。

快照与部分动作可使用 `include_screenshot`。
快照的 `screenshot_grid` 或截图的 `grid` 提供 CSS 坐标网格；grid 不与 full_page 同用。
整页截图在成功或失败后均尝试恢复滚动位置。

## 实时交互

服务端通过 target 事件同步标签，连接失效时回收会话并通知观察者。
指针在浏览器所在端插值，批量输入保留顺序和节奏，离散输入作为顺序屏障。
新增 `DispatchViewerInputs` 与 `WaitViewerInput` 供自定义传输使用。
默认 HTTP 数据源保留独立 WebSocket 输入，并在拥塞时只缓存最后一个 mouse move。
发送离散事件前先提交末尾位置，操作权、页面与连接变化时丢弃旧采样。

React 视图补充历史导航、指数退避重连、移动端触摸节流与取消清理。
隐藏标签停止图流与输入连接，保留状态订阅及最后一帧。
开发诊断避免慢请求与轮询叠加。
Windows 本地身份从可执行文件版本信息读取，Edge 保留品牌标记。

## 自定义接入升级

自定义 `SessionFactory` 的 `Page` 需要调整以下方法：

```go
Click(context.Context, ElementRef, ClickOptions) error
Press(context.Context, PressOptions) error
SetChecked(context.Context, ElementRef, bool) (bool, error)
Drag(context.Context, ElementRef, ElementRef) error
```

原有的 `Type`、`Wait`、`Screenshot` 等方法保持原签名。
历史导航、坐标动作、下载、弹窗与事件订阅使用可选能力接口；未实现时返回明确的不支持错误。
原生 Rod 实现已提供这些能力。

`browserTabs` 的函数处理器类型改为 `TabActionRequest -> TabActionResponse`；
只传 `session_id` 的 JSON 列表请求继续可用。
直接调用 `Manager.ListTabs(TabsRequest)` 的 Go 代码不受此工具签名调整影响。

`BrowserSource.activate/navigate/reload/viewport` 可以直接返回更新后的状态，
也可以继续返回 void，由视图查询状态。
新增的 `history()` 为可选方法，未实现时隐藏前进与后退按钮。
输入连接可实现 `discardPendingInputs()`，在操作边界清理传输层缓存。
`followActivePage=false` 可固定展示宿主指定的 pageID。
React 包继续单独导出默认样式，不需要 keycode 的 UI 控件或图标绑定。

原有序列仍记录导航、基于 ref 的点击、输入与显式脚本/外部能力。
新增的勾选、拖放、坐标、非默认按键和双击动作不自动扩展既有序列格式；不要把任意动作批次视作完整的可回放录制。

## 验证

常规验证使用 `go test ./...`、`go test -race ./...`、`go vet ./...`，
React 测试，以及库、standalone、demo 和 npm 打包构建。
`make smoke` 覆盖全部 `TestReal*` 真实 Chrome 与 CLI 测试，包含跨域和跨站 frame、
表单状态、批量动作、显式弹窗、下载、截图、历史导航、序列录制回放与 24 个浏览器工具的 JSON 调用。
OOPIF 靶场在额外回环地址不可用时回退到 localhost，与父页面的 127.0.0.1 保持跨站关系。
