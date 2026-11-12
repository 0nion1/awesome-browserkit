# keycode 浏览器增强迁移

本次以 keycode 提交 `9f5c208c7f06e701c15cddf044ed97665203ea7d` 为来源，
回迁 `plugin/browser/sdk` 与 `plugin/browser/frontend/view` 的浏览器能力。
keycode 同步的原始 BrowserKit 基线为 `1d22c4ff7e0981c00ccbf756ceb704e1ad57d7b5`。
合并按该基线做三方比较，保留本项目的独立 HTTP 服务、CLI、React 包与序列功能。
原生依赖版本不变，运行时不依赖 keycode、Agent、插件加载器或宿主 UI。

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

快照最多聚合 3 个可见跨域 frame 的正文与控件，包括跨站 OOPIF。
frame 元素使用包含文档身份的独立 `f` 前缀 ref。
点击、悬停、输入、下拉、勾选、按键、拖放、上传和等待可路由到 frame。
旋转、镜像、skew、遮挡、裁剪或不支持的缩放返回明确错误，避免误点父页面。
当前 OOPIF 操作支持一层；不支持的 frame 操作明确返回 `unsupported_frame_operation`。

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
