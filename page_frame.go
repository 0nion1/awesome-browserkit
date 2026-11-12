package browserkit

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// maxCrossFrames 限制单次快照聚合的跨域 frame 数量；广告和统计 iframe 通常不需要操作。
const maxCrossFrames = 3

// frameBinding 记录一个跨域 frame。同站点跨域使用 isolated world context，
// 跨站 OOPIF 使用独立的 target page。
type frameBinding struct {
	FrameID   proto.PageFrameID
	LoaderID  proto.NetworkLoaderID
	ContextID proto.RuntimeExecutionContextID
	Page      *rod.Page
	Prefix    string
	URL       string
}

// frameRefPrefix 为 frame 生成短且稳定的 ref 前缀。loaderID 在跨文档
// 导航后变化，使旧文档的 ref 不会在新文档中复活。
func frameRefPrefixFor(frameID proto.PageFrameID, loaderID proto.NetworkLoaderID) string {
	sum := sha1.Sum([]byte(string(frameID) + "|" + string(loaderID)))
	return "f" + hex.EncodeToString(sum[:4])
}

// frameBindings 返回当前页面的跨域子 frame，最多 maxCrossFrames 个。
func (page *rodPage) frameBindings(ctx context.Context) ([]frameBinding, error) {
	tree, err := proto.PageGetFrameTree{}.Call(page.page.Context(ctx))
	if err != nil || tree == nil || tree.FrameTree == nil {
		return nil, err
	}
	mainURL := ""
	if tree.FrameTree.Frame != nil {
		mainURL = tree.FrameTree.Frame.URL
	}
	var frames []*proto.PageFrame
	var walk func(node *proto.PageFrameTree, parentIsMain bool)
	walk = func(node *proto.PageFrameTree, parentIsMain bool) {
		if node == nil {
			return
		}
		for _, child := range node.ChildFrames {
			if child == nil || child.Frame == nil || child.Frame.ID == page.page.FrameID {
				continue
			}
			// 主文档脚本已经遍历同源 iframe，这里跳过；跨域 frame 内部
			// 的同源子 frame 不在主文档脚本范围内，仍需单独聚合。
			if parentIsMain && sameOrigin(child.Frame.URL, mainURL) {
				walk(child, false)
				continue
			}
			frames = append(frames, child.Frame)
			walk(child, false)
		}
	}
	walk(tree.FrameTree, true)
	bindings := make([]frameBinding, 0, len(frames)+maxCrossFrames)
	seen := map[proto.PageFrameID]bool{}
	for _, frame := range frames {
		if seen[frame.ID] {
			continue
		}
		seen[frame.ID] = true
		bindings = append(bindings, frameBinding{FrameID: frame.ID, LoaderID: frame.LoaderID, URL: frame.URL})
	}
	// 跨站 iframe 在独立进程中，Page.getFrameTree 看不到，需要从 target 列表发现。
	for _, binding := range page.oopifBindings(ctx) {
		if seen[binding.FrameID] {
			continue
		}
		seen[binding.FrameID] = true
		bindings = append(bindings, binding)
	}
	// frame 文档导航后旧 ref 必须失效，避免同一 ref 命中新文档的不同控件。
	var invalidated []frameBinding
	page.frameMu.Lock()
	if page.frameLoaders == nil {
		page.frameLoaders = map[proto.PageFrameID]proto.NetworkLoaderID{}
	}
	for _, binding := range bindings {
		previous, existed := page.frameLoaders[binding.FrameID]
		if existed && previous != binding.LoaderID {
			invalidated = append(invalidated, frameBinding{FrameID: binding.FrameID, LoaderID: previous})
		}
		page.frameLoaders[binding.FrameID] = binding.LoaderID
	}
	page.frameMu.Unlock()
	if len(invalidated) > 0 {
		page.stateMu.Lock()
		for _, stale := range invalidated {
			prefix := frameRefPrefixFor(stale.FrameID, stale.LoaderID) + "_"
			for ref := range page.refs {
				if strings.HasPrefix(ref, prefix) {
					delete(page.refs, ref)
				}
			}
		}
		page.stateMu.Unlock()
	}
	// 跳过零尺寸、隐藏或屏外的宿主 frame，避免按错误坐标点击。
	type visibleBinding struct {
		binding frameBinding
		area    float64
	}
	var visible []visibleBinding
	for _, binding := range bindings {
		owner, ownerErr := proto.DOMGetFrameOwner{FrameID: binding.FrameID}.Call(page.page.Context(ctx))
		if ownerErr != nil || owner == nil {
			continue
		}
		box, boxErr := proto.DOMGetBoxModel{BackendNodeID: owner.BackendNodeID}.Call(page.page.Context(ctx))
		if boxErr != nil || box == nil || box.Model == nil || len(box.Model.Content) < 8 {
			continue
		}
		if box.Model.Width <= 1 || box.Model.Height <= 1 {
			continue
		}
		left, top := box.Model.Content[0], box.Model.Content[1]
		if left+float64(box.Model.Width) < -1000 || top+float64(box.Model.Height) < -1000 {
			continue
		}
		// box model 不会反映 visibility/opacity；隐藏宿主下的跨域内容不应进入快照。
		if !page.frameOwnerVisible(ctx, owner.BackendNodeID) {
			continue
		}
		binding.Prefix = frameRefPrefixFor(binding.FrameID, binding.LoaderID)
		visible = append(visible, visibleBinding{binding: binding, area: float64(box.Model.Width) * float64(box.Model.Height)})
	}
	// 广告 iframe 常常排在目标前面；按面积优先保留更可能承载内容的 frame。
	sort.SliceStable(visible, func(left, right int) bool { return visible[left].area > visible[right].area })
	bindings = bindings[:0]
	for index, item := range visible {
		if index >= maxCrossFrames {
			break
		}
		bindings = append(bindings, item.binding)
	}
	// 已关闭 frame 的 isolated world 不再需要。
	page.frameMu.Lock()
	if page.frameWorlds != nil {
		active := make(map[proto.PageFrameID]bool, len(bindings))
		for _, binding := range bindings {
			active[binding.FrameID] = true
		}
		for id := range page.frameWorlds {
			if !active[id] {
				delete(page.frameWorlds, id)
			}
		}
	}
	page.frameMu.Unlock()
	return bindings, nil
}

// oopifBindings 发现属于当前页面的跨站 iframe target。
func (page *rodPage) oopifBindings(ctx context.Context) []frameBinding {
	targets, err := proto.TargetGetTargets{}.Call(page.page.Context(ctx))
	if err != nil || targets == nil {
		return nil
	}
	var bindings []frameBinding
	present := map[proto.TargetTargetID]bool{}
	for _, info := range targets.TargetInfos {
		if info.Type == "iframe" {
			present[info.TargetID] = true
		}
	}
	type candidate struct {
		info  *proto.TargetTargetInfo
		child *rod.Page
		owner proto.DOMBackendNodeID
		area  float64
	}
	var candidates []candidate
	for _, info := range targets.TargetInfos {
		if info.Type != "iframe" {
			continue
		}
		page.frameMu.Lock()
		child := page.oopifs[info.TargetID]
		page.frameMu.Unlock()
		if child != nil {
			candidates = append(candidates, candidate{info: info, child: child, area: math.MaxFloat64})
			continue
		}
		// 先用当前页的 frame tree 做归属预检，避免为其它 Tab 的 iframe
		// attach 出无法关闭的 CDP session。
		owner, ownerErr := (proto.DOMGetFrameOwner{FrameID: proto.PageFrameID(info.TargetID)}).Call(page.page.Context(ctx))
		if ownerErr != nil || owner == nil {
			continue
		}
		// attach 前就能拿到 owner box，用面积预排序，优先 attach 更可能
		// 承载内容的 frame，广告 iframe 不再挤占 attach 预算。
		area := 0.0
		if box, boxErr := (proto.DOMGetBoxModel{BackendNodeID: owner.BackendNodeID}).Call(page.page.Context(ctx)); boxErr == nil && box != nil && box.Model != nil {
			area = float64(box.Model.Width) * float64(box.Model.Height)
		}
		candidates = append(candidates, candidate{info: info, owner: owner.BackendNodeID, area: area})
	}
	sort.SliceStable(candidates, func(left, right int) bool { return candidates[left].area > candidates[right].area })
	attachBudget := 6
	visibilityChecks := 0
	for _, item := range candidates {
		child := item.child
		created := false
		if child == nil {
			if attachBudget <= 0 {
				continue
			}
			// 隐藏的大广告 iframe 不应占用 attach 预算；可见性在 attach
			// 前就能从父页面 owner 判断。
			if visibilityChecks >= 12 {
				continue
			}
			visibilityChecks++
			if item.area <= 1 || !page.frameOwnerVisible(ctx, item.owner) {
				continue
			}
			attachBudget--
			createdPage, createErr := page.page.Browser().PageFromTarget(item.info.TargetID)
			if createErr != nil {
				continue
			}
			child = createdPage
			created = true
		}
		frameID := child.FrameID
		if frameID == "" {
			page.dropOOPIF(ctx, item.info.TargetID, child, created)
			continue
		}
		// frame tree 是 page 级的；其他 Tab 的 iframe target 在这里会失败。
		owner, ownerErr := (proto.DOMGetFrameOwner{FrameID: frameID}).Call(page.page.Context(ctx))
		if ownerErr != nil || owner == nil {
			page.dropOOPIF(ctx, item.info.TargetID, child, created)
			continue
		}
		childTree, treeErr := (proto.PageGetFrameTree{}).Call(child.Context(ctx))
		if treeErr != nil || childTree == nil || childTree.FrameTree == nil || childTree.FrameTree.Frame == nil {
			page.dropOOPIF(ctx, item.info.TargetID, child, created)
			continue
		}
		frame := childTree.FrameTree.Frame
		if created {
			page.frameMu.Lock()
			if page.oopifs == nil {
				page.oopifs = map[proto.TargetTargetID]*rod.Page{}
			}
			page.oopifs[item.info.TargetID] = child
			page.frameMu.Unlock()
		}
		bindings = append(bindings, frameBinding{FrameID: frame.ID, LoaderID: frame.LoaderID, Page: child, URL: frame.URL})
	}
	var stale []*rod.Page
	var staleTargets []proto.TargetTargetID
	page.frameMu.Lock()
	for targetID, child := range page.oopifs {
		if !present[targetID] {
			stale = append(stale, child)
			staleTargets = append(staleTargets, targetID)
			delete(page.oopifs, targetID)
		}
	}
	page.frameMu.Unlock()
	// Close 会触发 CDP detach，不能持有 frameMu。
	for index, child := range stale {
		page.dropOOPIF(ctx, staleTargets[index], child, true)
	}
	return bindings
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
	if err != nil || result == nil || result.Result == nil {
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

const frameCollectScript = `(() => {
	const prefix = %q;
	const limit = %d;
	const state = window.__browserkit_frame_refs_v1__ || (window.__browserkit_frame_refs_v1__ = {next: 0});
	const clean = value => (value || '').replace(/[\u0000-\u001f]/g, ' ').replace(/\s+/g, ' ').trim();
	const selectors = 'button,a,input,textarea,select,summary,[contenteditable]:not([contenteditable="false"]),[role="button"],[role="link"],[role="textbox"],[role="checkbox"],[role="radio"],[role="switch"],[role="slider"],[role="combobox"],[role="menuitem"],[role="option"],[role="tab"],[aria-haspopup],[aria-expanded],[draggable="true"],[onclick],[tabindex]:not([tabindex="-1"])';
	const visible = el => {
		try {
			if (el.checkVisibility && !el.checkVisibility({checkOpacity: true, checkVisibilityCSS: true})) return false;
		} catch {}
		const style = getComputedStyle(el);
		const rect = el.getBoundingClientRect();
		return !el.hidden && style.display !== 'none' && style.pointerEvents !== 'none' && rect.width > 1 && rect.height > 1;
	};
	const out = [];
	for (const el of document.querySelectorAll(selectors)) {
		if (out.length >= limit) break;
		if (el.hasAttribute('contenteditable') && !el.isContentEditable) continue;
		if (!visible(el)) continue;
		const type = (el.getAttribute('type') || '').toLowerCase();
		const tag = el.tagName;
		if (tag === 'INPUT' && (type === 'hidden' || type === 'password')) continue;
		const style = getComputedStyle(el);
		const autocomplete = ((el.getAttribute('autocomplete') || '') + ' ' + (el.form?.getAttribute('autocomplete') || '')).toLowerCase().split(/\s+/);
		const sensitiveAuto = autocomplete.some(token => token === 'current-password' || token === 'new-password' || token === 'one-time-code' || token === 'username' || token.startsWith('cc-'));
		const masked = (style.getPropertyValue('-webkit-text-security') || 'none') !== 'none' || sensitiveAuto;
		let ref = el.getAttribute('data-browserkit-ref') || '';
		if (!ref.startsWith(prefix + '_')) {
			ref = prefix + '_' + (++state.next);
			el.setAttribute('data-browserkit-ref', ref);
		}
		const inputRole = type === 'file' ? 'button' : type === 'checkbox' ? 'checkbox' : type === 'radio' ? 'radio' :
			(type === 'button' || type === 'submit' || type === 'reset' || type === 'image') ? 'button' :
			type === 'range' ? 'slider' : type === 'number' ? 'spinbutton' : type === 'search' ? 'searchbox' : 'textbox';
		const contentEditableRoot = el.isContentEditable && !(el.parentElement && el.parentElement.isContentEditable);
		const role = el.getAttribute('role') || (tag === 'A' ? 'link' : tag === 'BUTTON' ? 'button' : tag === 'INPUT' ? inputRole : tag === 'SELECT' ? 'combobox' : tag === 'TEXTAREA' || contentEditableRoot ? 'textbox' : 'button');
		const label = el.getAttribute('aria-label') || [...(el.labels || [])].map(l => l.innerText || l.textContent || '').join(' ') || el.getAttribute('placeholder') || el.getAttribute('title') || clean(el.innerText || el.textContent || '') || el.getAttribute('name') || el.id || '';
		const editable = el.isContentEditable || tag === 'TEXTAREA' || (tag === 'INPUT' && !['checkbox','radio','file','button','submit','reset','image'].includes(type));
		const readonly = editable && (el.readOnly === true || el.getAttribute('aria-readonly') === 'true');
		const multiline = tag === 'TEXTAREA' || (contentEditableRoot && el.getAttribute('aria-multiline') !== 'false') || el.getAttribute('aria-multiline') === 'true';
		const disabled = el.matches(':disabled') || el.getAttribute('aria-disabled') === 'true';
		const valueCapable = editable || tag === 'SELECT';
		const rawValue = valueCapable && !masked ? (el.isContentEditable ? (el.innerText || '') : (typeof el.value === 'string' ? el.value : '')) : '';
		const value = rawValue.slice(0, 120);
		const item = {ref: ref, role: role, name: clean(label).slice(0, 160), type: type || undefined, id: el.id || undefined, placeholder: el.getAttribute('placeholder') || undefined, value: value || undefined, value_truncated: rawValue.length > 120 || undefined, readonly: readonly || undefined, contenteditable: contentEditableRoot || undefined, multiline: multiline || undefined, disabled: disabled || undefined, draggable: el.draggable === true || undefined};
		if (el.hasAttribute('aria-expanded')) item.expanded = el.getAttribute('aria-expanded') === 'true';
		if (el.hasAttribute('aria-pressed')) item.pressed = el.getAttribute('aria-pressed') === 'true';
		if (type === 'checkbox' || type === 'radio') item.indeterminate = el.indeterminate || undefined;
		if (tag === 'SELECT') {
			item.options = [...el.options].slice(0, 100).map(option => ({value: option.value, label: clean(option.label).slice(0, 160), selected: option.selected}));
			if (el.options.length > 100) item.options_truncated = true;
		}
		if (type === 'checkbox' || type === 'radio') item.checked = el.checked;
		out.push(item);
	}
	return out;
})()`

// captureCrossFrameElements 聚合跨域 frame 内的可交互元素。
func (page *rodPage) captureCrossFrameElements(ctx context.Context, frames []frameBinding, room int) ([]Element, bool) {
	if room <= 0 || len(frames) == 0 {
		return nil, false
	}
	var elements []Element
	for _, frame := range frames {
		if len(elements) >= room {
			break
		}
		limit := room - len(elements)
		if limit > 100 {
			limit = 100
		}
		expression := strings.Replace(frameCollectScript, "%q", strconvQuote(frame.Prefix), 1)
		expression = strings.Replace(expression, "%d", itoa(limit), 1)
		raw, ok := page.frameEvaluate(ctx, frame, expression)
		if !ok {
			continue
		}
		var items []Element
		if err := json.Unmarshal(raw, &items); err != nil {
			continue
		}
		for index := range items {
			items[index].FrameURL = frame.URL
		}
		elements = append(elements, items...)
	}
	// 跨域元素的 value 与主文档共享总量预算，避免大量 iframe 字段绕过限制。
	budget := 4000
	for index := range elements {
		if elements[index].Value == "" {
			continue
		}
		runes := []rune(elements[index].Value)
		if budget <= 0 {
			elements[index].Value = ""
			elements[index].ValueTruncated = true
			continue
		}
		if len(runes) > budget {
			elements[index].Value = string(runes[:budget])
			elements[index].ValueTruncated = true
			budget = 0
			continue
		}
		budget -= len(runes)
	}
	return elements, len(elements) > 0
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

// frameElementPoint 返回跨域元素在主视口中的中心点。
func (page *rodPage) frameElementPoint(ctx context.Context, binding frameBinding, ref string) (float64, float64, error) {
	pointExpression := `(() => {
		const el = document.querySelector('[data-browserkit-ref=' + JSON.stringify(` + strconvQuote(ref) + `) + ']');
		if (!el) return null;
		el.scrollIntoView({behavior: 'instant', block: 'center', inline: 'center'});
		const rect = el.getBoundingClientRect();
		const x = rect.x + rect.width / 2;
		const y = rect.y + rect.height / 2;
		const hit = document.elementFromPoint(x, y);
		const covered = !!hit && hit !== el && !el.contains(hit);
		return {x: x, y: y, width: rect.width, height: rect.height,
			inViewport: x >= 0 && y >= 0 && x <= window.innerWidth && y <= window.innerHeight, covered: covered};
	})()`
	raw, ok := page.frameEvaluate(ctx, binding, pointExpression)
	if !ok {
		return 0, 0, newError("stale_reference", "跨域 frame 中已找不到该元素", "重新调用 browserSnapshot", true)
	}
	var point struct {
		X          float64 `json:"x"`
		Y          float64 `json:"y"`
		Width      float64 `json:"width"`
		Height     float64 `json:"height"`
		InViewport bool    `json:"inViewport"`
		Covered    bool    `json:"covered"`
	}
	if err := json.Unmarshal(raw, &point); err != nil || point.Width <= 0 || point.Height <= 0 {
		return 0, 0, newError("stale_reference", "跨域元素不可交互", "重新调用 browserSnapshot 确认元素仍可见", true)
	}
	if !point.InViewport {
		// 元素中心在 frame 视口外（例如 fixed/溢出内容），坐标相加会落到父页面。
		return 0, 0, newError("element_out_of_frame", "跨域元素中心不在 iframe 可视区域内", "滚动 iframe 或改用页面内控件", true)
	}
	if point.Covered {
		return 0, 0, newError("frame_unavailable", "跨域元素被 frame 内其他元素遮挡", "关闭遮挡层后再试", true)
	}
	owner, err := proto.DOMGetFrameOwner{FrameID: binding.FrameID}.Call(page.page.Context(ctx))
	if err != nil || owner == nil {
		return 0, 0, newError("frame_unavailable", "无法定位跨域 frame", "重新加载页面后再试", true)
	}
	_ = proto.DOMScrollIntoViewIfNeeded{BackendNodeID: owner.BackendNodeID}.Call(page.page.Context(ctx))
	box, err := proto.DOMGetBoxModel{BackendNodeID: owner.BackendNodeID}.Call(page.page.Context(ctx))
	if err != nil || box == nil || box.Model == nil || len(box.Model.Content) < 2 {
		return 0, 0, newError("frame_unavailable", "无法读取跨域 frame 的位置", "重新加载页面后再试", true)
	}
	quad := box.Model.Content
	// 旋转、skew 或缩放会让 owner box 与 frame 内部坐标不再同尺度，
	// 无法安全换算时直接拒绝，避免把点击落到父页面。
	if !axisAlignedQuad(quad) {
		return 0, 0, newError("frame_transform_unsupported", "跨域 frame 使用了旋转或变形", "在未变形的页面中操作，或使用页面内控件", true)
	}
	offsetX, offsetY := quad[0], quad[1]
	// transform/zoom 会让 owner box 与 frame 内部坐标不再同尺度，
	// 直接相加会点错位置；两个方向都必须接近 1:1。
	if viewportWidth, viewportHeight, ok := page.frameViewport(ctx, binding); ok && viewportWidth > 0 && viewportHeight > 0 {
		quadWidth := math.Hypot(quad[2]-quad[0], quad[3]-quad[1])
		quadHeight := math.Hypot(quad[6]-quad[0], quad[7]-quad[1])
		if quadWidth <= 0 || quadHeight <= 0 {
			return 0, 0, newError("frame_transform_unsupported", "跨域 frame 尺寸无效", "重新加载页面后再试", true)
		}
		ratioX := quadWidth / viewportWidth
		ratioY := quadHeight / viewportHeight
		if ratioX < 0.98 || ratioX > 1.02 || ratioY < 0.98 || ratioY > 1.02 {
			return 0, 0, newError("frame_transform_unsupported", "跨域 frame 使用了缩放或变形", "在未缩放的页面中操作，或使用页面内控件", true)
		}
	}
	x, y := offsetX+point.X, offsetY+point.Y
	if !pointInQuad(quad, x, y) {
		return 0, 0, newError("frame_unavailable", "跨域 frame 不在可视区域内", "滚动页面或调整窗口后再试", true)
	}
	metrics, metricsErr := proto.PageGetLayoutMetrics{}.Call(page.page.Context(ctx))
	if metricsErr != nil || metrics == nil || metrics.CSSLayoutViewport == nil {
		return 0, 0, newError("frame_unavailable", "无法读取主页面视口", "重新加载页面后再试", true)
	}
	viewport := metrics.CSSLayoutViewport
	if x < 0 || y < 0 || x >= float64(viewport.ClientWidth) || y >= float64(viewport.ClientHeight) {
		return 0, 0, newError("frame_unavailable", "点击位置超出主页面视口", "滚动页面或调整窗口后再试", true)
	}
	// DOM.getNodeForLocation 使用文档坐标，而点击坐标是视口坐标。
	hit, hitErr := proto.DOMGetNodeForLocation{X: int(x) + viewport.PageX, Y: int(y) + viewport.PageY}.Call(page.page.Context(ctx))
	if hitErr != nil || hit == nil || (hit.BackendNodeID != owner.BackendNodeID && hit.FrameID != binding.FrameID) {
		return 0, 0, newError("frame_unavailable", "点击位置被其他元素遮挡", "滚动页面或关闭遮挡层后再试", true)
	}
	return x, y, nil
}

// axisAlignedQuad 判断 DOM quad 是否轴对齐（没有旋转或 skew）。
func axisAlignedQuad(quad []float64) bool {
	if len(quad) < 8 {
		return false
	}
	const tolerance = 0.5
	if math.Abs(quad[1]-quad[3]) >= tolerance || math.Abs(quad[0]-quad[6]) >= tolerance ||
		math.Abs(quad[2]-quad[4]) >= tolerance || math.Abs(quad[5]-quad[7]) >= tolerance {
		return false
	}
	// 镜像或 180° 旋转会让方向反转，坐标映射同样不可靠。
	return quad[2] > quad[0] && quad[5] > quad[1]
}

// pointInQuad 判断点是否落在轴对齐的 quad 内。
func pointInQuad(quad []float64, x, y float64) bool {
	if len(quad) < 8 {
		return false
	}
	left, right := math.Min(quad[0], quad[2]), math.Max(quad[0], quad[2])
	top, bottom := math.Min(quad[1], quad[5]), math.Max(quad[1], quad[5])
	return x >= left && x <= right && y >= top && y <= bottom
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

// typeFrameElement 在跨域 frame 内设置值并触发 input/change。
func (page *rodPage) typeFrameElement(ctx context.Context, binding frameBinding, ref ElementRef, text string, clear bool) error {
	expression := `(() => {
		const el = document.querySelector('[data-browserkit-ref=' + JSON.stringify(` + strconvQuote(ref.Ref) + `) + ']');
		if (!el) return {error: 'stale_reference'};
		const tag = el.tagName;
		const type = (el.type || '').toLowerCase();
		const editable = el.isContentEditable || tag === 'TEXTAREA' || (tag === 'INPUT' && !['checkbox','radio','file','button','submit','reset','image'].includes(type));
		if (!editable) return {error: 'element_not_editable'};
		const readonly = el.isContentEditable ? false : (el.readOnly === true || el.getAttribute('aria-readonly') === 'true');
		if (el.matches(':disabled') || el.getAttribute('aria-disabled') === 'true') return {error: 'element_disabled'};
		if (readonly) return {error: 'element_readonly'};
		const append = %t;
		const next = %s;
		const old = el.isContentEditable ? (el.innerText || '') : (typeof el.value === 'string' ? el.value : '');
		const value = append ? old + next : next;
		el.focus({preventScroll: true});
		if (el.isContentEditable) el.innerText = value; else el.value = value;
		el.dispatchEvent(new Event('input', {bubbles: true, composed: true}));
		el.dispatchEvent(new Event('change', {bubbles: true, composed: true}));
		const actual = el.isContentEditable ? (el.innerText || '') : (typeof el.value === 'string' ? el.value : '');
		return {value: actual, ok: actual === value};
	})()`
	expression = strings.Replace(expression, "%t", strconv.FormatBool(!clear), 1)
	expression = strings.Replace(expression, "%s", strconvQuote(text), 1)
	raw, ok := page.frameEvaluate(ctx, binding, expression)
	if !ok {
		return newError("stale_reference", "跨域 frame 中已找不到该元素", "重新调用 browserSnapshot", true)
	}
	var result struct {
		Error string `json:"error"`
		Value string `json:"value"`
		OK    bool   `json:"ok"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return err
	}
	switch result.Error {
	case "stale_reference":
		return newError("stale_reference", "跨域 frame 中已找不到该元素", "重新调用 browserSnapshot", true)
	case "element_not_editable":
		return newError("element_not_editable", "目标不是可编辑控件", "重新调用 browserSnapshot 选择输入框", true)
	case "element_disabled":
		return newError("element_disabled", "目标控件被禁用", "选择其他控件", true)
	case "element_readonly":
		return newError("element_readonly", "目标控件只读", "选择其他控件", true)
	}
	if !result.OK {
		return newError("invalid_value", "跨域控件没有接受该值", "检查控件类型和允许的值", true)
	}
	return nil
}

// hasFrameRef 在跨域 frame 内检查 ref 是否仍存在。
func (page *rodPage) hasFrameRef(ctx context.Context, binding frameBinding, ref string) (bool, error) {
	expression := `(() => document.querySelector('[data-browserkit-ref=' + JSON.stringify(` + strconvQuote(ref) + `) + ']') !== null)()`
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

// selectFrameOption 在跨域 frame 内选择原生单选下拉框。
func (page *rodPage) selectFrameOption(ctx context.Context, binding frameBinding, ref ElementRef, value, label string) (string, error) {
	expression := `(() => {
		const el = document.querySelector('[data-browserkit-ref=' + JSON.stringify(` + strconvQuote(ref.Ref) + `) + ']');
		if (!el) return {error: 'stale_reference'};
		if (el.tagName !== 'SELECT' || el.multiple) return {error: 'unsupported_select'};
		const criteria = {value: %s, label: %s};
		const option = [...el.options].find(option => criteria.value ? option.value === criteria.value : option.label.trim() === criteria.label);
		if (!option || option.disabled || option.closest('optgroup[disabled]')) return {error: 'option_not_found'};
		el.value = option.value;
		el.dispatchEvent(new Event('input', {bubbles: true}));
		el.dispatchEvent(new Event('change', {bubbles: true}));
		return {value: el.value};
	})()`
	expression = strings.Replace(expression, "%s", strconvQuote(value), 1)
	expression = strings.Replace(expression, "%s", strconvQuote(label), 1)
	raw, ok := page.frameEvaluate(ctx, binding, expression)
	if !ok {
		return "", newError("stale_reference", "跨域 frame 中已找不到该元素", "重新调用 browserSnapshot", true)
	}
	var result struct {
		Value string `json:"value"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", err
	}
	switch result.Error {
	case "stale_reference":
		return "", newError("stale_reference", "跨域 frame 中已找不到该元素", "重新调用 browserSnapshot", true)
	case "unsupported_select":
		return "", newError("unsupported_select", "只支持原生单选下拉框", "改用 browserAct 或页面自身控件", true)
	case "option_not_found":
		return "", newError("option_not_found", "找不到匹配的选项", "重新调用 browserSnapshot 获取可用选项", true)
	}
	return result.Value, nil
}

// setFrameChecked 在跨域 frame 内切换 checkbox/radio/switch。
func (page *rodPage) setFrameChecked(ctx context.Context, binding frameBinding, ref ElementRef, checked bool) (bool, error) {
	readExpression := `(() => {
		const el = document.querySelector('[data-browserkit-ref=' + JSON.stringify(` + strconvQuote(ref.Ref) + `) + ']');
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
		const el = document.querySelector('[data-browserkit-ref=' + JSON.stringify(` + strconvQuote(ref) + `) + ']');
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
		const el = document.querySelector('[data-browserkit-ref=' + JSON.stringify(` + strconvQuote(ref) + `) + ']');
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

// sameOrigin 判断两个地址是否同源；about:blank 和空地址按同源处理。
func sameOrigin(left, right string) bool {
	if left == "" || right == "" || strings.HasPrefix(left, "about:") || strings.HasPrefix(right, "about:") {
		return true
	}
	leftURL, leftErr := url.Parse(left)
	rightURL, rightErr := url.Parse(right)
	if leftErr != nil || rightErr != nil {
		return true
	}
	return leftURL.Scheme == rightURL.Scheme && leftURL.Host == rightURL.Host
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
	if err != nil || result == nil || result.Result == nil || result.Result.ObjectID == "" {
		return "", false
	}
	return result.Result.ObjectID, true
}

// uploadFrameFile 把文件设置到跨域 frame 内的 file input。
func (page *rodPage) uploadFrameFile(ctx context.Context, binding frameBinding, ref string, path string) error {
	selector := strconvQuote(`[data-browserkit-ref="` + ref + `"]`)
	check := `(() => {
		const el = document.querySelector(` + selector + `);
		if (!el) return {error: 'stale_reference'};
		if (el.tagName !== 'INPUT' || el.type !== 'file' || el.disabled) return {error: 'invalid_upload_target'};
		return {ok: true};
	})()`
	raw, ok := page.frameEvaluate(ctx, binding, check)
	if !ok {
		return newError("stale_reference", "跨域 frame 中已找不到该元素", "重新调用 browserSnapshot", true)
	}
	var checkResult struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &checkResult); err != nil {
		return err
	}
	if checkResult.Error == "stale_reference" {
		return newError("stale_reference", "跨域 frame 中已找不到该元素", "重新调用 browserSnapshot", true)
	}
	if !checkResult.OK {
		return newError("invalid_upload_target", "目标不是可用的文件输入框", "重新调用 browserSnapshot 获取 file input 的 ref", true)
	}
	objectID, ok := page.frameObject(ctx, binding, `document.querySelector(`+selector+`)`)
	if !ok {
		return newError("stale_reference", "跨域 frame 中已找不到该元素", "重新调用 browserSnapshot", true)
	}
	client := page.page.Context(ctx)
	if binding.Page != nil {
		client = binding.Page.Context(ctx)
	}
	if err := (proto.DOMSetFileInputFiles{ObjectID: objectID, Files: []string{path}}).Call(client); err != nil {
		return err
	}
	return nil
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

// hasOOPIFs 报告是否还有 OOPIF 缓存需要清理。
func (page *rodPage) hasOOPIFs() bool {
	page.frameMu.Lock()
	defer page.frameMu.Unlock()
	return len(page.oopifs) > 0
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
func (page *rodPage) frameOwnerVisible(ctx context.Context, backendNodeID proto.DOMBackendNodeID) bool {
	if backendNodeID == 0 {
		return true
	}
	node, err := proto.DOMResolveNode{BackendNodeID: backendNodeID}.Call(page.page.Context(ctx))
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
	}.Call(page.page.Context(ctx))
	if err != nil || result == nil || result.Result == nil {
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
