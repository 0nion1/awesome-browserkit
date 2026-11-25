# frame 与 Shadow DOM 的 SDK 边界

现有 `Snapshot` 和元素操作直接覆盖嵌套同源、跨域 iframe/OOPIF 及其开放 Shadow DOM，不增加另一套 frame 工具或由宿主换算坐标。`page_id` 仍标识顶层 Tab；`revision` 仍随顶层文档变化。子文档引用带 frame/loader 身份前缀，导航、移除或节点替换后不能静默指向新节点。跨域密码框有可操作 ref，但不返回密码值。

`page_frame.go` 通过 `Page.getFrameTree` 发现同进程文档，再以父 session 中的 `DOM.getFrameOwner` 核实 OOPIF 归属，逐层绑定。没有固定 frame 数量、attach 数量或可见性检查次数上限。内部 child session 继承 session 寿命；单次调用取消不会取消这些绑定。移除 frame 或关闭 Tab 时释放绑定与临时状态。

每个文档复用 `page_snapshot.go` 中的同一采集脚本，正文与控件均递归开放 Shadow DOM。文字、元素和字段值预算作用于整个 Tab。元素按一个全局 offset 分页，`next_offset` 可继续读取后续 frame，而不是只在第一页追加一部分跨域元素。

`frame_coverage` 为每个已发现文档返回 Chrome frame 身份和 URL：

| status | 含义 |
| --- | --- |
| `captured` | 当前预算内完成该文档采集 |
| `partial` | `reason=snapshot_budget`，文字或元素没有完全返回；元素可分页续读 |
| `hidden` | iframe 宿主或祖先不可见，未采集内容 |
| `unavailable` | 文档、session 或执行上下文不可用；原因随回执返回 |

`frame_id="*"` 的 unavailable 条目表示 frame 发现失败，无法断言已经列出所有后代。每个条目的 `shadow_dom=open_only` 都明确闭合 Shadow DOM 不在观察范围；captured 不能解释成完整 DOM。

## 变换和实际命中

点击、悬停和拖放从元素中心出发，用 `DOM.getBoxModel` 的 content quad 映射 frame 坐标，沿 OOPIF session 宿主链转换至顶层视口。支持非退化仿射缩放、旋转、镜像和 skew；`DOM.getNodeForLocation` 在每层父视口核对命中，裁剪或遮挡导致无法命中时返回 `frame_unavailable`，不会派发到父页面。元素中心不可见时不会寻找并猜测另一命中点。

透视四边形或退化尺寸返回 `frame_transform_unsupported`。动态 transform 在采样与派发之间改变，以及复杂 3D 合成组合没有完整验证；不能宣称任意 CSS 变换均可操作。

可控最小复现：

```html
<iframe src="https://other-origin.test/form"
  style="width:400px;height:240px;
         transform:perspective(300px) rotateY(30deg)"></iframe>
```

此 frame 的快照仍可见，真实点击会在派发前返回透视变形错误。`TestRealBrowserCrossFramePerspectiveRejected` 验证该边界。`TestRealBrowserAffineFrameClicks` 实测 scale、rotate(20deg/180deg)、scaleX(-1) 和 skewX；`TestRealBrowserClosedShadowScopeAndClipping` 验证点击中心被 clip-path 裁剪时明确拒绝。

## 闭合 Shadow DOM

```html
<div id="host"></div>
<script>
  document.getElementById('host').attachShadow({mode:'closed'})
    .innerHTML = '<button>Closed secret</button>';
</script>
```

开放根采集和安全 ref 路径无法进入上述闭合根；SDK 不为其中的按钮生成 ref，回执仍标明 `open_only`。Chromium 的 DOM 调试协议具有穿透部分闭合根的能力，但当前架构未把调试节点身份与所有动作、文档失效和跨进程操作可靠贯通，因此没有将这种能力伪装为完整支持。该最小复现及实际缺失由 `TestRealBrowserClosedShadowScopeAndClipping` 验证。

## 已执行的定向验证

容器内 Go 1.25.14 / Chromium 156（linux/arm64），`browser_smoke` 用例覆盖两层跨站 OOPIF 的开放 Shadow DOM、密码输入和真实点击、10 个业务子 frame、全 Tab 元素分页、预算覆盖回执、节点克隆导致旧 ref 失效、导航失效、已有引用跨分页保留、隐藏宿主、页面滚动及遮挡。命令与结果见 [SDK 验证记录](sdk-baseline-review.md)。linux/amd64 的真实浏览器行为及更长时运行未执行验证。
