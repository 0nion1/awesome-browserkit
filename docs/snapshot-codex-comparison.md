# BrowserKit snapshot 与 Codex 页面理解实测

日期：2026-10-26（Asia/Shanghai）  
仓库基线：`e36f901`，叠加当前工作区改动  
环境：macOS arm64、Go 1.25.8、Google Chrome 156.0.8078.47

## 结论

BrowserKit 更适合作为 Agent 的高频、结构化执行通道：快照小、速度快，元素 ref
与页面绑定，旧节点操作能返回明确的 `stale_reference`，并且 `browserWait` 能可靠地
卡住异步中间态。Codex 当前的浏览器可访问性快照更接近完整 UI 语义：它保留 disabled
状态，在打开 modal 后自动收敛到 modal 子树，而且不需要为 160 个按钮手动分页；代价是
快照更大、更慢，高频 DOM 替换时旧数字引用容易失效，自然的“观察—推理—再观察”循环也
可能越过亚秒级中间态。

这两条通道不是互相替代关系。实际 Agent 流程中，BrowserKit 适合等待、轮询、定位和执行，
Codex 的视觉/可访问性观察适合判断页面层级、阻塞弹层、disabled 原因和整体布局。
本轮继续细化后，共确认 20 项互不重复的 BrowserKit 欠缺；其中 17 项由真实 Chrome
自动化直接复现，3 项由公开数据结构、执行路径或浏览器封装边界确认。

## 测试集

运行 `make snapshot-lab`，访问
`http://127.0.0.1:8879/snapshot.html`。测试页由
`internal/toollab` 的同一个 Go 服务提供，不依赖公网或随机后端数据。

| 场景 | 页面行为 | 主要检查点 |
| --- | --- | --- |
| S1 语义与可见性 | label、ARIA、disabled、`aria-hidden`、pointer-only、dialog | 名称/角色、禁用状态、不可访问节点、弹层范围 |
| S2 变化元素 | 100ms 时钟、同节点改名、500ms 整节点替换、700ms 显隐 | ref 稳定性、旧 ref、revision 含义 |
| S3 高频集合 | 12 行列表每 100ms 全量替换并重排 | 快照一致性、旧引用、冻结后操作 |
| S4 异步页面 | 240ms summary、再等 720ms details、503 失败 | loading、partial、complete、error 四态 |
| S5 嵌入树 | open Shadow DOM、同源 iframe、iframe 内 Shadow DOM | 跨树元素发现和名称 |
| S6 密集页面 | 160 个稳定按钮 | 单次上限、offset 分页、上下文体积 |
| S7 状态与保真边界 | checked/pressed、ARIA switch、contenteditable、inert、遮挡、大型/多选 select、closed Shadow DOM | 状态字段、名称算法、角色覆盖、可操作性、封装边界、页面侵入性 |

真实 Chrome 回归入口是：

```sh
go test -tags=browser_smoke . -run '^TestRealBrowserSnapshotBehaviorLab$' -count=1 -v
```

测试通过 JSON Function Tool 边界调用 `browserOpen`、`browserSnapshot`、
`browserClick` 和 `browserWait`，不是直接调用页面 DOM 测试桩。`make smoke` 也已包含该用例。

## BrowserKit 实测

扩展测试集后连续运行专项 smoke 三次，全部通过。每次初始完整快照都返回 211 个可交互元素，包含
160 个 dense actions；`max_elements=25` 时，用 9 个非空页加 1 个终止空页恢复全部元素，
没有 dense action 重复或遗漏。

| 项目 | 观察结果 |
| --- | --- |
| 完整快照耗时 | 三轮中位数分别为 11.84ms、11.71ms、11.41ms |
| 单次范围 | 最小 10.16ms；最大 22.18ms |
| 文档 revision | 动态更新期间保持 `1`；当前 revision 表示 document 身份变化，不是每次 DOM mutation |
| 同节点改名 | 名称变化，ref 保持不变 |
| 节点替换 | 新节点取得新 ref；旧 ref 点击返回 `stale_reference` |
| 高频列表 | 两次快照可见不同 12 项代际；暂停后名称与 ref 稳定 |
| 异步页面 | 通过 `browserWait` 稳定观察 loading、partial、complete 和 503 error |
| disabled/ARIA hidden | 不进入 `elements`；可见 disabled 文本仍可能出现在正文 text 中，但没有结构化 disabled 状态 |
| dialog | 能发现打开后的按钮，但同时保留背景页面的可交互元素 |
| Shadow DOM / iframe | open Shadow DOM 和同源 iframe 内元素均被发现 |
| 状态与角色边界 | checked/pressed 等状态不返回；ARIA switch 漏掉；contenteditable 被标成 button |
| 可操作性边界 | inert 与被遮挡按钮先进入快照，点击时才返回 `element_not_interactable` |
| 大型控件 | 105 个 option 只返回前 100 个；multiple select 操作返回 `unsupported_select` |
| 页面侵入 | 快照写入 `data-browserkit-ref`、覆盖同名页面属性，并暴露 window 全局状态对象 |

耗时只用于描述本机这次实现表现，不是跨机器 benchmark。BrowserKit 返回的是定制的文本与
交互元素投影，不能把它和完整 accessibility tree 的耗时作完全等价的比较。

## Codex 实测

使用当前 Codex 桌面应用的 in-app browser 打开同一 URL，通过实际 accessibility snapshot
进行观察和点击。没有读取源码来代替页面理解。

| 项目 | 观察结果 |
| --- | --- |
| 基础测试集初始完整快照 | 11,123 字符，199ms；能看到 160 个 dense actions |
| 加入 S7 后的稳定完整快照 | 最终稳定为 14,370 字符；连续 5 次耗时为 97、100、101、103、120ms |
| disabled/ARIA hidden | disabled 控件保留并明确标为 disabled；`aria-hidden` 控件不进入可访问性树 |
| 同节点改名 | accessibility diff 保持元素 ID 并更新名称 |
| 高频列表 | 一次 diff 为 1,477 字符，列出时钟、phase、generation 和 12 行的变化 |
| 高频旧引用 | 使用旧数字引用点击失败，错误较泛化；点击稳定的“Pause live feed”后，再点动态项成功 |
| 异步页面 | 点击后的第一轮观察看到 loading 和两个 disabled 按钮；下一轮自然观察直接到 complete，错过 720ms partial 窗口 |
| dialog | 打开后 accessibility snapshot 只保留 modal 子树；关闭后恢复完整页面 |
| pointer-only | 初始表现为 text，但仍可点击；获得焦点后表现为带 description 的 container |
| Shadow DOM / iframe | Shadow 输入与按钮、iframe 及其内部 Shadow 按钮都可见 |
| S7 状态 | checked/radio/switch 都给出当前值；pressed+expanded 控件给出展开状态；输入当前值可见 |
| S7 名称与目标 | submit 的 value 成为按钮名、link 给出目标 URL、长名称没有静默截到 160 字符 |
| S7 可操作性 | inert 控件不进入树；被遮挡控件仍进入树；closed Shadow DOM 同样不可见 |
| S7 大型 select | 105 个选项全部可见；multiple select 表现为 list |

上述“错过 partial”描述的是自然 Agent 回合，不代表 Codex 无法通过脚本化 locator 或明确等待
捕捉该状态。它揭示的是：当页面状态窗口短于一次观察和推理循环时，Agent 需要稳定条件，
不能依赖“稍后再看”。

## 对比

| 维度 | BrowserKit | Codex accessibility snapshot |
| --- | --- | --- |
| 高频读取成本 | 扩展测试集本机约 11.4–11.8ms 中位数 | 扩展测试集本机稳定态约 97–120ms，完整树更大 |
| 输出控制 | `max_chars`、`max_elements`、`offset` | 一次给出完整树，依赖 diff 降低后续体积 |
| 操作引用 | 不透明 ref + revision；替换节点有结构化 stale error | 数字元素 ID；动态替换时失败信息不够针对性 |
| DOM 动态一致性 | 单次 Eval 原子采集；跨 offset 页不保证动态列表一致 | 单次树完整；跨 Agent 回合仍可能发生替换 |
| disabled 状态 | 元素被过滤，结构化信息缺失 | 保留并标记 disabled |
| modal 层级 | 背景控件仍在快照中 | 自动收敛到 modal 子树 |
| 亚秒异步状态 | `browserWait` 可按文本/选择器精确等待 | 自然 Agent 循环可能跳过，需 locator/wait 或页面稳定信号 |
| 嵌入内容 | open Shadow DOM、同源 iframe | open Shadow DOM、同源 iframe，且 AX 层级更清楚 |
| 密集元素 | 默认最多 100，需要调大或分页 | 一次返回全部，但消耗更多上下文 |
| 视觉分析 | 需显式 screenshot | 可组合 screenshot 与 accessibility tree |

## BrowserKit 的 20 项欠缺

证据标记：**R** 表示真实 Chrome 自动化复现；**C** 表示公开结构或执行路径确认；
**X** 表示同一页面上的 Codex accessibility snapshot 给出了更完整的对照。
优先级采用 P1（会误导操作或破坏一致性）、P2（显著降低理解/覆盖）、P3（明确边界或长尾）。

| ID | 优先级 | 欠缺 | 证据 | 影响与分析 | 建议方向 |
| --- | --- | --- | --- | --- | --- |
| F01 | P1 | disabled 控件从 `elements` 完全消失 | R/X | Agent 只能从正文猜出控件存在，无法区分“没找到”和“尚未启用”，也无法解释前置条件。Codex 明确保留 disabled。 | 返回元素并增加 `disabled`；需要保持旧行为时提供 `include_disabled`。 |
| F02 | P1 | 控件状态不完整：无 checked、pressed、expanded、busy、current 等 | R/X | checked checkbox、radio、ARIA pressed/expanded 在 BrowserKit 中只有 role/name；状态判断会发生语义错误。 | 为 `Element` 增加通用状态字段，按原生属性和 ARIA 归一化。 |
| F03 | P2 | 普通输入当前值不公开 | R/C/X | `Prefilled account` 没有 value，Agent 无法判断是否已有内容、是否需覆盖。当前代码是出于隐私主动隐藏，属于安全与可用性的权衡，不应简单全量开放。 | 默认仅返回 `has_value`/长度/掩码；由显式敏感度策略允许 value。 |
| F04 | P2 | link 不返回 href/target | R/X | 快照知道“Destination link”，但不知道将导航到哪里，点击前无法做目标域、下载或新 Tab 风险判断。Codex 给出完整目标 URL。 | 增加规范化 `href`、`target`、`download`，继续受网络策略约束。 |
| F05 | P1 | 可访问名称算法不完整 | R/X | `input[type=submit][value="Submit from value"]` 的 BrowserKit 名称为空；实现只拼 aria-label/label/placeholder/title/text/name，没有采用浏览器的完整 accessible-name 计算。 | 优先读取 Accessibility Tree/ComputedAccessibilityInfo，DOM 规则仅作为回退。 |
| F06 | P2 | 元素名称静默截断到 160 字符 | R/X | 长按钮名被截断且没有 `name_truncated`；相似长名称可能在 160 字符内完全相同，造成定位歧义。Codex 保留完整名称。 | 返回 `name_truncated` 和稳定摘要/hash，或允许配置上限。 |
| F07 | P1 | 交互角色使用固定 selector 白名单 | R/X | 无 tabindex/onclick 的合法 `role=switch` 完全漏掉；treeitem、spinbutton、scrollbar 等同类 ARIA 控件也存在同样风险。 | 以可访问性树为主，或扩展为完整 interactive-role 集合并结合 focusability/actionability。 |
| F08 | P1 | contenteditable 默认误分类为 button | R/X | `contenteditable=true` 虽被发现，却落入 fallback `button`；Agent 会点击而不是输入。Codex 将其呈现为含当前文本的可编辑 container。 | 映射为 textbox，并暴露 multiline/readOnly/contenteditable 状态。 |
| F09 | P1 | modal 打开后仍返回背景交互元素 | R/X | 背景按钮无法实际点击，但仍和 modal 操作并列，增加误操作和无效重试。Codex 快照自动收敛到 modal 子树。 | 识别 top layer、modal dialog、`aria-modal` 和 inert 背景；默认只返回有效交互范围。 |
| F10 | P1 | interactability 过滤不足，inert/被遮挡控件先被宣称可交互 | R/X | 两类按钮都进入快照，直到点击才报 `element_not_interactable`；这把可预判问题推迟成执行失败。 | 快照阶段加入 inert、hit-test、遮挡与 top-layer 判断，并返回原因字段而非直接丢弃。 |
| F11 | P2 | 快照是正文字符串加扁平元素表，没有层级和几何 | C/X | heading/list/table/section 关系、元素父子关系、bounds、viewport 内外和 z-order 都缺失；复杂表格、菜单和多栏页面难以理解。 | 提供可选 compact AX tree，或为元素增加 parent/section/bounds/viewport 状态。 |
| F12 | P3 | cross-origin iframe 内部是不可见黑盒 | C | 当前仅返回 frame 自身和 `frame_url`；登录、支付、第三方编辑器等常见嵌入内容无法由主页面 session 继续操作。 | 为授权 origin 建立独立 frame target/namespace；明确不可授权 frame 的边界。 |
| F13 | P3 | closed Shadow DOM 不可见 | R/X | BrowserKit 和 Codex accessibility snapshot 本次都未看到 closed shadow 内按钮；这是封装边界，不是 BrowserKit 独有，但会造成 Web Component 覆盖缺口。 | 能用 CDP Accessibility tree 时读取语义；无法操作时显式报告 opaque subtree。 |
| F14 | P1 | `revision` 只是 document 身份，不是 DOM 版本 | R/C | 100ms mutation、节点替换、异步完成期间 revision 始终为 1；调用方容易误以为相同 revision 代表相同页面。 | 改名为 `document_revision`，另加单调 `dom_generation` 或 content hash。 |
| F15 | P2 | 没有 captured_at、generation、hash 或稳定窗口 | C | 调用方不知道快照采集时刻，也无法请求“连续稳定 N ms 后返回”；实时页面只能靠业务 Pause 按钮。 | 增加 `captured_at`、`snapshot_id`、`settle_ms` 和稳定判据。 |
| F16 | P1 | offset 分页不是同一逻辑快照 | R | 高频列表前 6 项和后 6 项分别来自不同 generation；拼起来看似 12 项，实为不存在过的混合集合。 | 首次调用物化候选 ID 列表并返回分页 token；后续页绑定 token/快照 ID。 |
| F17 | P2 | `truncated` 含义歧义且合并两种截断 | R/C | 元素数恰好等于 `max_elements` 时，即使后一页为空也返回 true；同一 bool 还合并 text 与 elements 截断，调用方不知道该翻页还是增大文本上限。 | 拆成 `text_truncated`、`elements_truncated`、`has_more_elements`，判断时探测 limit+1。 |
| F18 | P2 | select 覆盖不完整 | R/X | 105 个 option 只返回 100 个且无 option 分页接口；multiple select 虽进入快照，操作必然 `unsupported_select`。Codex 显示全部 105 个并把多选呈现为 list。 | 增加 option cursor/搜索，以及多选 values 数组和 set/add/remove 操作。 |
| F19 | P2 | wait 条件过窄 | R/C | 仅支持 visible/hidden、URL/title/text substring、load complete；缺少属性/状态、元素数、文本正则、网络安静、DOM 稳定、任一/全部条件组合。 | 引入结构化 predicate，支持 count/state/attribute/network/settle 和 AND/OR。 |
| F20 | P1 | snapshot 会修改业务 DOM 并暴露全局状态 | R/C | 每个元素写入 `data-browserkit-ref`，页面自有同名属性被覆盖；还创建 `window.__browserkit_page_state_v1__`。这可能触发业务 MutationObserver、CSS 选择器、序列化差异或名称冲突。 | ref 存到 CDP backendNodeId/隔离 world/WeakMap；至少使用不可碰撞 Symbol 和不写 DOM 的映射。 |

### 优先处理顺序

第一批建议处理 F01、F02、F05、F07、F08、F09、F10：它们直接决定 Agent 是否能正确
理解并选择控件。第二批处理 F14、F15、F16、F17：它们决定动态页面结果是否一致、是否可
解释。F20 应和 ref 体系改造一起安排，因为它既是兼容性问题，也是被测页面可观察到的
自动化痕迹。其余项目可在公开快照 schema 版本升级时一并补齐。

### 不计入 20 项的已知操作面限制

BrowserKit 目前还没有原生 drag/drop、double-click、context-menu、按目标元素发送 key、
slider set-value 等高层工具。这些同样值得扩展，但属于动作 API 覆盖，而上面的 20 项刻意
聚焦本次目标：snapshot、动态页面理解、等待与由快照驱动的可靠操作。

## 复现与原始记录

机器可读的本次指标和操作结果保存在
[`snapshot-lab-2026-10-26.json`](results/snapshot-lab-2026-10-26.json)。
普通回归使用 `go test ./...`；完整真实 Chrome 回归使用 `make smoke`。

Codex 浏览器交互通道自身的 20 项欠缺、独立优先级和改进建议见
[`codex-browser-gap-analysis.md`](codex-browser-gap-analysis.md)。
