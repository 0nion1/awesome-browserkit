package browserkit

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"math"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// frameBinding 分别记录文档及 CDP session 身份；OOPIF 的文档与 iframe
// 宿主属于不同 session，嵌套坐标映射必须沿宿主链向上转换。
type frameBinding struct {
	FrameID     proto.PageFrameID
	ParentID    proto.PageFrameID
	LoaderID    proto.NetworkLoaderID
	ContextID   proto.RuntimeExecutionContextID
	Page        *rod.Page
	Owner       *rod.Page
	RootFrameID proto.PageFrameID
	Prefix      string
	URL         string
	Hidden      bool
	Unavailable string
}

func frameRefPrefixFor(frameID proto.PageFrameID, loaderID proto.NetworkLoaderID) string {
	sum := sha1.Sum([]byte(string(frameID) + "|" + string(loaderID)))
	return "f" + hex.EncodeToString(sum[:4])
}

// frameBindings 发现所有子文档，以共享的快照容量和操作时限约束采集。
func (page *rodPage) frameBindings(ctx context.Context) ([]frameBinding, error) {
	if page.released.Load() {
		return nil, newError("page_closed", "页面已关闭", "重新打开页面", true)
	}
	if err := lockInteraction(ctx, &page.frameScanMu); err != nil {
		return nil, err
	}
	defer page.frameScanMu.Unlock()
	bindings := map[proto.PageFrameID]frameBinding{}
	var order []proto.PageFrameID
	clients := []*rod.Page{page.page}
	addTree := func(client *rod.Page, tree *proto.PageFrameTree) {
		var walk func(*proto.PageFrameTree)
		walk = func(node *proto.PageFrameTree) {
			if node == nil || node.Frame == nil {
				return
			}
			frame := node.Frame
			if frame.ID != page.page.FrameID {
				previous, exists := bindings[frame.ID]
				if !exists {
					order = append(order, frame.ID)
				}
				binding := frameBinding{FrameID: frame.ID, ParentID: frame.ParentID, LoaderID: frame.LoaderID, Page: client, Owner: client, RootFrameID: client.FrameID, URL: frame.URL}
				if frame.ID == client.FrameID && client != page.page {
					binding.Owner, binding.RootFrameID = previous.Owner, previous.RootFrameID
					if binding.ParentID == "" {
						binding.ParentID = previous.ParentID
					}
				}
				bindings[frame.ID] = binding
			}
			for _, child := range node.ChildFrames {
				walk(child)
			}
		}
		walk(tree)
	}
	tree, err := (proto.PageGetFrameTree{}).Call(page.page.Context(ctx))
	if err != nil {
		return nil, err
	}
	if tree != nil {
		addTree(page.page, tree.FrameTree)
	}
	targets, err := (proto.TargetGetTargets{}).Call(page.page.Context(ctx))
	if err != nil {
		return nil, err
	}
	present := map[proto.TargetTargetID]bool{}
	var pending []*proto.TargetTargetInfo
	for _, info := range targets.TargetInfos {
		if info.Type == "iframe" {
			pending = append(pending, info)
			present[info.TargetID] = true
		}
	}
	// 先绑定父 target，才能在父 session 中解析嵌套 OOPIF 的宿主。
	for len(pending) > 0 && ctx.Err() == nil {
		var remaining []*proto.TargetTargetInfo
		progress := false
		for _, info := range pending {
			var ownerClient *rod.Page
			for _, client := range clients {
				if client.TargetID == info.TargetID {
					continue
				}
				owner, ownerErr := (proto.DOMGetFrameOwner{FrameID: proto.PageFrameID(info.TargetID)}).Call(client.Context(ctx))
				if ownerErr == nil && owner != nil {
					ownerClient = client
					break
				}
			}
			if ownerClient == nil {
				remaining = append(remaining, info)
				continue
			}
			page.frameMu.Lock()
			child := page.oopifs[info.TargetID]
			page.frameMu.Unlock()
			if child == nil {
				var childCancel context.CancelFunc
				child, childCancel, err = page.attachFrameTarget(ctx, info.TargetID)
				if err != nil {
					id := proto.PageFrameID(info.TargetID)
					binding, exists := bindings[id]
					if !exists {
						order = append(order, id)
					}
					binding.FrameID, binding.Owner, binding.RootFrameID, binding.URL = id, ownerClient, ownerClient.FrameID, info.URL
					binding.Unavailable = "frame_attach: " + err.Error()
					bindings[id] = binding
					continue
				}
				page.frameMu.Lock()
				if page.released.Load() {
					page.frameMu.Unlock()
					childCancel()
					page.dropOOPIF(ctx, info.TargetID, child, true)
					return nil, newError("page_closed", "页面已关闭", "重新打开页面", true)
				}
				if page.oopifs == nil {
					page.oopifs = map[proto.TargetTargetID]*rod.Page{}
				}
				page.oopifs[info.TargetID] = child
				if page.oopifCancels == nil {
					page.oopifCancels = map[proto.TargetTargetID]context.CancelFunc{}
				}
				page.oopifCancels[info.TargetID] = childCancel
				page.frameMu.Unlock()
			}
			id := child.FrameID
			binding, exists := bindings[id]
			if !exists {
				order = append(order, id)
			}
			binding.FrameID, binding.Page, binding.Owner, binding.RootFrameID, binding.URL = id, child, ownerClient, ownerClient.FrameID, info.URL
			bindings[id] = binding
			childTree, treeErr := (proto.PageGetFrameTree{}).Call(child.Context(ctx))
			if treeErr != nil {
				binding.Unavailable = treeErr.Error()
				bindings[id] = binding
			} else if childTree != nil {
				addTree(child, childTree.FrameTree)
			}
			clients = append(clients, child)
			progress = true
		}
		if !progress {
			break
		}
		pending = remaining
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var staleTargets []proto.TargetTargetID
	var stalePages []*rod.Page
	page.frameMu.Lock()
	for id, child := range page.oopifs {
		if !present[id] {
			staleTargets = append(staleTargets, id)
			stalePages = append(stalePages, child)
			delete(page.oopifs, id)
		}
	}
	page.frameMu.Unlock()
	for index, child := range stalePages {
		page.dropOOPIF(ctx, staleTargets[index], child, true)
	}

	result := make([]frameBinding, 0, len(order))
	for _, id := range order {
		binding := bindings[id]
		binding.Prefix = frameRefPrefixFor(binding.FrameID, binding.LoaderID)
		if binding.Owner == nil {
			binding.Unavailable = "frame owner unavailable"
		} else {
			owner, ownerErr := (proto.DOMGetFrameOwner{FrameID: id}).Call(binding.Owner.Context(ctx))
			if ownerErr != nil || owner == nil {
				binding.Unavailable = "frame owner unavailable"
			} else {
				binding.Hidden = !page.frameOwnerVisibleIn(ctx, binding.Owner, owner.BackendNodeID)
			}
		}
		result = append(result, binding)
	}
	for index := range result {
		for parentID := result[index].ParentID; parentID != ""; {
			parent, ok := bindings[parentID]
			if !ok {
				break
			}
			// 可见性取已完成宿主检查的条目。
			for _, candidate := range result {
				if candidate.FrameID == parentID && candidate.Hidden {
					result[index].Hidden = true
				}
			}
			parentID = parent.ParentID
		}
	}
	// 已移除或导航的 frame 文档不能保留旧引用。
	page.frameMu.Lock()
	oldLoaders := page.frameLoaders
	page.frameLoaders = map[proto.PageFrameID]proto.NetworkLoaderID{}
	active := map[proto.PageFrameID]bool{}
	for _, binding := range result {
		page.frameLoaders[binding.FrameID] = binding.LoaderID
		active[binding.FrameID] = true
	}
	for id := range page.frameWorlds {
		if !active[id] {
			delete(page.frameWorlds, id)
		}
	}
	page.frameMu.Unlock()
	page.stateMu.Lock()
	for id, loader := range oldLoaders {
		current, exists := bindings[id]
		if exists && current.LoaderID == loader {
			continue
		}
		prefix := frameRefPrefixFor(id, loader) + "_"
		for ref := range page.refs {
			if strings.HasPrefix(ref, prefix) {
				delete(page.refs, ref)
			}
		}
	}
	page.stateMu.Unlock()
	return result, nil
}

func (page *rodPage) attachFrameTarget(ctx context.Context, targetID proto.TargetTargetID) (*rod.Page, context.CancelFunc, error) {
	if page.owner != nil {
		return page.owner.pageFromTarget(ctx, targetID)
	}
	persistent, cancel := context.WithCancel(page.page.Browser().GetContext())
	stop := context.AfterFunc(ctx, cancel)
	child, err := page.page.Browser().Context(persistent).PageFromTarget(targetID)
	if !stop() {
		cancel()
		if ctx.Err() != nil {
			err = ctx.Err()
		}
	}
	if err != nil {
		cancel()
		return nil, func() {}, err
	}
	return child, cancel, nil
}

// dropOOPIF 清理未归属或失效的 child page，并 detach 已建立的 CDP session。
func (page *rodPage) dropOOPIF(ctx context.Context, targetID proto.TargetTargetID, child *rod.Page, created bool) {
	if !created {
		return
	}
	// 对 iframe target 调用 Page.Close 会返回 “Command can only be executed
	// on top-level targets”，必须显式 detach，否则 session 会一直保留。
	if sessionID := child.GetSessionID(); sessionID != "" {
		detachCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, _ = page.page.Browser().Call(detachCtx, "", "Target.detachFromTarget", proto.TargetDetachFromTarget{SessionID: sessionID})
		cancel()
	}
	page.page.Browser().RemoveState(targetID)
	page.frameMu.Lock()
	cancel := page.oopifCancels[targetID]
	delete(page.oopifCancels, targetID)
	page.frameMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// frameContext 复用 frame 的 isolated world；导航后 context 失效时重建。
func (page *rodPage) frameContext(ctx context.Context, frameID proto.PageFrameID) (proto.RuntimeExecutionContextID, bool) {
	page.frameMu.Lock()
	contextID := page.frameWorlds[frameID]
	page.frameMu.Unlock()
	if contextID != 0 && page.frameAlive(ctx, contextID) {
		return contextID, true
	}
	world, err := proto.PageCreateIsolatedWorld{FrameID: frameID, WorldName: "browserkit-frame"}.Call(page.page.Context(ctx))
	if err != nil || world == nil {
		return 0, false
	}
	page.frameMu.Lock()
	if page.frameWorlds == nil {
		page.frameWorlds = map[proto.PageFrameID]proto.RuntimeExecutionContextID{}
	}
	page.frameWorlds[frameID] = world.ExecutionContextID
	page.frameMu.Unlock()
	return world.ExecutionContextID, true
}

func (page *rodPage) frameAlive(ctx context.Context, contextID proto.RuntimeExecutionContextID) bool {
	result, err := proto.RuntimeEvaluate{ContextID: contextID, Expression: "1", ReturnByValue: true}.Call(page.page.Context(ctx))
	return err == nil && result != nil && result.Result != nil
}

// oopifContext 在 OOPIF 的 child session 中复用或创建 isolated world。
func (page *rodPage) oopifContext(ctx context.Context, binding frameBinding) (proto.RuntimeExecutionContextID, bool) {
	client := binding.Page.Context(ctx)
	page.frameMu.Lock()
	contextID := page.frameWorlds[binding.FrameID]
	page.frameMu.Unlock()
	if contextID != 0 {
		result, err := proto.RuntimeEvaluate{ContextID: contextID, Expression: "1", ReturnByValue: true}.Call(client)
		if err == nil && result != nil && result.Result != nil {
			return contextID, true
		}
	}
	world, err := proto.PageCreateIsolatedWorld{FrameID: binding.FrameID, WorldName: "browserkit-frame"}.Call(client)
	if err != nil || world == nil {
		return 0, false
	}
	page.frameMu.Lock()
	if page.frameWorlds == nil {
		page.frameWorlds = map[proto.PageFrameID]proto.RuntimeExecutionContextID{}
	}
	page.frameWorlds[binding.FrameID] = world.ExecutionContextID
	page.frameMu.Unlock()
	return world.ExecutionContextID, true
}

const frameFindScript = `function(selector) {
	const find = root => {
		for (const el of root.querySelectorAll(selector)) {
			const ref = el.getAttribute('data-browserkit-ref');
			const state = window.__browserkit_page_state_v1__;
			if (!selector.startsWith('[data-browserkit-ref=') || state?.nodes.get(ref) === el) return el;
		}
		for (const el of root.querySelectorAll('*')) {
			if (el.shadowRoot) { const found = find(el.shadowRoot); if (found) return found; }
		}
		return null;
	};
	return find(document);
}`

func frameFindExpression(selector string) string {
	return "(" + frameFindScript + ")(" + selector + ")"
}

func frameRefExpression(ref string) string {
	return frameFindExpression(strconvQuote(`[data-browserkit-ref="` + ref + `"]`))
}

// frameEvaluate 在指定 frame 中执行表达式并返回 JSON。
func (page *rodPage) frameEvaluate(ctx context.Context, binding frameBinding, expression string) (json.RawMessage, bool) {
	var result *proto.RuntimeEvaluateResult
	var err error
	if binding.Page != nil {
		// OOPIF 也必须使用 isolated world，避免页面脚本篡改快照和坐标。
		contextID, ok := page.oopifContext(ctx, binding)
		if !ok {
			return nil, false
		}
		result, err = proto.RuntimeEvaluate{ContextID: contextID, Expression: expression, ReturnByValue: true, AwaitPromise: true}.Call(binding.Page.Context(ctx))
	} else {
		contextID, ok := page.frameContext(ctx, binding.FrameID)
		if !ok {
			return nil, false
		}
		result, err = proto.RuntimeEvaluate{ContextID: contextID, Expression: expression, ReturnByValue: true, AwaitPromise: true}.Call(page.page.Context(ctx))
	}
	if err != nil || result == nil || result.Result == nil || result.ExceptionDetails != nil {
		return nil, false
	}
	data, marshalErr := result.Result.Value.MarshalJSON()
	if marshalErr != nil {
		return nil, false
	}
	return json.RawMessage(data), true
}

// frameBindingForRef 解析 ref 前缀并找到对应 frame。
func (page *rodPage) frameBindingForRef(ctx context.Context, ref string) (frameBinding, bool) {
	if !isFrameRef(ref) {
		return frameBinding{}, false
	}
	prefix := ref
	if index := strings.IndexByte(ref, '_'); index > 0 {
		prefix = ref[:index]
	}
	frames, err := page.frameBindings(ctx)
	if err != nil {
		return frameBinding{}, false
	}
	for _, frame := range frames {
		if frame.Hidden || frame.Unavailable != "" {
			continue
		}
		if frame.Prefix != prefix {
			continue
		}
		if frame.Page == nil {
			contextID, ok := page.frameContext(ctx, frame.FrameID)
			if !ok {
				return frameBinding{}, false
			}
			frame.ContextID = contextID
		}
		return frame, true
	}
	return frameBinding{}, false
}

func strconvQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func itoa(value int) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// validateFrameRef 校验跨域 ref 的 revision 与当前快照一致。
func (page *rodPage) validateFrameRef(ctx context.Context, ref ElementRef) error {
	if err := page.syncState(ctx); err != nil {
		return err
	}
	page.stateMu.Lock()
	defer page.stateMu.Unlock()
	if ref.Ref == "" || ref.Revision == 0 || ref.Revision != page.revision {
		return newError("stale_reference", "元素引用已经过期", "重新调用 browserSnapshot 获取当前 ref", true)
	}
	if _, ok := page.refs[ref.Ref]; !ok {
		return newError("invalid_reference", "当前快照不存在该元素引用", "重新调用 browserSnapshot", true)
	}
	return nil
}

// frameElementPoint 沿 CDP session 宿主链映射坐标。支持缩放、旋转、skew
// 和镜像等仿射变换，在派发输入前拒绝无法可靠映射的透视四边形。
func (page *rodPage) frameElementPoint(ctx context.Context, binding frameBinding, ref string) (float64, float64, error) {
	expression := `(() => {
		const el = ` + frameRefExpression(ref) + `;
		if (!el) return null;
		el.scrollIntoView({behavior: 'instant', block: 'center', inline: 'center'});
		const rect = el.getBoundingClientRect();
		const x = rect.x + rect.width / 2, y = rect.y + rect.height / 2;
		let hit = document.elementFromPoint(x, y);
		for (;;) { const next = hit?.shadowRoot?.elementFromPoint(x, y); if (!next || next === hit) break; hit = next; }
		return {x, y, width: rect.width, height: rect.height, inViewport: x >= 0 && y >= 0 && x < innerWidth && y < innerHeight, covered: !hit || hit !== el && !el.contains(hit)};
	})()`
	raw, ok := page.frameEvaluate(ctx, binding, expression)
	var point struct {
		X          float64 `json:"x"`
		Y          float64 `json:"y"`
		Width      float64 `json:"width"`
		Height     float64 `json:"height"`
		InViewport bool    `json:"inViewport"`
		Covered    bool    `json:"covered"`
	}
	if !ok || json.Unmarshal(raw, &point) != nil || point.Width <= 0 || point.Height <= 0 {
		return 0, 0, newError("stale_reference", "frame 元素已移除或不可见", "重新调用 browserSnapshot", true)
	}
	if !point.InViewport {
		return 0, 0, newError("element_out_of_frame", "元素中心不在 frame 可视区域内", "滚动 frame 后重试", true)
	}
	if point.Covered {
		return 0, 0, newError("frame_unavailable", "frame 内的目标被遮挡", "关闭遮挡后重试", true)
	}
	frames, err := page.frameBindings(ctx)
	if err != nil {
		return 0, 0, err
	}
	byID := map[proto.PageFrameID]frameBinding{}
	for _, frame := range frames {
		byID[frame.FrameID] = frame
	}
	originalID := binding.FrameID
	x, y := point.X, point.Y
	for {
		if binding.Owner == nil {
			return 0, 0, newError("frame_unavailable", "frame 父文档不可用", "重新调用 browserSnapshot", true)
		}
		client := binding.Owner.Context(ctx)
		owner, ownerErr := (proto.DOMGetFrameOwner{FrameID: binding.FrameID}).Call(client)
		if ownerErr != nil || owner == nil {
			return 0, 0, newError("frame_unavailable", "frame 已失效", "重新调用 browserSnapshot", true)
		}
		if err := (proto.DOMScrollIntoViewIfNeeded{BackendNodeID: owner.BackendNodeID}).Call(client); err != nil {
			return 0, 0, err
		}
		box, boxErr := (proto.DOMGetBoxModel{BackendNodeID: owner.BackendNodeID}).Call(client)
		width, height, viewportOK := page.frameViewport(ctx, binding)
		if boxErr != nil || box == nil || box.Model == nil || !viewportOK || width <= 0 || height <= 0 {
			return 0, 0, newError("frame_unavailable", "无法读取 frame 几何位置", "重新调用 browserSnapshot", true)
		}
		var supported bool
		x, y, supported = mapFramePoint(box.Model.Content, width, height, x, y)
		if !supported {
			return 0, 0, newError("frame_transform_unsupported", "frame 使用了无法可靠映射的透视变形", "使用无透视变形的 frame", true)
		}
		metrics, metricErr := (proto.PageGetLayoutMetrics{}).Call(client)
		if metricErr != nil || metrics == nil || metrics.CSSLayoutViewport == nil {
			return 0, 0, newError("frame_unavailable", "无法读取父视口", "重新调用 browserSnapshot", true)
		}
		viewport := metrics.CSSLayoutViewport
		if x < 0 || y < 0 || x >= float64(viewport.ClientWidth) || y >= float64(viewport.ClientHeight) {
			return 0, 0, newError("frame_unavailable", "点击位置超出父视口", "滚动页面后重试", true)
		}
		hit, hitErr := (proto.DOMGetNodeForLocation{X: int(x) + viewport.PageX, Y: int(y) + viewport.PageY}).Call(client)
		if hitErr != nil || hit == nil || (hit.BackendNodeID != owner.BackendNodeID && hit.FrameID != binding.FrameID && hit.FrameID != originalID) {
			return 0, 0, newError("frame_unavailable", "frame 被遮挡或裁剪", "滚动页面或移除遮挡后重试", true)
		}
		if binding.RootFrameID == page.page.FrameID {
			return x, y, nil
		}
		parent, exists := byID[binding.RootFrameID]
		if !exists || parent.FrameID == binding.FrameID {
			return 0, 0, newError("frame_unavailable", "嵌套 frame 父文档已变化", "重新调用 browserSnapshot", true)
		}
		binding = parent
	}
}

func mapFramePoint(quad []float64, width, height, x, y float64) (float64, float64, bool) {
	if len(quad) != 8 || width <= 0 || height <= 0 {
		return 0, 0, false
	}
	// 仿射四边形的对边必须平行。
	if math.Abs(quad[4]-(quad[2]+quad[6]-quad[0])) > 0.5 || math.Abs(quad[5]-(quad[3]+quad[7]-quad[1])) > 0.5 {
		return 0, 0, false
	}
	det := (quad[2]-quad[0])*(quad[7]-quad[1]) - (quad[3]-quad[1])*(quad[6]-quad[0])
	if math.Abs(det) < 0.01 {
		return 0, 0, false
	}
	return quad[0] + x/width*(quad[2]-quad[0]) + y/height*(quad[6]-quad[0]), quad[1] + x/width*(quad[3]-quad[1]) + y/height*(quad[7]-quad[1]), true
}

// clickFrameElement 通过 frame 内坐标和 CDP 输入事件点击跨域元素。
func (page *rodPage) clickFrameElement(ctx context.Context, binding frameBinding, ref string, options ClickOptions) error {
	button, count, err := clickParameters(options)
	if err != nil {
		return err
	}
	x, y, err := page.frameElementPoint(ctx, binding, ref)
	if err != nil {
		return err
	}
	browserCtx := page.page.Context(ctx)
	for index := 0; index < count; index++ {
		if err := (proto.InputDispatchMouseEvent{Type: proto.InputDispatchMouseEventTypeMouseMoved, X: x, Y: y}).Call(browserCtx); err != nil {
			return err
		}
		clickCount := index + 1
		if err := (proto.InputDispatchMouseEvent{Type: proto.InputDispatchMouseEventTypeMousePressed, X: x, Y: y, Button: button, ClickCount: clickCount}).Call(browserCtx); err != nil {
			return err
		}
		if err := (proto.InputDispatchMouseEvent{Type: proto.InputDispatchMouseEventTypeMouseReleased, X: x, Y: y, Button: button, ClickCount: clickCount}).Call(browserCtx); err != nil {
			return err
		}
	}
	return nil
}

// typeFrameElement 与主文档复用编辑性和原生特殊输入校验。
func (page *rodPage) typeFrameElement(ctx context.Context, binding frameBinding, ref ElementRef, text string, clear bool) error {
	client, objectID, err := page.formFrameElement(ctx, binding, ref.Ref)
	if err != nil {
		return err
	}
	defer func() { _ = (proto.RuntimeReleaseObject{ObjectID: objectID}).Call(client) }()
	element, err := client.ElementFromObject(&proto.RuntimeRemoteObject{ObjectID: objectID})
	if err != nil {
		return err
	}
	kind, err := editableInputType(ctx, element)
	if err != nil {
		return err
	}
	if isValueInputType(kind) {
		if err := validateValueInput(kind, text); err != nil {
			return err
		}
		return setValueInput(ctx, element, kind, text, kind == "number" && !clear)
	}
	result, err := callFormElement(ctx, element, `function(input) {
		const old = this.isContentEditable ? (this.innerText || '') : this.value;
		const value = input.clear ? input.text : old + input.text;
		this.focus({preventScroll: true});
		if (this.isContentEditable) this.innerText = value;
		else {
			const descriptor = Object.getOwnPropertyDescriptor(Object.getPrototypeOf(this), 'value');
			if (descriptor?.set) descriptor.set.call(this, value); else this.value = value;
		}
		this.dispatchEvent(new Event('input', {bubbles: true, composed: true}));
		this.dispatchEvent(new Event('change', {bubbles: true, composed: true}));
		const actual = this.isContentEditable ? (this.innerText || '') : this.value;
		return {ok: actual === value};
	}`, map[string]any{"text": text, "clear": clear})
	if err != nil {
		return err
	}
	var state struct {
		OK bool `json:"ok"`
	}
	if err := result.Value.Unmarshal(&state); err != nil {
		return err
	}
	if !state.OK {
		return newError("invalid_value", "frame 控件没有接受该值", "检查控件类型和允许的值", true)
	}
	return nil
}

// hasFrameRef 在跨域 frame 内检查 ref 是否仍存在。
func (page *rodPage) hasFrameRef(ctx context.Context, binding frameBinding, ref string) (bool, error) {
	expression := frameRefExpression(ref) + ` !== null`
	raw, ok := page.frameEvaluate(ctx, binding, expression)
	if !ok {
		return false, nil
	}
	var found bool
	if err := json.Unmarshal(raw, &found); err != nil {
		return false, err
	}
	return found, nil
}

// refFromSelector 从 ref 属性选择器中提取 ref。
func refFromSelector(selector string) (string, bool) {
	const prefix = `[data-browserkit-ref="`
	if !strings.HasPrefix(selector, prefix) || !strings.HasSuffix(selector, `"]`) {
		return "", false
	}
	ref := selector[len(prefix) : len(selector)-2]
	if ref == "" || strings.ContainsAny(ref, `"\\`) {
		return "", false
	}
	return ref, true
}

// setFrameChecked 在跨域 frame 内切换 checkbox/radio/switch。
func (page *rodPage) setFrameChecked(ctx context.Context, binding frameBinding, ref ElementRef, checked bool) (bool, error) {
	readExpression := `(() => {
		const el = ` + frameRefExpression(ref.Ref) + `;
		if (!el) return {error: 'stale_reference'};
		const native = el.tagName === 'INPUT' && (el.type === 'checkbox' || el.type === 'radio');
		const aria = el.getAttribute('aria-checked');
		const supported = native || aria !== null;
		const current = native ? el.checked : aria === 'true';
		return {supported: supported, checked: current, radio: el.type === 'radio', mixed: aria === 'mixed'};
	})()`
	read := func() (struct {
		Supported bool   `json:"supported"`
		Checked   bool   `json:"checked"`
		Radio     bool   `json:"radio"`
		Mixed     bool   `json:"mixed"`
		Error     string `json:"error"`
	}, error) {
		var state struct {
			Supported bool   `json:"supported"`
			Checked   bool   `json:"checked"`
			Radio     bool   `json:"radio"`
			Mixed     bool   `json:"mixed"`
			Error     string `json:"error"`
		}
		raw, ok := page.frameEvaluate(ctx, binding, readExpression)
		if !ok {
			return state, newError("stale_reference", "跨域 frame 中已找不到该元素", "重新调用 browserSnapshot", true)
		}
		if err := json.Unmarshal(raw, &state); err != nil {
			return state, err
		}
		if state.Error != "" {
			return state, newError("stale_reference", "跨域 frame 中已找不到该元素", "重新调用 browserSnapshot", true)
		}
		return state, nil
	}
	state, err := read()
	if err != nil {
		return false, err
	}
	if !state.Supported || state.Radio && !checked {
		return false, newError("invalid_check_target", "目标必须是可勾选控件，radio 不能取消选中", "使用快照中的 checkbox、radio 或 switch", true)
	}
	if state.Checked == checked && !state.Mixed {
		return false, nil
	}
	if err := page.clickFrameElement(ctx, binding, ref.Ref, ClickOptions{}); err != nil {
		return false, err
	}
	after, err := read()
	if err != nil {
		return true, err
	}
	if after.Checked != checked {
		return false, newError("check_not_applied", "切换后状态与请求不一致", "重新调用 browserSnapshot 确认控件状态", true)
	}
	return false, nil
}

// refFromSelectorValue 返回 ref 属性选择器中的 ref；调用方已确认格式。
func refFromSelectorValue(selector string) string {
	ref, _ := refFromSelector(selector)
	return ref
}

// frameRefVisible 检查跨域 frame 内的 ref 是否可见。
func (page *rodPage) frameRefVisible(ctx context.Context, binding frameBinding, ref string) bool {
	expression := `(() => {
		const el = ` + frameRefExpression(ref) + `;
		if (!el) return false;
		const style = getComputedStyle(el);
		const rect = el.getBoundingClientRect();
		return !el.hidden && style.display !== 'none' && style.visibility !== 'hidden' && Number(style.opacity) > 0 && rect.width > 0 && rect.height > 0;
	})()`
	raw, ok := page.frameEvaluate(ctx, binding, expression)
	if !ok {
		return false
	}
	var visible bool
	if err := json.Unmarshal(raw, &visible); err != nil {
		return false
	}
	return visible
}

// isFrameRef 判断 ref 是否使用跨域 frame 前缀。
func isFrameRef(ref string) bool {
	if len(ref) < 10 || ref[0] != 'f' || ref[9] != '_' {
		return false
	}
	for index := 1; index < 9; index++ {
		ch := ref[index]
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

// focusFrameElement 在跨域 frame 内聚焦元素。
func (page *rodPage) focusFrameElement(ctx context.Context, binding frameBinding, ref string) error {
	expression := `(() => {
		const el = ` + frameRefExpression(ref) + `;
		if (!el) return {error: 'stale_reference'};
		el.focus({preventScroll: true});
		const active = el.getRootNode().activeElement;
		return {ok: active === el};
	})()`
	raw, ok := page.frameEvaluate(ctx, binding, expression)
	if !ok {
		return newError("stale_reference", "跨域 frame 中已找不到该元素", "重新调用 browserSnapshot", true)
	}
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return err
	}
	if result.Error == "stale_reference" {
		return newError("stale_reference", "跨域 frame 中已找不到该元素", "重新调用 browserSnapshot", true)
	}
	if !result.OK {
		return newError("element_not_focusable", "跨域元素无法聚焦", "重新调用 browserSnapshot 选择可聚焦控件", true)
	}
	return nil
}

// frameObject 在跨域 frame 中求值并返回远端对象。
func (page *rodPage) frameObject(ctx context.Context, binding frameBinding, expression string) (proto.RuntimeRemoteObjectID, bool) {
	var result *proto.RuntimeEvaluateResult
	var err error
	if binding.Page != nil {
		contextID, ok := page.oopifContext(ctx, binding)
		if !ok {
			return "", false
		}
		result, err = proto.RuntimeEvaluate{ContextID: contextID, Expression: expression, ReturnByValue: false}.Call(binding.Page.Context(ctx))
	} else {
		contextID, ok := page.frameContext(ctx, binding.FrameID)
		if !ok {
			return "", false
		}
		result, err = proto.RuntimeEvaluate{ContextID: contextID, Expression: expression, ReturnByValue: false}.Call(page.page.Context(ctx))
	}
	if err != nil || result == nil || result.Result == nil || result.ExceptionDetails != nil || result.Result.ObjectID == "" {
		return "", false
	}
	return result.Result.ObjectID, true
}

// frameViewport 返回 frame 的布局视口宽度，用于检测 transform/zoom。
func (page *rodPage) frameViewport(ctx context.Context, binding frameBinding) (float64, float64, bool) {
	raw, ok := page.frameEvaluate(ctx, binding, `(() => ({width: window.innerWidth, height: window.innerHeight}))()`)
	if !ok {
		return 0, 0, false
	}
	var viewport struct {
		Width  float64 `json:"width"`
		Height float64 `json:"height"`
	}
	if err := json.Unmarshal(raw, &viewport); err != nil {
		return 0, 0, false
	}
	return viewport.Width, viewport.Height, true
}

// closeOOPIFs 释放所有 OOPIF child page；用于页面释放和快照清理。
func (page *rodPage) closeOOPIFs() {
	page.frameMu.Lock()
	stale := make([]*rod.Page, 0, len(page.oopifs))
	targets := make([]proto.TargetTargetID, 0, len(page.oopifs))
	for targetID, child := range page.oopifs {
		stale = append(stale, child)
		targets = append(targets, targetID)
		delete(page.oopifs, targetID)
	}
	page.frameMu.Unlock()
	for index, child := range stale {
		page.dropOOPIF(context.Background(), targets[index], child, true)
	}
}

// frameOwnerVisible 检查 iframe 宿主元素本身是否可见。
func (page *rodPage) frameOwnerVisibleIn(ctx context.Context, client *rod.Page, backendNodeID proto.DOMBackendNodeID) bool {
	if backendNodeID == 0 {
		return true
	}
	node, err := proto.DOMResolveNode{BackendNodeID: backendNodeID}.Call(client.Context(ctx))
	if err != nil || node == nil || node.Object == nil || node.Object.ObjectID == "" {
		// 嵌套跨进程 frame 的 owner 可能无法从主 world 解析；保守保留内容。
		return true
	}
	result, err := proto.RuntimeCallFunctionOn{
		ObjectID: node.Object.ObjectID,
		FunctionDeclaration: `function() {
			const clipped = (s) => {
				const value = s.clipPath;
				if (!value || value === 'none') return false;
				const match = value.match(/inset\(([^)]*)\)/);
				if (!match) return false;
				const parts = match[1].trim().split(/\s+/).filter(Boolean);
				const pct = (part) => part.endsWith('%') ? parseFloat(part) : (parseFloat(part) === 0 ? 0 : NaN);
				if (parts.length === 0) return false;
				const values = parts.length === 1 ? [parts[0], parts[0], parts[0], parts[0]]
					: parts.length === 2 ? [parts[0], parts[1], parts[0], parts[1]]
					: parts.length === 3 ? [parts[0], parts[1], parts[2], parts[1]] : parts;
				const [top, right, bottom, left] = values.map(pct);
				if ([top, right, bottom, left].some(Number.isNaN)) return false;
				return top + bottom >= 100 || left + right >= 100;
			};
			// visibility 可被子元素覆盖，取元素自身的最终值；其余属性
			// 无法被后代覆盖，必须沿祖先链检查。
			const self = getComputedStyle(this);
			if (self.visibility === 'hidden' || self.visibility === 'collapse') return {visible: false};
			for (let el = this; el;) {
				const s = getComputedStyle(el);
				if (s.display === 'none') return {visible: false};
				if (s.contentVisibility === 'hidden') return {visible: false};
				if (parseFloat(s.opacity) < 0.05) return {visible: false};
				if (clipped(s)) return {visible: false};
				if (el.parentElement) { el = el.parentElement; continue; }
				const root = el.getRootNode ? el.getRootNode() : null;
				if (root && root.host) { el = root.host; continue; }
				break;
			}
			const rect = this.getBoundingClientRect();
			const vw = window.innerWidth, vh = window.innerHeight;
			if (rect.width <= 1 || rect.height <= 1) return {visible: false};
			if (rect.right < -1000 || rect.bottom < -1000 || rect.left > vw + 5000 || rect.top > vh + 5000) return {visible: false};
			return {visible: true};
		}`,
		ReturnByValue: true,
	}.Call(client.Context(ctx))
	if err != nil || result == nil || result.Result == nil || result.ExceptionDetails != nil {
		return true
	}
	var visibility struct {
		Visible bool `json:"visible"`
	}
	if err := result.Result.Value.Unmarshal(&visibility); err != nil {
		return true
	}
	return visibility.Visible
}
