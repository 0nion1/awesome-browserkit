package browserkit

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
)

const maxSnapshotChars = 1 << 20
const maxSnapshotElements = 1_000
const targetCloseTimeout = 2 * time.Second

type rodPage struct {
	page             *rod.Page
	stateMu          sync.Mutex
	interactionMu    sync.Mutex
	releaseOnce      sync.Once
	cancel           context.CancelFunc
	owner            *rodSession
	revision         uint64
	refs             map[string]struct{}
	documentID       string
	stateKnown       bool
	operationTimeout time.Duration
	pointerMu        sync.Mutex
	pointerX         float64
	pointerY         float64
	pointerKnown     bool
}

func (page *rodPage) release() {
	if page == nil {
		return
	}
	page.releaseOnce.Do(func() {
		if page.cancel != nil {
			page.cancel()
		}
		if page.owner != nil {
			page.owner.forgetPage(page)
		}
	})
}

// ctx 为单次 Page 操作创建独立的、有界生命周期 context。
// Page 内部的 root、Browser 和 CDP 请求都必须从这个 context 派生，避免
// 页面操作因远端 renderer 或 websocket 无响应而永久阻塞。
func (page *rodPage) ctx(parent context.Context) (context.Context, context.CancelFunc, error) {
	if page == nil {
		return nil, func() {}, errors.New("浏览器 page 不能为空")
	}
	return contextWithTimeout(parent, page.operationTimeout)
}

func (page *rodPage) controlCtx(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc, error) {
	if page != nil && page.operationTimeout > 0 && page.operationTimeout < timeout {
		timeout = page.operationTimeout
	}
	return contextWithTimeout(parent, timeout)
}

// lockInteraction 可取消地获取交互锁，避免前一个失去响应的 CDP 输入
// 调用拖住后续输入请求。
func lockInteraction(ctx context.Context, mutex *sync.Mutex) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	for {
		if mutex.TryLock() {
			return nil
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// TargetID 返回 rod 页面对应的 CDP target 身份。
func (page *rodPage) TargetID() string {
	if page == nil || page.page == nil {
		return ""
	}
	return string(page.page.TargetID)
}

type domSnapshot struct {
	URL      string    `json:"url"`
	Title    string    `json:"title"`
	Text     string    `json:"text"`
	Elements []Element `json:"elements"`
	Refs     []string  `json:"refs"`
}

type domState struct {
	DocumentID string `json:"document_id"`
}

// pageStateObserverBody 在每个 document 中维护唯一的文档身份，并由
// MutationObserver 清理已从 DOM 移除节点上的 ref，避免 SPA 长时间运行时
// ref 集合无限增长。
const pageStateObserverBody = `
  const key = '__browserkit_page_state_v1__';
  const current = window[key];
  if (current && current.observerInstalled) return current;
  const token = Math.random().toString(36).slice(2);
  const state = {
    documentId: String(performance.timeOrigin || Date.now()) + ':' + token,
    refPrefix: 'e' + token,
    nextRef: 0,
    refs: new Set(),
    observerInstalled: true,
  };
  state.observer = new MutationObserver(mutations => {
    for (const mutation of mutations) for (const node of mutation.removedNodes) {
      if (node.nodeType !== 1) continue;
      const removed = node.matches?.('[data-browserkit-ref]') ? [node] : [];
      const descendants = node.querySelectorAll ? Array.from(node.querySelectorAll('[data-browserkit-ref]')) : [];
      for (const element of [...removed, ...descendants]) {
        const ref = element.getAttribute('data-browserkit-ref');
        if (ref) state.refs.delete(ref);
      }
    }
  });
  if (document.documentElement) state.observer.observe(document.documentElement, {subtree: true, childList: true});
  else document.addEventListener('DOMContentLoaded', () => {
    if (document.documentElement) state.observer.observe(document.documentElement, {subtree: true, childList: true});
  }, {once: true});
  Object.defineProperty(window, key, { configurable: true, value: state });
  return state;`

// pageStateObserverScript 用于 CDP 的新 document 注入，必须是普通脚本。
const pageStateObserverScript = `(function() {` + pageStateObserverBody + `})()`

// pageStateObserverEval 用于 go-rod Eval。使用普通 function，避免 rod 再包一层 apply 时
// 被页面中的异常脚本或非标准 Function 原型影响。
const pageStateObserverEval = `function() {` + pageStateObserverBody + `}`

func (page *rodPage) Info(ctx context.Context) (PageInfo, error) {
	if err := contextErr(ctx); err != nil {
		return PageInfo{}, err
	}
	operationCtx, operationCancel, err := page.ctx(ctx)
	if err != nil {
		return PageInfo{}, err
	}
	defer operationCancel()
	return page.info(operationCtx)
}

func (page *rodPage) info(ctx context.Context) (PageInfo, error) {
	// Rod Page.Info 内部通过未带 context 的 Browser.pageInfo 调用，无法
	// 继承本次操作 deadline；直接使用同一 Target.getTargetInfo 请求并绑定
	// 有界 Browser context。
	info, err := (proto.TargetGetTargetInfo{TargetID: page.page.TargetID}).Call(page.page.Browser().Context(ctx))
	if err != nil {
		return PageInfo{}, err
	}
	if info.TargetInfo == nil {
		return PageInfo{}, errors.New("浏览器 target 信息为空")
	}
	return PageInfo{URL: info.TargetInfo.URL, Title: info.TargetInfo.Title}, nil
}

func (page *rodPage) activate(ctx context.Context) error {
	// Rod Page.Activate 的实现使用 p.browser（持久 context），不能保证
	// 调用方的 deadline；这里用同一 Target.activateTarget 请求绑定 ctx。
	return (proto.TargetActivateTarget{TargetID: page.page.TargetID}).Call(page.page.Browser().Context(ctx))
}

func (page *rodPage) Navigate(ctx context.Context, address string) (PageInfo, error) {
	if err := contextErr(ctx); err != nil {
		return PageInfo{}, err
	}
	operationCtx, operationCancel, err := page.ctx(ctx)
	if err != nil {
		return PageInfo{}, err
	}
	defer operationCancel()
	if err := page.page.Context(operationCtx).Navigate(address); err != nil {
		return PageInfo{}, err
	}
	page.stateMu.Lock()
	page.revision++
	page.refs = make(map[string]struct{})
	page.documentID = ""
	page.stateKnown = false
	page.stateMu.Unlock()
	return page.info(operationCtx)
}

func (page *rodPage) Snapshot(ctx context.Context, options SnapshotOptions) (Snapshot, error) {
	if err := contextErr(ctx); err != nil {
		return Snapshot{}, err
	}
	operationCtx, operationCancel, err := page.ctx(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	defer operationCancel()
	if err := page.syncState(operationCtx); err != nil {
		return Snapshot{}, err
	}
	page.stateMu.Lock()
	if page.revision == 0 {
		page.revision = 1
	}
	revision := page.revision
	page.stateMu.Unlock()
	if options.MaxChars <= 0 {
		options.MaxChars = 12000
	} else if options.MaxChars > maxSnapshotChars {
		options.MaxChars = maxSnapshotChars
	}
	if options.MaxElements <= 0 {
		options.MaxElements = 100
	} else if options.MaxElements > maxSnapshotElements {
		options.MaxElements = maxSnapshotElements
	}
	if options.Offset < 0 {
		options.Offset = 0
	}
	args := map[string]int{"max_chars": options.MaxChars, "max_elements": options.MaxElements, "offset": options.Offset}
	result, err := page.page.Context(operationCtx).Eval(`(options) => {
		const clean = value => (value || '').replace(/[\u0000-\u001f]/g, ' ').replace(/\s+/g, ' ').trim();
		const selectors = 'button,a,input,textarea,select,summary,iframe,[contenteditable="true"],[role="button"],[role="link"],[role="textbox"],[role="menuitem"],[role="option"],[role="tab"],[role="menuitemcheckbox"],[role="menuitemradio"],[role="combobox"],[aria-haspopup],[aria-expanded],[onclick],[tabindex]:not([tabindex="-1"])';
		const visible = el => {
			const style = getComputedStyle(el);
			const rect = el.getBoundingClientRect();
			return !el.hidden && !el.closest('[hidden],[aria-hidden="true"]') && style.display !== 'none' && style.visibility !== 'hidden' && style.opacity !== '0' && style.pointerEvents !== 'none' && rect.width > 0 && rect.height > 0;
		};
		const disabled = el => el.disabled || el.getAttribute('aria-disabled') === 'true';
		const text = el => clean(el.innerText || el.textContent || '');
		const offset = Math.max(0, options.offset || 0);
		const maxElements = Math.max(1, options.max_elements || 100);
		const scanLimit = offset + maxElements;
		const roots = [];
		const visit = root => {
			roots.push(root);
			for (const el of root.querySelectorAll('*')) {
				if (el.shadowRoot) visit(el.shadowRoot);
				if (el.tagName === 'IFRAME') {
					try { if (el.contentDocument?.documentElement) visit(el.contentDocument); } catch {}
				}
			}
		};
		visit(document);
		const candidates = [];
		const candidateSet = new Set();
		const addCandidate = el => {
			if (candidates.length >= scanLimit || candidateSet.has(el)) return;
			candidateSet.add(el);
			candidates.push(el);
		};
		for (const root of roots) {
			for (const el of root.querySelectorAll(selectors)) {
				if (disabled(el) || !visible(el)) continue;
				addCandidate(el);
				if (candidates.length >= scanLimit) break;
			}
			if (candidates.length >= scanLimit) break;
		}
		if (candidates.length < scanLimit) for (const root of roots) {
			for (const el of root.querySelectorAll('*')) {
				if (disabled(el) || !visible(el) || getComputedStyle(el).cursor !== 'pointer') continue;
				const name = text(el);
				if (name.length === 0 || name.length > 160) continue;
				const parent = el.parentElement;
				if (parent && getComputedStyle(parent).cursor === 'pointer') continue;
				if (el.closest('button,a,[role="button"],[role="link"],[role="menuitem"],[role="option"],[role="tab"]')) continue;
				addCandidate(el);
				if (candidates.length >= scanLimit) break;
			}
			if (candidates.length >= scanLimit) break;
		}
		const windowed = candidates.slice(offset, offset + maxElements);
		const state = window.__browserkit_page_state_v1__;
		const elements = windowed.map(el => {
			const previous = el.getAttribute('data-browserkit-ref') || '';
			const ref = previous.startsWith(state.refPrefix + '_') ? previous : state.refPrefix + '_' + (++state.nextRef);
			el.setAttribute('data-browserkit-ref', ref);
			state.refs.add(ref);
			const type = (el.getAttribute('type') || '').toLowerCase();
			const inputRole = type === 'file' ? 'button' : type === 'checkbox' ? 'checkbox' : type === 'radio' ? 'radio' :
				(type === 'button' || type === 'submit' || type === 'reset' || type === 'image') ? 'button' :
				type === 'range' ? 'slider' : type === 'number' ? 'spinbutton' : type === 'search' ? 'searchbox' : 'textbox';
			const role = el.getAttribute('role') || (el.tagName === 'IFRAME' ? 'frame' : el.tagName === 'A' ? 'link' : el.tagName === 'BUTTON' ? 'button' : el.tagName === 'INPUT' ? inputRole : el.tagName === 'SELECT' ? 'combobox' : el.tagName === 'TEXTAREA' ? 'textbox' : 'button');
			const labelledBy = (el.getAttribute('aria-labelledby') || '').split(/\s+/).filter(Boolean).map(id => el.getRootNode().getElementById?.(id)?.textContent || el.ownerDocument.getElementById(id)?.textContent || '').join(' ');
			const nativeLabel = [...(el.labels || [])].map(label => label.innerText || label.textContent || '').join(' ');
			const label = el.getAttribute('aria-label') || labelledBy || nativeLabel || el.getAttribute('placeholder') || el.getAttribute('title') || text(el) || el.getAttribute('name') || '';
			const options = el.tagName === 'SELECT' ? [...el.options].slice(0, 100).map(option => ({value: option.value, label: clean(option.label).slice(0, 160), selected: option.selected})) : undefined;
			return {ref, role, name: clean(label).slice(0, 160), type: type === 'password' ? 'password' : type, frame_url: el.tagName === 'IFRAME' ? el.src : undefined, options, options_truncated: el.tagName === 'SELECT' && el.options.length > 100};
		});
		state.refs = new Set(roots.flatMap(root => [...root.querySelectorAll('[data-browserkit-ref]')].map(el => el.getAttribute('data-browserkit-ref')).filter(ref => ref?.startsWith(state.refPrefix + '_'))));
		const bodyText = clean(roots.map(root => root.body?.innerText || [...(root.children || [])].map(el => el.innerText || '').join(' ')).join(' '));
		return {url: location.href, title: document.title, text: bodyText.slice(0, Math.max(0, options.max_chars || 12000)), elements, refs: [...state.refs]};
	}`, args)
	if err != nil {
		return Snapshot{}, err
	}
	var raw domSnapshot
	if err := result.Value.Unmarshal(&raw); err != nil {
		return Snapshot{}, err
	}
	if raw.URL == "" {
		raw.URL = "about:blank"
	}
	truncated := len(raw.Text) >= options.MaxChars
	activeRefs := make(map[string]struct{}, len(raw.Refs))
	for _, ref := range raw.Refs {
		activeRefs[ref] = struct{}{}
	}
	page.stateMu.Lock()
	if page.revision != revision {
		page.stateMu.Unlock()
		return Snapshot{Info: PageInfo{URL: raw.URL, Title: raw.Title}, Revision: revision, Text: raw.Text, Elements: raw.Elements, Truncated: truncated || len(raw.Elements) == options.MaxElements, ActiveRefs: raw.Refs}, nil
	}
	for ref := range page.refs {
		if _, ok := activeRefs[ref]; !ok {
			delete(page.refs, ref)
		}
	}
	for _, element := range raw.Elements {
		page.refs[element.Ref] = struct{}{}
	}
	revision = page.revision
	page.stateMu.Unlock()
	return Snapshot{Info: PageInfo{URL: raw.URL, Title: raw.Title}, Revision: revision, Text: raw.Text, Elements: raw.Elements, Truncated: truncated || len(raw.Elements) == options.MaxElements, ActiveRefs: raw.Refs}, nil
}

// InspectPoint 为任意可见元素建立与 Snapshot 相同命名空间的 ref。
func (page *rodPage) InspectPoint(ctx context.Context, x, y float64) (InspectedElement, error) {
	operationCtx, cancel, err := page.ctx(ctx)
	if err != nil {
		return InspectedElement{}, err
	}
	defer cancel()
	if err := page.syncState(operationCtx); err != nil {
		return InspectedElement{}, err
	}
	page.stateMu.Lock()
	revision := page.revision
	page.stateMu.Unlock()
	result, err := page.page.Context(operationCtx).Eval(`(point) => {
		const hit = document.elementFromPoint(point.x, point.y);
		if (!hit || hit === document.documentElement) return null;
		const el = hit.closest('button,a,input,textarea,select,summary,[role="button"],[role="link"],[role="textbox"],[role="tab"]') || hit;
		const rect = el.getBoundingClientRect();
		const state = window.__browserkit_page_state_v1__;
		const previous = el.getAttribute('data-browserkit-ref') || '';
		const ref = previous.startsWith(state.refPrefix + '_') ? previous : state.refPrefix + '_' + (++state.nextRef);
		el.setAttribute('data-browserkit-ref', ref);
		state.refs.add(ref);
		const clean = value => (value || '').replace(/[\u0000-\u001f]/g, ' ').replace(/\s+/g, ' ').trim().slice(0, 160);
		const role = el.getAttribute('role') || el.tagName.toLowerCase();
		const labelledBy = (el.getAttribute('aria-labelledby') || '').split(/\s+/).filter(Boolean).map(id => document.getElementById(id)?.textContent || '').join(' ');
		const name = clean(el.getAttribute('aria-label') || labelledBy || el.getAttribute('placeholder') || el.getAttribute('title') || el.innerText || el.textContent || '');
		return {ref, role, name, url: location.href, bounds: {x: rect.left, y: rect.top, width: rect.width, height: rect.height}};
	}`, map[string]float64{"x": x, "y": y})
	if err != nil {
		return InspectedElement{}, err
	}
	var element InspectedElement
	if err := result.Value.Unmarshal(&element); err != nil {
		return InspectedElement{}, err
	}
	if element.Ref == "" {
		return InspectedElement{}, errors.New("该位置没有可选择的元素")
	}
	page.stateMu.Lock()
	if revision != page.revision {
		page.stateMu.Unlock()
		return InspectedElement{}, errors.New("页面已变化，请重新选择元素")
	}
	page.refs[element.Ref] = struct{}{}
	page.stateMu.Unlock()
	element.Revision = revision
	return element, nil
}

// elementLocked 只在持有 page.interactionMu 时调用。返回的 Element 仍会使用传入的
// operationCtx，因此不能在这里创建并取消一个临时 context。
func (page *rodPage) elementLocked(ctx context.Context, ref ElementRef) (*rod.Element, error) {
	if err := page.syncState(ctx); err != nil {
		return nil, err
	}
	page.stateMu.Lock()
	if ref.Ref == "" || ref.Revision == 0 || ref.Revision != page.revision {
		page.stateMu.Unlock()
		return nil, newError("stale_reference", "元素引用已经过期", "重新调用 browserSnapshot 获取当前 ref", true)
	}
	if _, ok := page.refs[ref.Ref]; !ok {
		page.stateMu.Unlock()
		return nil, newError("invalid_reference", "当前快照不存在该元素引用", "重新调用 browserSnapshot", true)
	}
	page.stateMu.Unlock()
	// 这是对快照 ref 的一次性校验，不能使用 rod 默认会持续重试的 Element。
	// 节点被替换或删除时必须立即返回 stale_reference，否则 browserClick 会表现为卡死。
	el, err := page.page.Context(ctx).Sleeper(rod.NotFoundSleeper).ElementByJS(rod.Eval(`function(ref) {
		const find = root => {
			const direct = root.querySelector('[data-browserkit-ref="' + ref + '"]');
			if (direct) return direct;
			for (const node of root.querySelectorAll('*')) {
				if (node.shadowRoot) { const found = find(node.shadowRoot); if (found) return found; }
				if (node.tagName === 'IFRAME') {
					try { if (node.contentDocument) { const found = find(node.contentDocument); if (found) return found; } } catch {}
				}
			}
			return null;
		};
		return find(document);
	}`, ref.Ref))
	if err != nil {
		return nil, newError("stale_reference", "页面结构已变化，元素引用已经过期", "重新调用 browserSnapshot 获取当前 ref", true)
	}
	return el, nil
}

func (page *rodPage) Click(ctx context.Context, ref ElementRef) error {
	operationCtx, operationCancel, err := page.ctx(ctx)
	if err != nil {
		return err
	}
	defer operationCancel()
	if err := lockInteraction(operationCtx, &page.interactionMu); err != nil {
		return err
	}
	defer page.interactionMu.Unlock()
	el, err := page.elementLocked(operationCtx, ref)
	if err != nil {
		return err
	}
	if err := scrollElementNaturally(operationCtx, el); err != nil {
		return err
	}
	target, err := page.elementPoint(operationCtx, el)
	if err != nil {
		return err
	}
	if err := page.movePointer(page.page.Context(operationCtx), target.X, target.Y); err != nil {
		return err
	}
	return dispatchAtomicClick(page.page.Context(operationCtx), target.X, target.Y)
}

// Hover 将真实指针移动到快照元素的可交互坐标。移动轨迹由 movePointer
// 内部生成，属于浏览器执行细节，不向调用方单独报告。
func (page *rodPage) Hover(ctx context.Context, ref ElementRef) error {
	operationCtx, operationCancel, err := page.ctx(ctx)
	if err != nil {
		return err
	}
	defer operationCancel()
	if err := lockInteraction(operationCtx, &page.interactionMu); err != nil {
		return err
	}
	defer page.interactionMu.Unlock()
	el, err := page.elementLocked(operationCtx, ref)
	if err != nil {
		return err
	}
	if err := scrollElementNaturally(operationCtx, el); err != nil {
		return err
	}
	target, err := page.elementPoint(operationCtx, el)
	if err != nil {
		return err
	}
	return page.movePointer(page.page.Context(operationCtx), target.X, target.Y)
}

type elementPointResult struct {
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
	Visible bool    `json:"visible"`
	Enabled bool    `json:"enabled"`
	Covered bool    `json:"covered"`
}

func (page *rodPage) elementPoint(ctx context.Context, element *rod.Element) (elementPointResult, error) {
	result, err := element.Evaluate(rod.Eval(`function() {
		const rect = this.getBoundingClientRect();
		const style = this.ownerDocument.defaultView.getComputedStyle(this);
		let x = rect.left + rect.width / 2;
		let y = rect.top + rect.height / 2;
		const root = this.getRootNode();
		const hit = (root.elementFromPoint?.(x, y) || this.ownerDocument.elementFromPoint(x, y));
		let view = this.ownerDocument.defaultView;
		while (view !== view.top && view.frameElement) {
			const frame = view.frameElement.getBoundingClientRect();
			x += frame.left;
			y += frame.top;
			view = view.parent;
		}
		return {
			x, y,
			visible: !this.hidden && !this.closest('[hidden],[aria-hidden="true"]') &&
				style.display !== 'none' && style.visibility !== 'hidden' &&
				style.opacity !== '0' && style.pointerEvents !== 'none' &&
				rect.width > 0 && rect.height > 0,
			enabled: !this.disabled && this.getAttribute('aria-disabled') !== 'true',
			covered: !hit || !(hit === this || this.contains(hit)),
		};
	}`).ByUser())
	if err != nil {
		return elementPointResult{}, err
	}
	var target elementPointResult
	if err := result.Value.Unmarshal(&target); err != nil {
		return elementPointResult{}, err
	}
	if !target.Visible || !target.Enabled || target.Covered || validateInputPoint(target.X, target.Y) != nil {
		return elementPointResult{}, newError("element_not_interactable", "元素没有可交互的页面区域", "重新调用 browserSnapshot 获取当前 ref", true)
	}
	return target, nil
}

// scrollElementNaturally 将目标滚动拆成最多两段，每段之间留出短暂的
// renderer 时间。依次滚动所有裁剪目标的容器和页面，避免嵌套容器本身
// 位于视口外时只滚动内部容器，最后仍无法点击目标。
func scrollElementNaturally(ctx context.Context, element *rod.Element) error {
	if _, err := element.Evaluate(rod.Eval(`function() {
		if (this.ownerDocument.defaultView !== top || this.getRootNode() !== this.ownerDocument)
			this.scrollIntoView({block: 'center', inline: 'center', behavior: 'instant'});
	}`).ByUser()); err != nil {
		return err
	}
	for step := 0; step < 2; step++ {
		fraction := 0.6
		if step == 1 {
			fraction = 1
		}
		result, err := element.Evaluate(rod.Eval(`function() {
			const doc = this.ownerDocument;
			const view = doc.defaultView;
			const viewportHeight = view.innerHeight || doc.documentElement.clientHeight;
			const viewportWidth = view.innerWidth || doc.documentElement.clientWidth;
			const containers = [];
			let node = this.parentElement;
			while (node && node !== doc.scrollingElement) {
				const style = getComputedStyle(node);
				if ((style.overflowY === 'auto' || style.overflowY === 'scroll' || style.overflowY === 'overlay') && node.scrollHeight > node.clientHeight ||
					(style.overflowX === 'auto' || style.overflowX === 'scroll' || style.overflowX === 'overlay') && node.scrollWidth > node.clientWidth) {
					containers.push(node);
				}
				node = node.parentElement;
			}
			containers.push(doc.scrollingElement || doc.documentElement);
			let moved = false;
			for (const container of containers) {
				const rect = this.getBoundingClientRect();
				const containerRect = container === doc.scrollingElement ?
					{top: 0, left: 0, height: viewportHeight, width: viewportWidth} : container.getBoundingClientRect();
				const margin = Math.min(48, Math.max(12, Math.min(containerRect.height, containerRect.width) * 0.05));
				const centerX = rect.left + rect.width / 2;
				const centerY = rect.top + rect.height / 2;
				if (centerX >= containerRect.left + margin && centerX <= containerRect.left + containerRect.width - margin &&
					centerY >= containerRect.top + margin && centerY <= containerRect.top + containerRect.height - margin) continue;
				const deltaY = centerY - (containerRect.top + containerRect.height / 2);
				const deltaX = centerX - (containerRect.left + containerRect.width / 2);
				container.scrollBy({top: deltaY * ` + fmt.Sprintf("%g", fraction) + `, left: deltaX * ` + fmt.Sprintf("%g", fraction) + `, behavior: 'auto'});
				moved = true;
			}
			return moved;
		}`).ByUser())
		if err != nil {
			return err
		}
		if !result.Value.Bool() {
			return nil
		}
		if err := pauseContext(ctx, 35*time.Millisecond); err != nil {
			return err
		}
	}
	return nil
}

func pauseContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (page *rodPage) movePointer(client proto.Client, x, y float64) error {
	if err := validateInputPoint(x, y); err != nil {
		return err
	}
	page.pointerMu.Lock()
	startX, startY := page.pointerX, page.pointerY
	if !page.pointerKnown {
		startX, startY = 0, 0
	}
	page.pointerMu.Unlock()
	distance := math.Hypot(x-startX, y-startY)
	steps := int(distance/80) + 3
	if steps < 3 {
		steps = 3
	} else if steps > 24 {
		steps = 24
	}
	// 固定的二次贝塞尔控制点提供平滑、可复现的轻微弧线，不引入随机数。
	controlX := (startX+x)/2 - (y-startY)*0.12
	controlY := (startY+y)/2 + (x-startX)*0.12
	released := 0
	for index := 1; index <= steps; index++ {
		t := float64(index) / float64(steps)
		eased := t * t * (3 - 2*t)
		oneMinus := 1 - eased
		pointX := oneMinus*oneMinus*startX + 2*oneMinus*eased*controlX + eased*eased*x
		pointY := oneMinus*oneMinus*startY + 2*oneMinus*eased*controlY + eased*eased*y
		event := proto.InputDispatchMouseEvent{Type: proto.InputDispatchMouseEventTypeMouseMoved, X: pointX, Y: pointY, Button: proto.InputMouseButtonNone, Buttons: &released, PointerType: proto.InputDispatchMouseEventPointerTypeMouse}
		if err := event.Call(client); err != nil {
			return err
		}
	}
	page.pointerMu.Lock()
	page.pointerX, page.pointerY, page.pointerKnown = x, y, true
	page.pointerMu.Unlock()
	return nil
}

func dispatchAtomicClick(client proto.Client, x, y float64) error {
	pressed, released := 1, 0
	move := proto.InputDispatchMouseEvent{Type: proto.InputDispatchMouseEventTypeMouseMoved, X: x, Y: y, Button: proto.InputMouseButtonNone, Buttons: &released, PointerType: proto.InputDispatchMouseEventPointerTypeMouse}
	if err := move.Call(client); err != nil {
		return err
	}
	down := proto.InputDispatchMouseEvent{Type: proto.InputDispatchMouseEventTypeMousePressed, X: x, Y: y, Button: proto.InputMouseButtonLeft, Buttons: &pressed, ClickCount: 1, PointerType: proto.InputDispatchMouseEventPointerTypeMouse}
	if err := down.Call(client); err != nil {
		return err
	}
	up := proto.InputDispatchMouseEvent{Type: proto.InputDispatchMouseEventTypeMouseReleased, X: x, Y: y, Button: proto.InputMouseButtonLeft, Buttons: &released, ClickCount: 1, PointerType: proto.InputDispatchMouseEventPointerTypeMouse}
	return up.Call(client)
}

func (page *rodPage) Type(ctx context.Context, ref ElementRef, text string, clear bool) error {
	operationCtx, operationCancel, err := page.ctx(ctx)
	if err != nil {
		return err
	}
	defer operationCancel()
	if err := lockInteraction(operationCtx, &page.interactionMu); err != nil {
		return err
	}
	defer page.interactionMu.Unlock()
	el, err := page.elementLocked(operationCtx, ref)
	if err != nil {
		return err
	}
	if err := scrollElementNaturally(operationCtx, el); err != nil {
		return err
	}
	target, err := page.elementPoint(operationCtx, el)
	if err != nil {
		return err
	}
	if err := page.movePointer(page.page.Context(operationCtx), target.X, target.Y); err != nil {
		return err
	}
	// 滚动、聚焦和选区在一次 JS 任务中完成，文本再通过一个独立 CDP 命令
	// 插入；整个路径不依赖动画帧、重绘或 Rod Element 交互包装。
	prepare := `function() {
		this.focus({preventScroll: true});
		if (` + fmt.Sprintf("%t", clear) + `) {
			if (typeof this.select === 'function') this.select();
			else {
				const range = document.createRange();
				range.selectNodeContents(this);
				const selection = window.getSelection();
				selection.removeAllRanges();
				selection.addRange(range);
			}
		}
	}`
	if _, err := el.Evaluate(rod.Eval(prepare).ByUser()); err != nil {
		return err
	}
	if err := (proto.InputInsertText{Text: text}).Call(page.page.Context(operationCtx)); err != nil {
		return err
	}
	_, _ = el.Evaluate(rod.Eval(`function() {
		this.dispatchEvent(new Event('input', {bubbles: true, composed: true}));
	}`).ByUser())
	return nil
}

func (page *rodPage) SelectOption(ctx context.Context, ref ElementRef, value, label string) (string, error) {
	operationCtx, operationCancel, err := page.ctx(ctx)
	if err != nil {
		return "", err
	}
	defer operationCancel()
	if err := lockInteraction(operationCtx, &page.interactionMu); err != nil {
		return "", err
	}
	defer page.interactionMu.Unlock()
	el, err := page.elementLocked(operationCtx, ref)
	if err != nil {
		return "", err
	}
	if err := scrollElementNaturally(operationCtx, el); err != nil {
		return "", err
	}
	if _, err := page.elementPoint(operationCtx, el); err != nil {
		return "", err
	}
	result, err := el.Evaluate(rod.Eval(`function(criteria) {
		if (this.tagName !== 'SELECT' || this.multiple) return {error: 'unsupported_select'};
		const option = [...this.options].find(option => criteria.value ? option.value === criteria.value : option.label.trim() === criteria.label);
		if (!option || option.disabled || option.closest('optgroup[disabled]')) return {error: 'option_not_found'};
		this.value = option.value;
		this.dispatchEvent(new Event('input', {bubbles: true}));
		this.dispatchEvent(new Event('change', {bubbles: true}));
		return {value: this.value};
	}`, map[string]string{"value": value, "label": label}).ByUser())
	if err != nil {
		return "", err
	}
	var selection struct {
		Value string `json:"value"`
		Error string `json:"error"`
	}
	if err := result.Value.Unmarshal(&selection); err != nil {
		return "", err
	}
	if selection.Error != "" {
		return "", newError(selection.Error, "无法选择该下拉选项", "重新调用 browserSnapshot 获取可用选项", true)
	}
	return selection.Value, nil
}

func (page *rodPage) UploadFile(ctx context.Context, ref ElementRef, path string) error {
	operationCtx, cancel, err := page.ctx(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	if err := lockInteraction(operationCtx, &page.interactionMu); err != nil {
		return err
	}
	defer page.interactionMu.Unlock()
	el, err := page.elementLocked(operationCtx, ref)
	if err != nil {
		return err
	}
	check, err := el.Evaluate(rod.Eval(`function() { return this.tagName === 'INPUT' && this.type === 'file' && !this.disabled }`).ByUser())
	if err != nil {
		return err
	}
	if !check.Value.Bool() {
		return newError("invalid_upload_target", "目标不是可用的文件输入框", "重新调用 browserSnapshot 获取 file input 的 ref", true)
	}
	return el.SetFiles([]string{path})
}

func (page *rodPage) Press(ctx context.Context, key string) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	keys := map[string]input.Key{"Enter": input.Enter, "Tab": input.Tab, "Escape": input.Escape, "Backspace": input.Backspace, "ArrowUp": input.ArrowUp, "ArrowDown": input.ArrowDown, "ArrowLeft": input.ArrowLeft, "ArrowRight": input.ArrowRight, "Space": input.Space}
	keyValue, ok := keys[key]
	if !ok {
		return newError("invalid_key", "不支持的按键", "使用 Enter、Tab、Escape、Backspace、方向键或 Space", true)
	}
	operationCtx, operationCancel, err := page.ctx(ctx)
	if err != nil {
		return err
	}
	defer operationCancel()
	if err := lockInteraction(operationCtx, &page.interactionMu); err != nil {
		return err
	}
	defer page.interactionMu.Unlock()
	err = page.syncState(operationCtx)
	if err != nil {
		return err
	}
	client := page.page.Context(operationCtx)
	if err := keyValue.Encode(proto.InputDispatchKeyEventTypeKeyDown, keyValue.Modifier()).Call(client); err != nil {
		return err
	}
	return keyValue.Encode(proto.InputDispatchKeyEventTypeKeyUp, 0).Call(client)
}

func (page *rodPage) Wait(ctx context.Context, options WaitOptions) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	supported := map[string]bool{
		"selector_visible": true, "selector_hidden": true,
		"url_contains": true, "title_contains": true,
		"text_contains": true, "load_complete": true,
	}
	if !supported[options.Condition] {
		return newError("unsupported_wait", "不支持的等待条件", "使用 selector_visible、selector_hidden、url_contains、title_contains、text_contains 或 load_complete", true)
	}
	if options.Condition != "load_complete" && options.Selector == "" {
		return errors.New("等待条件需要 ref_or_selector")
	}
	operationCtx, operationCancel, err := page.ctx(ctx)
	if err != nil {
		return err
	}
	defer operationCancel()
	deadline := time.NewTicker(25 * time.Millisecond)
	defer deadline.Stop()
	for {
		if err := contextErr(operationCtx); err != nil {
			return err
		}
		result, err := page.page.Context(operationCtx).Eval(`(condition, value) => {
			const roots = [];
			const visit = root => {
				roots.push(root);
				for (const el of root.querySelectorAll('*')) {
					if (el.shadowRoot) visit(el.shadowRoot);
					if (el.tagName === 'IFRAME') {
						try { if (el.contentDocument?.documentElement) visit(el.contentDocument); } catch {}
					}
				}
			};
			visit(document);
			const matches = selector => roots.map(root => root.querySelector(selector)).filter(Boolean);
			const visible = element => {
				if (!element) return false;
				const style = getComputedStyle(element);
				const rect = element.getBoundingClientRect();
				return !element.hidden && !element.closest('[hidden],[aria-hidden="true"]') &&
					style.display !== 'none' && style.visibility !== 'hidden' &&
					style.opacity !== '0' && rect.width > 0 && rect.height > 0;
			};
			switch (condition) {
			case 'selector_visible': return matches(value).some(visible);
			case 'selector_hidden': return !matches(value).some(visible);
			case 'url_contains': return location.href.includes(value);
			case 'title_contains': return document.title.includes(value);
			case 'text_contains': return roots.some(root => (root.body?.innerText || [...(root.children || [])].map(el => el.innerText || '').join(' ')).includes(value));
			case 'load_complete': return document.readyState === 'complete';
			default: return false;
			}
		}`, options.Condition, options.Selector)
		if err == nil && result.Value.Bool() {
			return nil
		}
		if err != nil {
			return err
		}
		select {
		case <-operationCtx.Done():
			return operationCtx.Err()
		case <-deadline.C:
		}
	}
}

func (page *rodPage) WaitIdle(ctx context.Context, timeout time.Duration) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	operationCtx, operationCancel, err := page.ctx(ctx)
	if err != nil {
		return err
	}
	defer operationCancel()
	maxTimeout := page.operationTimeout
	if maxTimeout <= 0 {
		maxTimeout = defaultOperationTimeout
	}
	if timeout <= 0 || timeout > maxTimeout {
		timeout = maxTimeout
	}
	return page.page.Context(operationCtx).WaitIdle(timeout)
}

func (page *rodPage) syncState(ctx context.Context) error {
	result, err := page.page.Context(ctx).Eval(`() => {
		const state = window.__browserkit_page_state_v1__;
		return state ? {document_id: state.documentId} : null;
	}`)
	if err != nil {
		return err
	}
	var state *domState
	if err := result.Value.Unmarshal(&state); err != nil {
		return err
	}
	if state == nil || state.DocumentID == "" {
		result, err = page.page.Context(ctx).Eval(pageStateObserverEval)
		if err != nil {
			return err
		}
		state = &domState{}
		if err := result.Value.Unmarshal(state); err != nil {
			return err
		}
	}
	page.stateMu.Lock()
	defer page.stateMu.Unlock()
	if page.stateKnown && state.DocumentID != page.documentID {
		page.revision++
		page.refs = make(map[string]struct{})
	}
	page.documentID = state.DocumentID
	page.stateKnown = true
	return nil
}

func (page *rodPage) Close(ctx context.Context) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	operationCtx, operationCancel, err := page.controlCtx(ctx, targetCloseTimeout)
	if err != nil {
		return err
	}
	defer operationCancel()
	// Rod Page.Close 会在持有全局 targetsLock 时等待 targetDestroyed，锁等待
	// 本身不能被 context 取消。Target.closeTarget 是单个有界 CDP 请求。
	_, err = (proto.TargetCloseTarget{TargetID: page.page.TargetID}).Call(page.page.Browser().Context(operationCtx))
	if err != nil {
		return err
	}
	page.page.Browser().RemoveState(page.page.TargetID)
	page.release()
	return nil
}

func contextWithTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc, error) {
	if err := contextErr(ctx); err != nil {
		return nil, nil, err
	}
	if timeout <= 0 {
		timeout = defaultOperationTimeout
	}
	operationCtx, cancel := context.WithTimeout(ctx, timeout)
	return operationCtx, cancel, nil
}

func contextErr(ctx context.Context) error {
	if ctx == nil {
		return errors.New("browserkit context 不能为空")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

// EncodeScreenshot 是给不支持二进制 Tool Result 的宿主使用的有限辅助函数。
func EncodeScreenshot(data []byte) string { return base64.StdEncoding.EncodeToString(data) }
