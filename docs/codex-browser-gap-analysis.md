# Codex 浏览器交互的 20 项欠缺

日期：2026-10-26（Asia/Shanghai）\
测试目标：`http://127.0.0.1:8879/snapshot.html`\
测试表面：当前 Codex 桌面应用的 in-app Browser、accessibility snapshot、元素索引操作和 `pageAssets`

## 范围与方法

这里的“Codex 欠缺”特指当前桌面应用浏览器交互链路，不等同于模型本身在所有宿主、
Responses API computer tool、Chrome 插件或 Playwright 集成中的永久限制。部分问题可以通过
截图、Playwright locator、页面脚本或切换到常用 Chrome 绕过；报告仍将它们列为欠缺，
因为绕过会引入额外工具选择、时序、权限和验证成本。

测试沿用 BrowserKit 对比实验的同一确定性页面。运行时证据包括：

- 100ms 全量替换的动态列表和周期性节点替换；
- loading、720ms partial、complete、503 error 四种异步状态；
- disabled、modal、pointer-only、contenteditable、inert、视觉遮挡；
- open/closed Shadow DOM、同源 iframe；
- 160 个密集按钮和 105 个 native select option；
- 预填表单值、带 query 的链接和失败 fetch；
- Codex 浏览器能力清单与 `pageAssets` 实际返回值。

官方 OpenAI 文档用于确认产品边界，而不是替代实测。官方说明内置 Browser 和用户常用的
Chrome 是两个入口，后者需要扩展和权限；同时明确建议在 UI 未知时回传当前观察，并验证
实际结果而不是只依赖最终回答：

- [Using your computer and browser](https://developers.openai.com/training/walkthroughs/using-your-computer-and-browser)
- [Computer use](https://developers.openai.com/api/docs/guides/tools-computer-use)
- [Computer use integration recipes](https://developers.openai.com/api/docs/guides/tools-computer-use-integration)

证据标记：**R** 为当前 Codex 桌面应用运行时复现；**I** 为当前浏览器接口/能力清单确认；
**D** 为官方 OpenAI 文档确认；**B** 为同目标 BrowserKit 对照。

## 20 项清单

| ID | 优先级 | 欠缺 | 证据 | 影响与分析 | 建议方向 |
| --- | --- | --- | --- | --- | --- |
| C01 | P1 | 高频 DOM 下数字元素索引容易失效 | R/B | 100ms 全量替换列表中，依据上一轮 snapshot 的按钮索引点击失败；Agent 必须重新观察。BrowserKit 至少把同类问题显式建模为 stale ref。 | 为每次观察返回 snapshot token，并让操作绑定 token；支持按稳定语义重新解析。 |
| C02 | P1 | 失效操作错误缺少可恢复语义 | R | 动态旧索引失败信息是泛化的 shadow-root 检查错误，没有 `stale_element`、是否已产生副作用、是否可安全重试等字段。 | 标准化 `stale_target`、`covered`、`detached`、`ambiguous`、`retry_safe`。 |
| C03 | P1 | accessibility 输出没有通用稳定 locator | R/I | 有 DOM `id` 的节点可见 ID；没有 ID 的动态项只剩瞬时数字索引和名称，不能可靠跨观察引用，也不能导出稳定 selector。 | 同时返回 backend node identity、role/name locator、frame path 和可选 CSS locator。 |
| C04 | P1 | 高频 diff 可能是半一致观察 | R | 动态列表重建期间的一次 diff 出现 12 个没有名称的 `button`，下一次完整树又恢复名称；模型可能在过渡态做决定。 | 原子化采集 accessibility tree，或在 mutation 静默窗口后返回，并标记 unstable snapshot。 |
| C05 | P2 | 观察结果没有采集时间、generation 或内容 hash | R/I | 输出只有 tab/title/URL 和树，没有 `captured_at`、snapshot ID、DOM generation；调用方无法判断两个结果的相对新旧和稳定性。 | 返回单调 snapshot ID、采集时间、document identity、mutation generation。 |
| C06 | P1 | 语义操作面没有条件等待/稳定等待 | R/I/B | 点击异步加载后能看到 loading，但下一次自然观察已直接到 complete，错过 720ms partial；BrowserKit `browserWait` 可精确卡住该中间态。Playwright 可绕过，但需要切换接口。 | 在浏览器主操作面加入 wait-for text/role/state/count/network/settle 和组合条件。 |
| C07 | P2 | 完整 accessibility 观察延迟较高且抖动明显 | R/B | 扩展页冻结后 5 次完整树耗时为 69、102、103、115、223ms；BrowserKit 同页约 10–22ms。两者信息量不同，但高频 Agent 循环仍受影响。 | 支持服务端过滤、子树读取、紧凑状态投影和稳定缓存。 |
| C08 | P2 | 完整树占用上下文大 | R/B | 冻结后的完整树为 15,109 字符；大量稳定元素会反复占据模型上下文，增加成本并稀释重点。 | 返回结构化对象或紧凑编码；允许仅取变化、目标区域或状态摘要。 |
| C09 | P2 | 没有 subtree、role/name filter、limit 或分页参数 | I | `getAXState` 只有 diff/full 开关，无法请求“异步区域”“前 20 个按钮”或某个 frame；页面越大越只能整树读取。 | 增加 root/role/name/state filter、depth、limit、cursor。 |
| C10 | P2 | 折叠的 native select 仍展开全部 option 到树中 | R | `Large option set` 处于 collapsed 状态，但 105 个 option 全部进入输出；虽然信息完整，却造成明显上下文膨胀。 | 默认只返回 selected value 和 option count，需要时再按需展开/搜索 options。 |
| C11 | P1 | 忽略视觉遮挡，并可穿透遮罩点击底层控件 | R | `Covered action` 在树中显示为普通按钮；用数字索引点击成功并触发底层业务事件，尽管用户视觉上无法点到它。这比单纯误报可见性更严重。 | 默认采用 hit-test/真实指针语义；DOM/AX 强制点击必须显式标注并受策略控制。 |
| C12 | P2 | 非语义自定义控件最初只表现为 text | R | pointer-only 控件第一次是普通 text；点击后才变成带 description 的 container。Agent 无法在操作前可靠判断它是否可点击、可键盘操作或只是标签。 | 合并可访问性、事件/focusability 和视觉 cursor 信号，并标记推断置信度。 |
| C13 | P1 | contenteditable 缺少明确的可编辑动作语义 | R/I | `Editable note` 被报告为含 Value 的 container，不是 settable textbox；直接 accessibility 操作面没有明显 fill/type 契约，需要切换到 locator 或尝试键盘。 | 把 contenteditable 归一为 editable textbox，返回 multiline/readOnly 和 set-value 能力。 |
| C14 | P2 | 可访问关系在文本化输出中丢失 | R | `aria-describedby` 的帮助文字作为独立 text 出现，没有附着到 `Prefilled account`；`aria-controls`、owns、flowto 等同类关系也难以从扁平文本恢复。 | 保留 description、controls、owns、labelledBy 等边，并用引用连接节点。 |
| C15 | P3 | closed Shadow DOM 消失且没有 opaque 边界提示 | R/B | closed shadow 内按钮完全不在树中，页面输出也没有提示这里存在无法观察的封装子树；BrowserKit 同样受限。 | 返回 `opaque_subtree`/shadow boundary 元数据；能用 CDP AX tree 时提供授权读取路径。 |
| C16 | P1 | 当前输入值默认进入模型上下文，缺少按敏感度脱敏 | R | `Prefilled account` 的 `existing-value` 直接出现在 accessibility 输出。对密码外的邮箱、令牌、订单号、内部 ID 等仍可能敏感。 | 按字段类型、autocomplete、名称和站点策略返回 masked/has-value；显式授权后再读原值。 |
| C17 | P1 | 链接完整 query 默认进入模型上下文 | R | `Destination link` 暴露完整 `?source=snapshot-fidelity`；真实应用中 query 常包含搜索词、内部资源 ID 或一次性参数。 | 默认拆分 origin/path 与敏感 query；按任务和权限选择性展示。 |
| C18 | P1 | 页面资源诊断缺少响应结果与性能数据 | R/I/B | 成功和 503 fetch 都只显示 URL、kind 和来源；没有 status、失败原因、transfer bytes、duration。BrowserKit assets 能提供状态、大小和耗时。 | 为 observed resource 增加 method/status/error/initiator/size/timing，并区分 pending/completed/failed。 |
| C19 | P3 | 当前 in-app Browser 没有 console/network 日志能力 | I | 本次 tab capability 只有 `pageAssets` 与 `webmcp`；异步错误只能靠页面自己渲染，无法读取 console exception、请求响应或 stack。 | 提供有界、可清理、按级别/域过滤的 console 与 network diagnostics。 |
| C20 | P3 | 内置 Browser 与用户已登录 Chrome 是分离上下文 | D | 官方文档把 Browser 与 Chrome 列为不同入口；常用 Chrome 需要扩展和权限。内置浏览器测试无法自然继承用户登录态、扩展和日常 profile。 | 让任务显式声明所需 browser context，并提供安全、可审计的会话移交或状态导入。 |

## 优先顺序

第一批应处理 C01–C06 与 C11：它们会造成错误目标、不可恢复失败、过渡态决策或视觉上
不可能的点击。尤其是 C11，默认穿透视觉遮罩会破坏“Codex 实际完成了一个人能够通过 UI
完成的动作”这一验证前提。

第二批处理 C07–C10：它们决定大型页面上的时延、上下文成本和信息密度。当前完整性很好，
但所有稳定节点和 collapsed options 都进入上下文，不利于持续测试。

第三批处理 C13–C19：补齐可编辑语义、关系、隐私策略和诊断能力。C16/C17 虽然没有造成
当前测试数据泄露，但在真实已登录页面中风险更高，建议与浏览器 origin 权限一同设计。

## 与 BrowserKit 的互补关系

Codex 优于当前 BrowserKit 的方面仍然明确：disabled、checked、radio、ARIA switch、输入值、
link destination、完整 option 列表和 modal focus scope 都更完整；它还能结合截图做视觉判断。
BrowserKit 的优势是快照紧凑、显式 stale ref、条件等待、XHR 状态/大小/耗时，以及可保存和
回放操作序列。

较合理的演进方向不是互相复制全部功能，而是：Codex 保留 accessibility/visual 语义，增加
稳定 snapshot token、真实 hit-test、条件等待和有界诊断；BrowserKit 补齐状态/层级/可访问
名称，并消除 DOM 注入。两者都应明确区分语义 DOM 操作与真实人类可见的指针操作。

## 不计入 20 项的相关能力空白

当前 Codex Browser 没有直接生成 BrowserKit 风格的可移植 sequence/replay artifact；大型多步
流程通常需要临时写 Playwright 或重新推理。该项值得单独评估，但本报告没有把它计入 20 项，
以免把“工作流产物能力”与本次页面观察/交互通道的欠缺混在一起。

机器可读结果见
[`codex-browser-gaps-2026-10-26.json`](results/codex-browser-gaps-2026-10-26.json)。
