# Go SDK 验证记录

本次能力基于 `ba1ea8ee12121f7dc01a402d7711a9a3b0df8aef`，沿用 Manager、Session/Page 可选接口及 Rod/CDP 实现。配置和调用示例见 [host-sdk.md](host-sdk.md)，frame 的实际支持边界见 [frame-tool-design.md](frame-tool-design.md)。

## 行为与回归用例

| 能力 | 行为 | 主要验证 |
| --- | --- | --- |
| 宿主配置 | 认证代理凭据只回应匹配代理的 challenge；可配置忽略证书错误、环回代理和关闭空闲回收 | `host_proxy_smoke_test.go`、`host_lifecycle_test.go`、`proxy_test.go` |
| 下载寿命 | 自动跟踪实际 GUID；按 ID 查询、续等及取消，等待超时不取消下载；进度与完成大小均检查预算 | `download_lifecycle_smoke_test.go`、`session_download_test.go` |
| 下载归属 | `start` 只执行一次点击并返回 `download_unconfirmed`，不从 frame、URL、新弹窗或事件顺序猜测文件；实际结果通过 `list → GUID → query/wait` 获取 | `acceptance_regression_smoke_test.go`、`TestRealBrowserDownloadStartDialogDoesNotConfirmClick` |
| 附件导航 | 仅接受 Chromium 原生 `isDownload` 事实，返回 `load_status=download` 并保留会话与原文档 | `page_navigation_test.go`、`TestRealBrowserSDKNavigationDownloadKeepsSession` |
| 选择与上传 | 单选、多选、按值/标签及清空；多文件与文件选择器逐个复用授权路径和单文件 10 MiB 限制 | `page_form_smoke_test.go` |
| frame 与开放根 | 嵌套 iframe/OOPIF、开放 Shadow DOM、密码 ref、全 Tab 预算和分页；文档及节点身份防止旧引用复活 | `frame_baseline_smoke_test.go`、既有 frame 定向用例 |
| 输入完整性 | 固定模板显式查 ref，输入使用 CDP 参数；各文档共用特殊输入格式、min/max/step 校验与回滚 | `frame_input_smoke_test.go`、`acceptance_regression_smoke_test.go` |
| 执行事实 | 步骤失败与附加快照/截图失败分开；取消保留部分结果；CLI 不把 Act 响应内失败当成功 | `manager_browser_test.go`、`cmd/browserkit/act_result_test.go` |

延迟下载回归用例先触发 A，再触发 B，由 B 的请求释放 A 的响应。两次 `start` 均不得返回猜测的文件或 ID；释放两个响应后，分别按实际 GUID 等待并核对 `a.txt=A`、`b.txt=B`。同 frame 不构成此次点击的因果证据。

frame 输入回归包括 `document.querySelector(...)`、引号、换行及追加文本原样保留；`min=1/max=10/value=3` 的 number 输入填写 `99` 时返回 `invalid_value` 并恢复 `3`。主文档、同源、跨域和 OOPIF 开放根复用同一特殊输入实现。

## 同源 frame 元数据回归

`TestRealBrowserSnapshotFormValues` 在基线通过，而合并 frame 采集后的实现会遗漏 `IframeSecret`、`FadedFrameSecret`、`RightFrame`。原因是把“宿主导致字段值不可见”直接当成“整个文档不应枚举”，跳过了原有同源控件元数据。

修复区分内容遮蔽与文档排除：同源低透明度、零尺寸或极远屏外 frame 保留 ref、名称和角色，但隐藏值、选项、正文和活动字段值；是否同源按浏览器实际访问完整父链的权限判断，不根据 URL 猜测。跨域隐藏 frame 的原有排除规则保留。正常同源 frame 因页面滚动而离开视口时，正文和字段值的可见性分别判断。

此修复保留原表单及隐藏宿主测试的断言，并增加嵌套 `srcdoc`、同源/跨域/OOPIF、分页、名称回退及普通滚动的定向用例。原 `docs/frame-tool-design.md` 同时恢复英文，保留最新能力说明。

Docker 对照运行原用例：基线 `ba1ea8e` PASS 1.306s；修复前 PR 提交 `7966ae1` FAIL 1.295s，缺少上述三个字段。修复后使用原测试断言，最终运行：

```sh
docker exec browserkit-pr-review go test -race -tags=browser_smoke . \
  -run '^(TestRealBrowserSnapshotFormValues|TestRealBrowserSameOriginHiddenHostExcluded|TestRealBrowserOOPIFHiddenHostExcluded|TestRealBrowserCrossSiteOOPIF|TestRealBrowserFrameMetadataOnly|TestRealBrowserScrolledFrameKeepsValues|TestRealBrowserFrameBaseline|TestRealBrowserSnapshotValueAfterScroll)$' -v
docker exec browserkit-pr-review go test . -run 'TestFunctionsContract|TestMapFramePoint'
docker exec browserkit-pr-review go build ./...
docker exec browserkit-pr-review env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...
```

结果：真实浏览器定向 race 全部 PASS 26.618s；声明与几何单测 PASS 0.003s；两种架构构建通过。`TestRealBrowserFrameMetadataOnly` 包括 12 组宿主呈现/源组合，字段值、选项、正文、名称回退与活动值不会在仅元数据回执中泄漏；`TestRealBrowserScrolledFrameKeepsValues` 确认正常滚动保留字段值，绝对负向定位仍遮蔽。未改动旧测试断言，也未运行无关全量回归。

## Docker 运行

所有依赖、构建及浏览器测试在 Docker 内执行。测试镜像按当前机器架构安装发行版 Chromium，不固定 `--platform`。

```sh
docker build -f scripts/Dockerfile.sdk-test -t browserkit-sdk-test:local .
docker run -d --name browserkit-sdk-check --shm-size=1g \
  -v "$PWD:/workspace/browserkit" \
  -w /workspace/browserkit --entrypoint sleep browserkit-sdk-test:local infinity
docker exec browserkit-sdk-check go mod download
```

函数声明、定向 race 与构建：

```sh
docker exec browserkit-sdk-check go test . -run TestFunctionsContract
docker exec browserkit-sdk-check go test -race . \
  -run 'TestDownload|TestBrowserDownload|TestMapFramePoint|TestType|TestValueInput|TestAtomicClick'
docker exec browserkit-sdk-check go build ./...
docker exec browserkit-sdk-check env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...
```

真实浏览器回归与点击事实：

```sh
docker exec browserkit-sdk-check go test -race -tags=browser_smoke . \
  -run '^TestAcceptance|^TestRealBrowserDownloadStartDialogDoesNotConfirmClick$' -v
```

相关能力可按主题定向验证，不需要重复全量 smoke：

```sh
docker exec browserkit-sdk-check go test -tags=browser_smoke . \
  -run 'TestRealBrowserFrameInputSemantics|TestRealBrowserFrameBaseline|TestRealBrowserAffineFrameClicks|TestRealBrowserClosedShadowScopeAndClipping|TestRealBrowserNativeSelectAndFileChooser' -v
docker exec browserkit-sdk-check go test -tags=browser_smoke . \
  -run 'TestRealBrowserSDKNavigationDownloadKeepsSession|TestRealBrowserAutomaticDownload|TestRealBrowserDownloadBudget|TestRealBrowserDownloadsNavigation|TestRealBrowserDownloadFromNestedOOPIF' -v
docker exec browserkit-sdk-check go test -tags=browser_smoke . \
  -run 'TestRealHostAuthenticatedProxyAndCertificates|TestRealOperationCancellationPreservesSession|TestRealBrowserOwnsAndRemovesLocalProfile' -v
docker rm -f browserkit-sdk-check
```

## 实测结果与范围

实测环境为 Go 1.25.14、Chromium 156、linux/arm64。函数声明检查、arm64 原生 Go 构建和 linux/amd64 Go 交叉编译通过。输入、下载误关联与弹窗抢先场景的最终浏览器 race 检查通过（6.324s），下载/点击事实相关单元 race 检查通过（1.039s）。四种文档输入语义及相关表单用例通过（13.147s）；frame 点击、hover、check、仿射变换及导航失效相关用例通过（13.028s）。

其他通过的定向验证包括代理环回 HTTPS、自签名/证书名不匹配及默认拒绝、代理凭据不进入目标认证、操作取消保留 Cookie/session、临时 profile 清理、下载续等不重点击、过程中预算终止、明确取消、宿主关闭清理、不同 Tab 下载及同名文件、附件导航保留现场。没有执行无关 React 回归或全量 smoke。

未验证与实际限制：

- amd64 仅完成 Go 交叉编译，未运行 amd64 Chromium；未做小时/天级压力运行或跨进程重启恢复。
- HTTPS 实测使用 HTTP 认证代理的 CONNECT；未独立搭建可信 CA 目标、HTTPS 代理本身或所有认证方式的 fixture。
- 下载预算依赖 CDP 进度事件间隔，会有在途超出，完成时再次检查实际大小。CDP 未给出 canceled 的具体原因时报告失败，不猜测已明确取消。
- frame 过快销毁时下载 TargetID 可能无法核实，保留实际 FrameID；列表不证明与最近一次动作的因果。关闭仅清理 session 临时/未完成文件，完成文件归宿主持有。
- 闭合 Shadow DOM 明确在 `open_only` 范围之外。仿射变换受实际命中核实约束；透视、退化、中心被裁剪或遮挡返回错误。动态变换及复杂 3D 组合未完整验证。
- 文件选择器成功只表示原生输入框接受文件，站点业务上传完成需继续观察页面或网络；单文件始终限制为 10 MiB。
