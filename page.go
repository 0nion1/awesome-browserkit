package browserkit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

const maxSnapshotChars = 1 << 20
const maxSnapshotElements = 1_000
const targetCloseTimeout = 2 * time.Second

type rodPage struct {
	page             *rod.Page
	stateMu          sync.Mutex
	frameMu          sync.Mutex
	frameWorlds      map[proto.PageFrameID]proto.RuntimeExecutionContextID
	frameLoaders     map[proto.PageFrameID]proto.NetworkLoaderID
	oopifs           map[proto.TargetTargetID]*rod.Page
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
	dialogMu         sync.Mutex
	pendingDialog    *Dialog
	dialogSequence   uint64
	dialogChanges    chan struct{}
	sameDocSeq       uint64
	sameDocURL       string
}

// startDialogWatcher 记录原生 JavaScript 弹窗，但不替用户决定接受或取消。
// 弹窗存在时 renderer 会暂停；Manager 使用最近快照返回页面状态，直到
// browserHandleDialog 显式处理当前弹窗。
func (page *rodPage) startDialogWatcher() {
	if page == nil || page.page == nil {
		return
	}
	// EachEvent 的注册本身是同步的；先注册回调，再在 goroutine 中等待事件，
	// 避免页面在 goroutine 调度前就弹出对话框而漏处理。
	wait := page.page.EachEvent(func(event *proto.PageJavascriptDialogOpening) {
		page.dialogMu.Lock()
		page.dialogSequence++
		options := []string{"accept", "dismiss"}
		if event.Type == proto.PageDialogTypeAlert {
			options = []string{"accept"}
		}
		page.pendingDialog = &Dialog{
			ID:         page.dialogSequence,
			Type:       string(event.Type),
			Message:    event.Message,
			URL:        event.URL,
			Options:    options,
			PromptText: event.DefaultPrompt,
		}
		page.notifyDialogLocked()
		page.dialogMu.Unlock()
	}, func(event *proto.PageNavigatedWithinDocument) {
		// 只记录主 frame 的同文档导航；iframe 的 pushState 不能证明
		// 当前页面的历史导航已经完成。
		if event.FrameID != page.page.FrameID {
			return
		}
		page.stateMu.Lock()
		page.sameDocSeq++
		page.sameDocURL = event.URL
		page.stateMu.Unlock()
	})
	go wait()
}

func (page *rodPage) notifyDialogLocked() {
	if page.dialogChanges != nil {
		close(page.dialogChanges)
	}
	page.dialogChanges = make(chan struct{})
}

// PendingDialog 返回当前仍阻塞页面的弹窗，不消费状态。
func (page *rodPage) PendingDialog() *Dialog {
	if page == nil {
		return nil
	}
	page.dialogMu.Lock()
	defer page.dialogMu.Unlock()
	if page.pendingDialog == nil {
		return nil
	}
	value := *page.pendingDialog
	value.Options = append([]string(nil), value.Options...)
	return &value
}

// DialogChanges 返回下一次弹窗出现时关闭的通知通道。
func (page *rodPage) DialogChanges() <-chan struct{} {
	if page == nil {
		return nil
	}
	page.dialogMu.Lock()
	defer page.dialogMu.Unlock()
	if page.dialogChanges == nil {
		page.dialogChanges = make(chan struct{})
	}
	return page.dialogChanges
}

// registerTitleBindingAsync 异步恢复标题变化通知。标题通知只是页面投影的
// 辅助能力，不能占用导航或弹窗处理的关键路径；弹窗出现时优先取消请求。
func (page *rodPage) registerTitleBindingAsync() {
	if page == nil || page.page == nil || page.PendingDialog() != nil {
		return
	}
	changes := page.DialogChanges()
	if page.PendingDialog() != nil {
		return
	}
	base := page.page.GetContext()
	if base == nil {
		base = context.Background()
	}
	go func() {
		bindCtx, cancel := context.WithTimeout(base, time.Second)
		defer cancel()
		done := make(chan struct{}, 1)
		go func() {
			registerTitleBinding(bindCtx, page.page)
			done <- struct{}{}
		}()
		select {
		case <-done:
		case <-changes:
			// 原生弹窗暂停 renderer；标题通知让路给弹窗状态和
			// browserHandleDialog，不能等待普通 OperationTimeout。
			cancel()
		case <-bindCtx.Done():
		}
	}()
}

// runUntilDialog 执行一个可能触发原生弹窗的页面动作。CDP 输入请求可能在
// renderer 弹窗后仍等待脚本返回；此时弹窗事件就是动作已经派发的边界，
// 立即取消等待并让调用方通过 browserHandleDialog 决定后续行为。
func (page *rodPage) runUntilDialog(ctx context.Context, action func(context.Context) error) error {
	if pending := page.PendingDialog(); pending != nil {
		return newError("dialog_open", "页面存在待处理弹窗", "调用 browserSnapshot 获取 dialog id，再使用 browserHandleDialog", true)
	}
	changes := page.DialogChanges()
	// 弹窗可能恰好出现在第一次检查与订阅之间；订阅后必须再检查一次，
	// 否则会等待下一次变化直到操作超时。
	if page.PendingDialog() != nil {
		return nil
	}
	actionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- action(actionCtx) }()
	for {
		select {
		case err := <-done:
			if page.PendingDialog() != nil {
				return nil
			}
			return err
		case <-changes:
			if page.PendingDialog() != nil {
				cancel()
				return nil
			}
			changes = page.DialogChanges()
		case <-ctx.Done():
			if page.PendingDialog() != nil {
				return nil
			}
			return ctx.Err()
		}
	}
}

// HandleDialog 按调用方的明确选择处理当前弹窗。
func (page *rodPage) HandleDialog(ctx context.Context, options DialogHandleOptions) (Dialog, error) {
	if page == nil || page.page == nil {
		return Dialog{}, errors.New("浏览器 page 不能为空")
	}
	page.dialogMu.Lock()
	pending := page.pendingDialog
	if pending == nil {
		page.dialogMu.Unlock()
		return Dialog{}, newError("dialog_not_found", "页面当前没有待处理弹窗", "重新调用 browserSnapshot 获取页面状态", true)
	}
	value := *pending
	value.Options = append([]string(nil), pending.Options...)
	page.dialogMu.Unlock()
	if options.ID == 0 || options.ID != value.ID {
		return Dialog{}, newError("dialog_stale", "弹窗身份已经变化", "重新调用 browserSnapshot，并使用当前 dialog id", true)
	}
	accept := options.Action == "accept"
	if !accept && options.Action != "dismiss" {
		return Dialog{}, newError("invalid_dialog_action", "弹窗动作必须是 accept 或 dismiss", "使用 browserSnapshot 返回的 options", true)
	}
	allowed := false
	for _, action := range value.Options {
		if action == options.Action {
			allowed = true
			break
		}
	}
	if !allowed {
		return Dialog{}, newError("invalid_dialog_action", "当前弹窗不支持该动作", "使用 browserSnapshot 返回的 options", true)
	}
	if value.Type != string(proto.PageDialogTypePrompt) && options.PromptText != "" {
		return Dialog{}, newError("invalid_prompt_text", "只有 prompt 弹窗可以填写 prompt_text", "移除 prompt_text 后重试", true)
	}
	operationCtx, cancel, err := page.ctx(ctx)
	if err != nil {
		return Dialog{}, err
	}
	defer cancel()
	if err := (proto.PageHandleJavaScriptDialog{Accept: accept, PromptText: options.PromptText}).Call(page.page.Context(operationCtx)); err != nil {
		return Dialog{}, err
	}
	page.dialogMu.Lock()
	if page.pendingDialog != nil && page.pendingDialog.ID == value.ID {
		page.pendingDialog = nil
	}
	page.dialogMu.Unlock()
	page.registerTitleBindingAsync()
	value.Handled = true
	value.Accepted = accept
	if value.Type == string(proto.PageDialogTypePrompt) && accept {
		value.PromptText = options.PromptText
	}
	return value, nil
}

func (page *rodPage) release() {
	if page == nil {
		return
	}
	page.releaseOnce.Do(func() {
		page.closeOOPIFs()
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
	HasMore  bool      `json:"has_more"`
	// TextTruncated 表示正文被 max_chars 截断。
	TextTruncated bool `json:"text_truncated"`
	// CrossFrame 表示存在无法从主文档读取的跨域 iframe。
	CrossFrame         bool   `json:"cross_frame"`
	ActiveElementRef   string `json:"active_element_ref"`
	ActiveElementValue string `json:"active_element_value"`
}

type domState struct {
	DocumentID string `json:"document_id"`
}

// titleBindingName 是页面标题变化上报使用的 CDP binding。
// Headless Chrome 不会为纯标题变化发送 Target.targetInfoChanged，
// 因此由页面内的 MutationObserver 主动通知 Session。
const titleBindingName = "__browserkit_title_v1__"

// pageStateObserverBody 在每个 document 中维护唯一的文档身份，并由
// MutationObserver 清理已从 DOM 移除节点上的 ref，避免 SPA 长时间运行时
// ref 集合无限增长。标题变化同时通过 CDP binding 上报给 Session。
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
  if (!state.titleObserverInstalled) {
    state.titleObserverInstalled = true;
    let titleObserver = null;
    let headObserver = null;
    const reportTitle = () => {
      try {
        if (typeof window['` + titleBindingName + `'] === 'function') window['` + titleBindingName + `']('changed');
      } catch (error) {}
    };
    const watchTitle = () => {
      if (titleObserver) titleObserver.disconnect();
      titleObserver = null;
      state.titleObserver = null;
      const title = document.querySelector('title');
      if (!title) return;
      titleObserver = new MutationObserver(reportTitle);
      state.titleObserver = titleObserver;
      titleObserver.observe(title, {childList: true, characterData: true, subtree: true});
    };
    const watchHead = (attempts) => {
      const head = document.head;
      if (!head) {
        // 文档起始脚本运行时 head 尚未创建；短暂重试直到解析器建立 head。
        if (attempts > 0) setTimeout(() => watchHead(attempts - 1), 50);
        return;
      }
      watchTitle();
      if (!headObserver) {
        headObserver = new MutationObserver(watchTitle);
        state.headObserver = headObserver;
        headObserver.observe(head, {childList: true});
      }
    };
    document.addEventListener('DOMContentLoaded', () => watchHead(0), {once: true});
    watchHead(200);
  }
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
	if err := page.runUntilDialog(operationCtx, func(actionCtx context.Context) error {
		return page.page.Context(actionCtx).Navigate(address)
	}); err != nil {
		return PageInfo{}, err
	}
	// Navigation 会产生新的文档，CDP binding 不会自动保留。标题通知是
	// 辅助能力，异步恢复并在弹窗出现时立即让路，不能阻塞导航结果。
	page.registerTitleBindingAsync()
	page.stateMu.Lock()
	page.revision++
	page.refs = make(map[string]struct{})
	page.documentID = ""
	page.stateKnown = false
	page.stateMu.Unlock()
	if page.PendingDialog() != nil {
		return PageInfo{URL: address}, nil
	}
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
		const xpathFor = (el, useId) => {
			const segments = [];
			for (let node = el; node && node.nodeType === 1; node = node.parentElement) {
				if (useId && node.id) {
					const escaped = String(node.id).replace(/"/g, '\\"');
					segments.unshift('*[@id="' + escaped + '"]');
					return '//' + segments.join('/');
				}
				let index = 1;
				for (let sibling = node.previousElementSibling; sibling; sibling = sibling.previousElementSibling) {
					if (sibling.localName === node.localName) index++;
				}
				segments.unshift(node.localName + '[' + index + ']');
			}
			return segments.length ? '/' + segments.join('/') : '';
		};
		const cssFor = el => {
			if (el.id) return '#' + CSS.escape(el.id);
			const parts = [];
			for (let node = el; node && node.nodeType === 1; node = node.parentElement) {
				if (node.id) { parts.unshift('#' + CSS.escape(node.id)); break; }
				let part = node.localName;
				const siblings = node.parentElement ? [...node.parentElement.children].filter(item => item.localName === node.localName) : [];
				if (siblings.length > 1) part += ':nth-of-type(' + (siblings.indexOf(node) + 1) + ')';
				parts.unshift(part);
				if (node.localName === 'html') break;
			}
			return parts.join(' > ');
		};
		const cleanValue = value => (value || '').replace(/[\u0000-\u0008\u000b-\u001f]/g, ' ');
		const truncate = (value, limit) => {
			if (value.length <= limit) return {text: value, truncated: false};
			// 只对前 2*limit 个 UTF-16 单元做码点转换，避免超大值先被完整展开。
			const chars = Array.from(value.slice(0, Math.min(value.length, limit * 2)));
			return {text: chars.slice(0, limit).join(''), truncated: chars.length > limit || value.length > limit * 2};
		};
		const selectors = 'button,a,input,textarea,select,summary,iframe,[contenteditable]:not([contenteditable="false"]),[role="button"],[role="link"],[role="textbox"],[role="checkbox"],[role="radio"],[role="switch"],[role="slider"],[draggable="true"],[ondrop],[ondragover],[role="menuitem"],[role="option"],[role="tab"],[role="menuitemcheckbox"],[role="menuitemradio"],[role="combobox"],[aria-haspopup],[aria-expanded],[onclick],[tabindex]:not([tabindex="-1"])';
		const visible = el => {
			const style = getComputedStyle(el);
			const rect = el.getBoundingClientRect();
			return !el.hidden && !el.closest('[hidden],[aria-hidden="true"]') && style.display !== 'none' && style.visibility !== 'hidden' && style.opacity !== '0' && style.pointerEvents !== 'none' && rect.width > 0 && rect.height > 0;
		};
		const clippedAway = style => {
			const value = style.clipPath;
			if (!value || value === 'none') return false;
			const match = value.match(/inset\(([^)]*)\)/);
			if (!match) return false;
			const parts = match[1].trim().split(/\s+/).filter(Boolean);
			const pct = part => part.endsWith('%') ? parseFloat(part) : (parseFloat(part) === 0 ? 0 : NaN);
			if (parts.length === 0) return false;
			const values = parts.length === 1 ? [parts[0], parts[0], parts[0], parts[0]]
				: parts.length === 2 ? [parts[0], parts[1], parts[0], parts[1]]
				: parts.length === 3 ? [parts[0], parts[1], parts[2], parts[1]] : parts;
			const [top, right, bottom, left] = values.map(pct);
			if ([top, right, bottom, left].some(Number.isNaN)) return false;
			return top + bottom >= 100 || left + right >= 100;
		};
		// 同源 iframe 的正文/元素由主文档脚本遍历，必须自行检查宿主及其祖先，
		// 否则隐藏容器里的 iframe 内容仍会进入快照。
		const hiddenHost = el => {
			for (let node = el; node;) {
				const style = getComputedStyle(node);
				if (style.display === 'none' || Number(style.opacity) < 0.05 || style.contentVisibility === 'hidden' || clippedAway(style)) return true;
				if (node.parentElement) { node = node.parentElement; continue; }
				const root = node.getRootNode ? node.getRootNode() : null;
				if (root && root.host) { node = root.host; continue; }
				break;
			}
			const rect = el.getBoundingClientRect();
			const view = el.ownerDocument && el.ownerDocument.defaultView;
			const width = view ? view.innerWidth : 0;
			const height = view ? view.innerHeight : 0;
			return rect.width <= 1 || rect.height <= 1 || rect.right < -1000 || rect.bottom < -1000 || rect.left > width + 5000 || rect.top > height + 5000;
		};
		const disabled = el => el.matches(':disabled') || el.getAttribute('aria-disabled') === 'true';
		const text = el => clean(el.innerText || el.textContent || '');
		const offset = Math.max(0, options.offset || 0);
		const maxElements = Math.max(1, options.max_elements || 100);
		// 多扫描一个候选只用于判断是否还有下一页，避免把“恰好满页”
		// 误报成还需继续分页。
		const scanLimit = offset + maxElements + 1;
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
				if (el.hasAttribute('contenteditable') && !el.isContentEditable) continue;
				if (!visible(el)) continue;
				addCandidate(el);
				if (candidates.length >= scanLimit) break;
			}
			if (candidates.length >= scanLimit) break;
		}
		if (candidates.length < scanLimit) for (const root of roots) {
			for (const el of root.querySelectorAll('*')) {
				// 先看 cursor 再看布局：大页面里绝大多数元素不是可点击卡片，
				// 这样只在命中时才付出 getBoundingClientRect 和文本提取成本。
				if (el.hidden) continue;
				const style = getComputedStyle(el);
				if (style.cursor !== 'pointer' || style.display === 'none' || style.visibility === 'hidden' || style.opacity === '0' || style.pointerEvents === 'none') continue;
				const rect = el.getBoundingClientRect();
				if (rect.width <= 0 || rect.height <= 0 || el.closest('[hidden],[aria-hidden="true"]')) continue;
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
		let valueBudget = 4000;
		// 沿祖先、shadow host 和 iframe 宿主链计算有效可见性；隐藏宿主
		// 下的值不应因为子文档坐标系看起来正常而泄露。
		const effectivelyHidden = start => {
				let node = start;
				let first = true;
				while (node) {
					const style = getComputedStyle(node);
					const fill = style.webkitTextFillColor || '';
					if (style.display === 'none' || Number(style.opacity) < 0.05 || style.contentVisibility === 'hidden' || clippedAway(style) || (style.clip && style.clip !== 'auto')) return true;
					// visibility、font-size 和 text-fill 可被子元素覆盖；start 的
					// computed style 已包含继承或覆盖结果，只检查 start。
					if (first && (style.visibility === 'hidden' || style.visibility === 'collapse' || parseFloat(style.fontSize) === 0 || fill === 'transparent' || fill === 'rgba(0, 0, 0, 0)')) return true;
					first = false;
				const rect = node.getBoundingClientRect();
				const view = node.ownerDocument && node.ownerDocument.defaultView;
				const doc = node.ownerDocument && node.ownerDocument.documentElement;
				const left = rect.left + (view ? view.scrollX : 0);
				const top = rect.top + (view ? view.scrollY : 0);
				// 用文档坐标判断屏外，避免长页面滚动后把顶部正常字段误删。
				if (doc && (left < -1000 || top < -1000 || top > doc.scrollHeight + 1000)) return true;
				// 正方向越界用视口判断：绝对定位元素会把文档 scrollWidth 撑大。
				if (view && (rect.left > view.innerWidth + 5000 || rect.top > view.innerHeight + 5000)) return true;
				if (node.parentElement) { node = node.parentElement; continue; }
				const root = node.getRootNode ? node.getRootNode() : null;
				if (root && root.host) { node = root.host; continue; }
				const frameElement = view ? view.frameElement : null;
				if (frameElement) {
					const frameStyle = getComputedStyle(frameElement);
					if (frameStyle.display === 'none' || frameStyle.visibility === 'hidden' || frameStyle.visibility === 'collapse' || frameElement.clientWidth <= 1 || frameElement.clientHeight <= 1) return true;
					node = frameElement;
					continue;
				}
				node = null;
			}
			return false;
		};
		const elements = windowed.map(el => {
			const previous = el.getAttribute('data-browserkit-ref') || '';
			const ref = previous.startsWith(state.refPrefix + '_') ? previous : state.refPrefix + '_' + (++state.nextRef);
			el.setAttribute('data-browserkit-ref', ref);
			state.refs.add(ref);
			const type = (el.getAttribute('type') || '').toLowerCase();
			const contentEditableRoot = el.isContentEditable && !(el.parentElement && el.parentElement.isContentEditable);
			const inputRole = type === 'file' ? 'button' : type === 'checkbox' ? 'checkbox' : type === 'radio' ? 'radio' :
				(type === 'button' || type === 'submit' || type === 'reset' || type === 'image') ? 'button' :
				type === 'range' ? 'slider' : type === 'number' ? 'spinbutton' : type === 'search' ? 'searchbox' : 'textbox';
			const role = el.getAttribute('role') || (el.tagName === 'IFRAME' ? 'frame' : el.tagName === 'A' ? 'link' : el.tagName === 'BUTTON' ? 'button' : el.tagName === 'INPUT' ? inputRole : el.tagName === 'SELECT' ? 'combobox' : el.tagName === 'TEXTAREA' || contentEditableRoot ? 'textbox' : 'button');
			const labelledBy = (el.getAttribute('aria-labelledby') || '').split(/\s+/).filter(Boolean).map(id => el.getRootNode().getElementById?.(id)?.textContent || el.ownerDocument.getElementById(id)?.textContent || '').join(' ');
			const nativeLabel = [...(el.labels || [])].map(label => label.innerText || label.textContent || '').join(' ');
			const buttonValue = el.tagName === 'INPUT' && ['button', 'submit', 'reset'].includes(type) ? el.value : '';
			const label = el.getAttribute('aria-label') || labelledBy || nativeLabel || el.getAttribute('placeholder') || el.getAttribute('title') || text(el) || buttonValue || el.getAttribute('name') || el.id || '';
			const optionsFor = el => [...el.options].slice(0, 100).map(option => ({value: option.value, label: clean(option.label).slice(0, 160), selected: option.selected}));
			const checkable = el.tagName === 'INPUT' && (type === 'checkbox' || type === 'radio');
			const ariaChecked = el.getAttribute('aria-checked');
			const checked = checkable ? el.checked : ariaChecked === 'true' ? true : ariaChecked === 'false' || ariaChecked === 'mixed' ? false : undefined;
			const readonly = (el.tagName === 'TEXTAREA' || el.isContentEditable || (el.tagName === 'INPUT' && !['checkbox', 'radio', 'file', 'button', 'submit', 'reset', 'image', 'range', 'color'].includes(type))) && (el.readOnly === true || el.getAttribute('aria-readonly') === 'true');
			const pressedAttr = el.getAttribute('aria-pressed');
			const pressed = pressedAttr === 'true' ? true : pressedAttr === 'false' ? false : undefined;
			const pressedMixed = pressedAttr === 'mixed';
			const expanded = el.hasAttribute('aria-expanded') ? el.getAttribute('aria-expanded') === 'true' : undefined;
			const placeholder = el.getAttribute('placeholder') || '';
			const multiline = el.tagName === 'TEXTAREA' || (contentEditableRoot && el.getAttribute('aria-multiline') !== 'false') || el.getAttribute('aria-multiline') === 'true';
			const valueCapable = contentEditableRoot || el.tagName === 'TEXTAREA' || el.tagName === 'SELECT' || (el.tagName === 'INPUT' && !['password', 'hidden', 'file', 'button', 'submit', 'reset', 'image', 'checkbox', 'radio'].includes(type));
			const autocomplete = ((el.getAttribute('autocomplete') || '') + ' ' + (el.form?.getAttribute('autocomplete') || '')).toLowerCase().split(/\s+/);
			const maskedToken = item => item === 'current-password' || item === 'new-password' || item === 'one-time-code' || item === 'username' || item.startsWith('cc-');
			const valueStyle = getComputedStyle(el);
			const masked = (valueStyle.getPropertyValue('-webkit-text-security') || 'none') !== 'none' || autocomplete.some(maskedToken);
			const valueRect = el.getBoundingClientRect();
			const valueHidden = masked || valueRect.width <= 1 || valueRect.height <= 1 || effectivelyHidden(el);
			const options = el.tagName === 'SELECT' && !valueHidden ? optionsFor(el) : undefined;
			let value, valueTruncated;
			if (valueCapable && !valueHidden) {
				const raw = contentEditableRoot ? (el.innerText || '') : (typeof el.value === 'string' ? el.value : '');
				const cleaned = cleanValue(raw);
				if (valueBudget > 0) {
					if (cleaned !== '') {
						const take = Math.min(120, valueBudget);
						const cut = truncate(cleaned, take);
						value = cut.text.length > 0 ? cut.text : undefined;
						valueTruncated = cut.truncated || undefined;
						const used = Array.from(cleaned.slice(0, Math.min(cleaned.length, take * 2))).length;
						valueBudget -= Math.min(used, take);
					}
				} else if (cleaned !== '') {
					valueTruncated = true;
				}
			}
			return {locator: {selector: cssFor(el), xpath: xpathFor(el, true), absolute_xpath: xpathFor(el, false), role, name: clean(label).slice(0, 160), type, frame_url: el.ownerDocument === document ? undefined : el.ownerDocument.location.href}, ref, role, checked, indeterminate: checkable ? el.indeterminate : ariaChecked === 'mixed', disabled: disabled(el), readonly, contenteditable: contentEditableRoot, multiline, pressed, pressed_mixed: pressedMixed, expanded, draggable: el.draggable === true, name: clean(label).slice(0, 160), id: el.id ? el.id.replace(/[\u0000-\u001f]/g, ' ').slice(0, 160) : undefined, placeholder: placeholder ? clean(placeholder).slice(0, 160) : undefined, value, value_truncated: valueTruncated, type: type === 'password' ? 'password' : type, frame_url: el.tagName === 'IFRAME' ? el.src : undefined, options, options_truncated: el.tagName === 'SELECT' && !valueHidden && el.options.length > 100};
		});
		state.refs = new Set(roots.flatMap(root => [...root.querySelectorAll('[data-browserkit-ref]')].map(el => el.getAttribute('data-browserkit-ref')).filter(ref => ref?.startsWith(state.refPrefix + '_'))));
		let active = document.activeElement;
		for (;;) {
			let nested;
			try { nested = active?.shadowRoot?.activeElement || (active?.tagName === 'IFRAME' ? active.contentDocument?.activeElement : null); } catch {}
			if (!nested || nested === active) break;
			active = nested;
		}
		const activeRef = active?.getAttribute?.('data-browserkit-ref') || '';
		const activeValue = active && ('value' in active) ? String(active.value ?? '') : (active?.isContentEditable ? String(active.textContent || '') : '');
		const visibleForText = el => {
			try {
				return !el.checkVisibility || el.checkVisibility({checkOpacity: true, checkVisibilityCSS: true});
			} catch { return true; }
		};
		const visibleText = (node, limit) => {
			const out = [];
			let size = 0;
			let stopped = false;
			let crossFrame = false;
			// 只收集到正文预算即可；长页面不需要为截断后的文本继续扫描整棵 DOM。
			const push = value => {
				if (stopped || value === '') return;
				const gap = out.length > 0 ? 1 : 0;
				const remain = limit - size - gap;
				if (remain <= 0) { stopped = true; return; }
				if (value.length > remain) {
					out.push(value.slice(0, remain));
					size = limit;
					stopped = true;
					return;
				}
				out.push(value);
				size += value.length + gap;
			};
			const walk = (parent, parentStyle) => {
				if (stopped) return;
				const textHidden = parentStyle.visibility === 'hidden' || parentStyle.visibility === 'collapse' || parseFloat(parentStyle.fontSize) === 0 || parentStyle.webkitTextFillColor === 'transparent' || parentStyle.webkitTextFillColor === 'rgba(0, 0, 0, 0)';
				for (const child of parent.childNodes) {
					if (stopped) return;
					if (child.nodeType === 3) {
						if (!textHidden && visibleForText(parent)) push(child.textContent || '');
						continue;
					}
					if (child.nodeType !== 1) continue;
					if (['SCRIPT', 'STYLE', 'NOSCRIPT', 'TEMPLATE'].includes(child.tagName)) continue;
					const style = getComputedStyle(child);
					// display:none 和近透明子树不可能渲染出可见文本；visibility 和
					// font-size 可被子元素覆盖，因此逐元素判断而不整棵剪枝。
					if (style.display === 'none' || Number(style.opacity) < 0.05) continue;
					if (child.tagName === 'TEXTAREA' || child.tagName === 'SELECT') continue;
					if ((style.getPropertyValue('-webkit-text-security') || 'none') !== 'none') continue;
					if (child.tagName === 'IFRAME') {
						if (style.visibility === 'hidden' || style.visibility === 'collapse' || hiddenHost(child)) continue;
						try {
							if (child.contentDocument) {
								const body = child.contentDocument.body || child.contentDocument;
								walk(body, getComputedStyle(body));
							} else {
								// 跨域 iframe 的 contentDocument 为 null，交给 CDP 只读聚合。
								crossFrame = true;
							}
						} catch { crossFrame = true; }
						continue;
					}
					if (child.shadowRoot) walk(child.shadowRoot, style);
					walk(child, style);
				}
			};
			const rootStyle = getComputedStyle(node);
			if (rootStyle.display === 'none' || Number(rootStyle.opacity) < 0.05) return {text: '', stopped: false, crossFrame: false};
			walk(node, rootStyle);
			return {text: out.join(' '), stopped, crossFrame};
		};
		const maxChars = Math.max(0, options.max_chars || 12000);
		// 多收一点给 clean 的空白折叠留余量，避免刚好卡在预算边界时误判截断。
		const collected = visibleText(document.body || document.documentElement, maxChars + 64);
		const bodyText = clean(collected.text);
		return {url: location.href, title: document.title, text: bodyText.slice(0, maxChars), elements, refs: [...state.refs], has_more: candidates.length > offset + elements.length, text_truncated: bodyText.length > maxChars || collected.stopped, cross_frame: collected.crossFrame, active_element_ref: activeRef, active_element_value: activeValue};
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
	// 跨域子 frame 的内容无法从主文档读取；用 CDP isolated world 只读聚合正文，
	// 让模型至少能看到嵌入式表单、合同或说明文本。
	if raw.CrossFrame || page.hasOOPIFs() {
		// frame tree 只读取一次，正文与元素共用同一份快照。
		frames, frameErr := page.frameBindings(operationCtx)
		if frameErr == nil && len(frames) > 0 {
			if !raw.TextTruncated && options.MaxChars > len(raw.Text) {
				if extra, cut := page.crossFrameText(operationCtx, frames, options.MaxChars-len(raw.Text)); extra != "" {
					raw.Text += extra
					raw.TextTruncated = cut
				}
			}
			// 分页快照不会重新采集跨域元素，但已有 frame ref 仍然有效，
			// 需要保留在 ActiveRefs 中，避免被下一次快照误删。
			if options.Offset > 0 {
				page.stateMu.Lock()
				for ref := range page.refs {
					if isFrameRef(ref) {
						raw.Refs = append(raw.Refs, ref)
					}
				}
				page.stateMu.Unlock()
			}
			// 跨域 frame 内的可交互元素使用独立 ref 前缀；只在第一页追加，
			// 保持主文档的分页语义不被跨域元素打乱。
			if options.Offset == 0 && len(raw.Elements) < options.MaxElements {
				if extra, ok := page.captureCrossFrameElements(operationCtx, frames, options.MaxElements-len(raw.Elements)); ok {
					for _, element := range extra {
						raw.Refs = append(raw.Refs, element.Ref)
					}
					raw.Elements = append(raw.Elements, extra...)
				}
			}
		}
	}
	truncated := raw.TextTruncated
	activeRefs := make(map[string]struct{}, len(raw.Refs))
	for _, ref := range raw.Refs {
		activeRefs[ref] = struct{}{}
	}
	page.stateMu.Lock()
	if page.revision != revision {
		page.stateMu.Unlock()
		return Snapshot{Info: PageInfo{URL: raw.URL, Title: raw.Title}, Revision: revision, Text: raw.Text, Elements: raw.Elements, Truncated: truncated || raw.HasMore, HasMore: raw.HasMore, TextTruncated: truncated, PaginationKnown: true, ActiveElementRef: raw.ActiveElementRef, ActiveElementValue: raw.ActiveElementValue, ActiveRefs: raw.Refs}, nil
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
	return Snapshot{Info: PageInfo{URL: raw.URL, Title: raw.Title}, Revision: revision, Text: raw.Text, Elements: raw.Elements, Truncated: truncated || raw.HasMore, HasMore: raw.HasMore, TextTruncated: truncated, PaginationKnown: true, ActiveElementRef: raw.ActiveElementRef, ActiveElementValue: raw.ActiveElementValue, ActiveRefs: raw.Refs}, nil
}

// crossFrameText 通过 isolated world 读取跨域子 frame 的渲染文本。
// 只读，不注入脚本到页面主 world；每个 frame 独立创建世界，导航后自动失效。
func (page *rodPage) crossFrameText(ctx context.Context, frames []frameBinding, limit int) (string, bool) {
	if limit <= 0 || len(frames) == 0 {
		return "", false
	}
	var builder strings.Builder
	for _, frame := range frames {
		expression := `(() => { const body = document.body; return {url: location.url || location.href, text: body ? (body.innerText || '').slice(0, 16000) : ''}; })()`
		raw, ok := page.frameEvaluate(ctx, frame, expression)
		if !ok {
			continue
		}
		var payload struct {
			URL  string `json:"url"`
			Text string `json:"text"`
		}
		if unmarshalErr := json.Unmarshal(raw, &payload); unmarshalErr != nil {
			continue
		}
		if payload.URL == "" {
			payload.URL = frame.URL
		}
		text := strings.Join(strings.Fields(payload.Text), " ")
		if text == "" {
			continue
		}
		label := "\n[frame " + payload.URL + "]\n"
		remaining := limit - builder.Len()
		if remaining <= len(label) {
			return builder.String(), true
		}
		room := remaining - len(label)
		if len(text) > room {
			// 按字节截断可能切开 UTF-8；回退到最近的有效边界。
			cut := room
			for cut > 0 && !utf8.RuneStart(text[cut]) {
				cut--
			}
			text = text[:cut]
			builder.WriteString(label)
			builder.WriteString(text)
			return builder.String(), true
		}
		builder.WriteString(label)
		builder.WriteString(text)
	}
	return builder.String(), false
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
	if isFrameRef(ref.Ref) {
		return nil, newError("frame_unavailable", "跨域 frame 已失效或当前不可见", "重新调用 browserSnapshot 获取当前 frame ref", true)
	}
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

func (page *rodPage) Click(ctx context.Context, ref ElementRef, options ClickOptions) error {
	if _, _, err := clickParameters(options); err != nil {
		return err
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
	if binding, ok := page.frameBindingForRef(operationCtx, ref.Ref); ok {
		if err := page.validateFrameRef(operationCtx, ref); err != nil {
			return err
		}
		return page.runUntilDialog(operationCtx, func(actionCtx context.Context) error {
			return page.clickFrameElement(actionCtx, binding, ref.Ref, options)
		})
	}
	el, err := page.elementLocked(operationCtx, ref)
	if err != nil {
		return err
	}
	return page.runUntilDialog(operationCtx, func(actionCtx context.Context) error {
		return page.clickElementLocked(actionCtx, el, options)
	})
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
	if binding, ok := page.frameBindingForRef(operationCtx, ref.Ref); ok {
		if err := page.validateFrameRef(operationCtx, ref); err != nil {
			return err
		}
		x, y, pointErr := page.frameElementPoint(operationCtx, binding, ref.Ref)
		if pointErr != nil {
			return pointErr
		}
		return page.movePointer(page.page.Context(operationCtx), x, y)
	}
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
			enabled: !this.matches(':disabled') && this.getAttribute('aria-disabled') !== 'true',
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
	if binding, ok := page.frameBindingForRef(operationCtx, ref.Ref); ok {
		if err := page.validateFrameRef(operationCtx, ref); err != nil {
			return err
		}
		return page.typeFrameElement(operationCtx, binding, ref, text, clear)
	}
	el, err := page.elementLocked(operationCtx, ref)
	if err != nil {
		return err
	}
	// 先拒绝只读、禁用和非输入控件，避免滚动、移动指针和
	// Input.insertText 之后页面值没有变化却被报告为成功。
	state, err := el.Evaluate(rod.Eval(`function() {
		const tag = this.tagName;
		const type = (this.type || '').toLowerCase();
		const textInput = tag === 'INPUT' && ['text', 'search', 'tel', 'url', 'email', 'password', 'number'].includes(type);
		const valueInput = tag === 'INPUT' && ['date', 'month', 'week', 'time', 'datetime-local', 'color', 'range', 'number'].includes(type);
		const editable = this.isContentEditable || tag === 'TEXTAREA' || textInput || valueInput;
		const readonlyCapable = this.isContentEditable || tag === 'TEXTAREA' || textInput || (valueInput && type !== 'range' && type !== 'color');
		const readonly = readonlyCapable && (this.readOnly === true || this.getAttribute('aria-readonly') === 'true');
		const disabled = this.matches(':disabled') || this.getAttribute('aria-disabled') === 'true';
		if (disabled || readonly || !editable) return {error: disabled ? 'element_disabled' : readonly ? 'element_readonly' : 'element_not_editable', tag: tag, inputType: type};
		return {type: type};
	}`).ByUser())
	if err != nil {
		return err
	}
	var editState struct {
		Error     string `json:"error"`
		Type      string `json:"type"`
		Tag       string `json:"tag"`
		InputType string `json:"inputType"`
	}
	if err := state.Value.Unmarshal(&editState); err != nil {
		return err
	}
	if editState.Error != "" {
		next := "重新调用 browserSnapshot 选择可编辑的输入框或内容编辑区域"
		switch {
		case editState.Tag == "SELECT":
			next = "使用 browserSelectOption 选择选项（仅支持单选原生下拉）"
		case editState.Tag == "INPUT" && (editState.InputType == "checkbox" || editState.InputType == "radio"):
			next = "使用 browserSetChecked 设置勾选状态"
		case editState.Tag == "INPUT" && editState.InputType == "file":
			next = "文件输入请使用 browserUploadFile（需要配置 upload_dir）"
		case editState.Tag == "BUTTON" || editState.Tag == "A" || (editState.Tag == "INPUT" && (editState.InputType == "button" || editState.InputType == "submit" || editState.InputType == "reset" || editState.InputType == "image")):
			next = "该元素不是输入框，使用 browserClick 操作"
		}
		return newError(editState.Error, "元素不可编辑", next, true)
	}
	if isValueInputType(editState.Type) {
		if err := validateValueInput(editState.Type, text); err != nil {
			return err
		}
		return setValueInput(operationCtx, el, editState.Type, text, editState.Type == "number" && !clear)
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

func isValueInputType(kind string) bool {
	switch kind {
	case "date", "month", "week", "time", "datetime-local", "color", "range", "number":
		return true
	default:
		return false
	}
}

var rangeNumberPattern = regexp.MustCompile(`^-?(?:[0-9]+(?:\.[0-9]+)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)

func validateValueInput(kind, text string) error {
	if text == "" {
		// range 和 color 没有“空值”语义：浏览器会把空串变成中点值或黑色。
		if kind == "range" || kind == "color" {
			return newError("invalid_value", kind+" 需要有效取值", "填写该控件允许的取值", true)
		}
		return nil
	}
	valid := false
	hint := ""
	switch kind {
	case "date":
		parsed, err := time.Parse("2006-01-02", text)
		valid = err == nil && parsed.Year() > 0
		hint = "使用 YYYY-MM-DD"
	case "month":
		parsed, err := time.Parse("2006-01", text)
		valid = err == nil && parsed.Year() > 0
		hint = "使用 YYYY-MM"
	case "week":
		valid = len(text) == 8 && text[4] == '-' && text[5] == 'W'
		if valid {
			year, yearErr := strconv.Atoi(text[:4])
			week, weekErr := strconv.Atoi(text[6:])
			valid = yearErr == nil && weekErr == nil && year > 0 && week >= 1 && week <= 53
		}
		hint = "使用 YYYY-Www，例如 2026-W42"
	case "time":
		_, err := time.Parse("15:04", text)
		if err != nil {
			_, err = time.Parse("15:04:05", text)
		}
		if err != nil {
			_, err = time.Parse("15:04:05.999", text)
		}
		valid = err == nil
		hint = "使用 HH:MM、HH:MM:SS 或带小数的秒；秒需符合控件 step"
	case "datetime-local":
		parsed, err := time.Parse("2006-01-02T15:04", text)
		if err != nil {
			parsed, err = time.Parse("2006-01-02T15:04:05", text)
		}
		if err != nil {
			parsed, err = time.Parse("2006-01-02T15:04:05.999", text)
		}
		valid = err == nil && parsed.Year() > 0
		hint = "使用 YYYY-MM-DDTHH:MM[:SS[.sss]]；秒需符合控件 step"
	case "color":
		valid = len(text) == 7 && text[0] == '#'
		if valid {
			for _, char := range text[1:] {
				if !strings.ContainsRune("0123456789abcdefABCDEF", char) {
					valid = false
					break
				}
			}
		}
		hint = "使用 #rrggbb"
	case "range":
		if !rangeNumberPattern.MatchString(text) {
			valid, hint = false, "使用十进制数字，可带小数或指数"
			break
		}
		value, err := strconv.ParseFloat(text, 64)
		if err != nil || math.IsInf(value, 0) || math.IsNaN(value) {
			valid, hint = false, "填写有限数字"
			break
		}
		valid, hint = true, "填写有限数字"
	case "number":
		value, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
		valid = err == nil && !math.IsInf(value, 0) && !math.IsNaN(value)
		hint = "填写有限数字"
	}
	if !valid {
		return newError("invalid_value", kind+" 输入格式无效", hint, true)
	}
	return nil
}

func setValueInput(ctx context.Context, element *rod.Element, kind, text string, appendValue bool) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	result, err := element.Evaluate(rod.Eval(`function(input) {
		const value = input.text;
		this.focus({preventScroll: true});
		const descriptor = Object.getOwnPropertyDescriptor(Object.getPrototypeOf(this), 'value');
		const setValue = next => {
			if (descriptor && descriptor.set) descriptor.set.call(this, next);
			else this.value = next;
		};
		const oldValue = this.value;
		const next = input.append ? oldValue + value : value;
		const rangeRequested = this.type === 'range' && value !== '';
		const canonicalDecimal = text => {
			text = String(text).trim();
			if (!text) return null;
			let sign = '';
			if (text[0] === '-') { sign = '-'; text = text.slice(1); }
			else if (text[0] === '+') { text = text.slice(1); }
			if (!text) return null;
			let exponent = 0;
			const e = text.search(/[eE]/);
			if (e >= 0) {
				const parsed = Number(text.slice(e + 1));
				if (!Number.isInteger(parsed)) return null;
				exponent = parsed;
				text = text.slice(0, e);
			}
			let digits = text;
			const dot = digits.indexOf('.');
			if (dot >= 0) {
				const fraction = digits.slice(dot + 1);
				digits = digits.slice(0, dot) + fraction;
				exponent -= fraction.length;
			}
			digits = digits.replace(/^0+/, '');
			if (!digits) return '0';
			while (digits.endsWith('0')) {
				digits = digits.slice(0, -1);
				exponent++;
			}
			return sign + digits + 'e' + exponent;
		};
		setValue(next);
		const validity = this.validity || {};
		let invalid = validity.badInput === true || validity.rangeUnderflow === true || validity.rangeOverflow === true || validity.stepMismatch === true;
		// 浏览器会 sanitize 非法输入：可能是空串，也可能像 range 一样 clamp
		// 或 round。用十进制规范形式比较请求值和实际值，既允许 5.0→5
		// 这类等价归一化，也能抓住跨 step 或精度丢失的改写。
		if (next !== '' && this.value === '') invalid = true;
		if (rangeRequested && this.value !== '') {
			const requested = canonicalDecimal(value);
			if (!Number.isFinite(this.valueAsNumber)) invalid = true;
			else {
				const actual = canonicalDecimal(String(this.valueAsNumber));
				if (requested === null || actual === null || requested !== actual) invalid = true;
			}
		}
		if (invalid) {
			setValue(oldValue);
			return {value: oldValue, invalid: true};
		}
		if (this.value === oldValue) return {value: this.value, invalid: false};
		this.dispatchEvent(new Event('input', {bubbles: true, composed: true}));
		this.dispatchEvent(new Event('change', {bubbles: true}));
		return {value: this.value, invalid: false};
	}`, map[string]any{"text": text, "append": appendValue}).ByUser())
	if err != nil {
		return err
	}
	var state struct {
		Value   string `json:"value"`
		Invalid bool   `json:"invalid"`
	}
	if err := result.Value.Unmarshal(&state); err != nil {
		return err
	}
	if state.Invalid {
		return newError("invalid_value", kind+" 取值无效或超出控件范围", "检查取值格式以及 min、max 和 step 限制", true)
	}
	if state.Value == "" && text != "" {
		return newError("invalid_value", kind+" 没有接受该值", "检查取值是否在该控件的合法范围内", true)
	}
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
	if binding, ok := page.frameBindingForRef(operationCtx, ref.Ref); ok {
		if err := page.validateFrameRef(operationCtx, ref); err != nil {
			return "", err
		}
		return page.selectFrameOption(operationCtx, binding, ref, value, label)
	}
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
	if binding, ok := page.frameBindingForRef(operationCtx, ref.Ref); ok {
		if err := page.validateFrameRef(operationCtx, ref); err != nil {
			return err
		}
		return page.uploadFrameFile(operationCtx, binding, ref.Ref, path)
	}
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

// HasRef 判断一个 ref 属性选择器当前是否仍能在 document、open shadow root
// 或同源 iframe 中命中元素。
func (page *rodPage) HasRef(ctx context.Context, selector string) (bool, error) {
	operationCtx, operationCancel, err := page.ctx(ctx)
	if err != nil {
		return false, err
	}
	defer operationCancel()
	if ref, ok := refFromSelector(selector); ok {
		if binding, found := page.frameBindingForRef(operationCtx, ref); found {
			return page.hasFrameRef(operationCtx, binding, ref)
		}
	}
	result, err := page.page.Context(operationCtx).Eval(`(selector) => {
		if (document.querySelector(selector)) return true;
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
		return roots.some(root => root.querySelector(selector));
	}`, selector)
	if err != nil {
		return false, err
	}
	return result.Value.Bool(), nil
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
	var frameWait *frameBinding
	if options.Condition == "selector_visible" || options.Condition == "selector_hidden" {
		if ref, ok := refFromSelector(options.Selector); ok {
			if binding, found := page.frameBindingForRef(operationCtx, ref); found {
				frameWait = &binding
			}
		}
	}
	for {
		if err := contextErr(operationCtx); err != nil {
			return err
		}
		if frameWait != nil {
			visible := page.frameRefVisible(operationCtx, *frameWait, refFromSelectorValue(options.Selector))
			if options.Condition == "selector_hidden" {
				visible = !visible
			}
			if visible {
				return nil
			}
			select {
			case <-operationCtx.Done():
				return contextErr(operationCtx)
			case <-deadline.C:
				continue
			}
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
		// 返回可调用的空 cancel：CancelFunc 是函数类型，Yaegi 处理裸 nil
		// 返回值时会在 reflect.New(nil) 处 panic；调用方仍需无条件执行 cancel。
		return nil, func() {}, err
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
