package browserkit

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

const pageSnapshotScript = `(options) => {
		const suppressContent = options.suppress_content === true;
		const suppressText = suppressContent || options.suppress_text === true;
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
			try { if (el.checkVisibility && !el.checkVisibility({checkOpacity: true, checkVisibilityCSS: true})) return false; } catch {}
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
		const maxElements = Math.max(0, options.max_elements ?? 100);
		// 多扫描一个候选只用于判断是否还有下一页，避免把“恰好满页”
		// 误报成还需继续分页。
		const scanLimit = offset + maxElements + 1;
		const roots = [];
		const visit = root => {
			roots.push(root);
			for (const el of root.querySelectorAll('*')) {
				if (el.shadowRoot) visit(el.shadowRoot);
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
			const ref = previous.startsWith(state.refPrefix + '_') && state.nodes.get(previous) === el ? previous : state.refPrefix + '_' + (++state.nextRef);
			el.setAttribute('data-browserkit-ref', ref);
			state.refs.add(ref);
			state.nodes.set(ref, el);
			const type = (el.getAttribute('type') || '').toLowerCase();
			const contentEditableRoot = el.isContentEditable && !(el.parentElement && el.parentElement.isContentEditable);
			const inputRole = type === 'file' ? 'button' : type === 'checkbox' ? 'checkbox' : type === 'radio' ? 'radio' :
				(type === 'button' || type === 'submit' || type === 'reset' || type === 'image') ? 'button' :
				type === 'range' ? 'slider' : type === 'number' ? 'spinbutton' : type === 'search' ? 'searchbox' : 'textbox';
			const role = el.getAttribute('role') || (el.tagName === 'IFRAME' ? 'frame' : el.tagName === 'A' ? 'link' : el.tagName === 'BUTTON' ? 'button' : el.tagName === 'INPUT' ? inputRole : el.tagName === 'SELECT' ? 'combobox' : el.tagName === 'TEXTAREA' || contentEditableRoot ? 'textbox' : 'button');
			const labelledBy = (el.getAttribute('aria-labelledby') || '').split(/\s+/).filter(Boolean).map(id => el.getRootNode().getElementById?.(id)?.textContent || el.ownerDocument.getElementById(id)?.textContent || '').join(' ');
			const nativeLabel = [...(el.labels || [])].map(label => label.innerText || label.textContent || '').join(' ');
			const buttonValue = el.tagName === 'INPUT' && ['button', 'submit', 'reset'].includes(type) ? el.value : '';
			const label = el.getAttribute('aria-label') || labelledBy || nativeLabel || el.getAttribute('placeholder') || el.getAttribute('title') || (!suppressContent && (text(el) || buttonValue)) || el.getAttribute('name') || el.id || '';
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
			const valueHidden = suppressContent || masked || valueRect.width <= 1 || valueRect.height <= 1 || effectivelyHidden(el);
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
		for (const [ref, node] of state.nodes) if (!node.isConnected || node.getAttribute('data-browserkit-ref') !== ref) state.nodes.delete(ref);
		state.refs = new Set(state.nodes.keys());
		let active = document.activeElement;
		for (;;) {
			let nested;
			try { nested = active?.shadowRoot?.activeElement || (active?.tagName === 'IFRAME' ? active.contentDocument?.activeElement : null); } catch {}
			if (!nested || nested === active) break;
			active = nested;
		}
		const activeRef = active?.getAttribute?.('data-browserkit-ref') || '';
		const activeValue = suppressContent || (active && effectivelyHidden(active)) ? '' : active && ('value' in active) ? String(active.value ?? '') : (active?.isContentEditable ? String(active.textContent || '') : '');
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
						crossFrame = true;
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
		const maxChars = Math.max(0, options.max_chars ?? 12000);
		// 多收一点给 clean 的空白折叠留余量，避免刚好卡在预算边界时误判截断。
		const collected = suppressText ? {text: '', stopped: false, crossFrame: false} : visibleText(document.body || document.documentElement, maxChars + 64);
		const bodyText = clean(collected.text);
		return {url: location.href, title: document.title, text: bodyText.slice(0, maxChars), elements, total: Math.min(candidates.length, offset + elements.length), refs: [...state.refs], has_more: candidates.length > offset + elements.length, text_truncated: bodyText.length > maxChars || collected.stopped, cross_frame: collected.crossFrame, active_element_ref: activeRef, active_element_value: activeValue};
	}`

func (page *rodPage) mergeFrameSnapshots(ctx context.Context, raw *domSnapshot, options SnapshotOptions) []FrameCoverage {
	coverage := []FrameCoverage{{FrameID: string(page.page.FrameID), URL: raw.URL, Status: "captured", ShadowDOM: "open_only"}}
	if raw.HasMore || raw.TextTruncated {
		coverage[0].Status = "partial"
		coverage[0].Reason = "snapshot_budget"
	}
	frames, err := page.frameBindings(ctx)
	if err != nil {
		raw.TextTruncated = true
		return append(coverage, FrameCoverage{FrameID: "*", Status: "unavailable", Reason: "frame_discovery: " + err.Error(), ShadowDOM: "open_only"})
	}
	elementOffset := options.Offset - raw.Total
	if elementOffset < 0 {
		elementOffset = 0
	}
	mainHasMore := raw.HasMore
	for _, frame := range frames {
		entry := FrameCoverage{FrameID: string(frame.FrameID), URL: frame.URL, Status: "captured", ShadowDOM: "open_only"}
		if frame.Hidden {
			entry.Status = "hidden"
			coverage = append(coverage, entry)
			continue
		}
		if frame.Unavailable != "" {
			entry.Status = "unavailable"
			entry.Reason = frame.Unavailable
			raw.TextTruncated = true
			coverage = append(coverage, entry)
			continue
		}
		room := options.MaxElements - len(raw.Elements)
		if mainHasMore {
			room = 0
		}
		textRoom := options.MaxChars - utf8.RuneCountInString(raw.Text)
		label := "\n[frame " + frame.URL + "]\n"
		textRoom -= utf8.RuneCountInString(label)
		if textRoom < 0 {
			textRoom = 0
		}
		args, _ := json.Marshal(map[string]any{"max_chars": textRoom, "max_elements": room, "offset": elementOffset, "suppress_content": frame.MetadataOnly, "suppress_text": frame.SuppressText})
		expression := "(() => { const state = " + pageStateObserverScript + "; state.refPrefix = " + strconvQuote(frame.Prefix) + "; return (" + pageSnapshotScript + ")(" + string(args) + "); })()"
		data, ok := page.frameEvaluate(ctx, frame, expression)
		var child domSnapshot
		if !ok || json.Unmarshal(data, &child) != nil {
			entry.Status, entry.Reason = "unavailable", "frame_snapshot_failed"
			raw.TextTruncated = true
			// 观察失败无法证明以前签发的引用已经消失。
			page.stateMu.Lock()
			for ref := range page.refs {
				if strings.HasPrefix(ref, frame.Prefix+"_") {
					raw.Refs = append(raw.Refs, ref)
				}
			}
			page.stateMu.Unlock()
			coverage = append(coverage, entry)
			continue
		}
		if child.Text != "" && textRoom > 0 {
			raw.Text += label + child.Text
		}
		if child.TextTruncated {
			raw.TextTruncated = true
		}
		for index := range child.Elements {
			child.Elements[index].FrameURL = frame.URL
			child.Elements[index].Locator.FrameURL = frame.URL
		}
		raw.Elements = append(raw.Elements, child.Elements...)
		raw.Refs = append(raw.Refs, child.Refs...)
		raw.HasMore = raw.HasMore || child.HasMore
		if elementOffset >= child.Total {
			elementOffset -= child.Total
		} else {
			elementOffset = 0
		}
		if frame.SuppressText {
			entry.Status, entry.Reason = "partial", "text_not_visible"
		}
		if frame.MetadataOnly {
			entry.Status, entry.Reason = "partial", "metadata_only"
		}
		if child.HasMore || child.TextTruncated {
			entry.Status, entry.Reason = "partial", "snapshot_budget"
		}
		coverage = append(coverage, entry)
	}
	// 字段值预览在整个 Tab 的所有 frame 间共享预算。
	valueBudget := 4000
	for index := range raw.Elements {
		value := []rune(raw.Elements[index].Value)
		if len(value) > valueBudget {
			raw.Elements[index].Value = string(value[:valueBudget])
			raw.Elements[index].ValueTruncated = true
			valueBudget = 0
		} else {
			valueBudget -= len(value)
		}
	}
	return coverage
}
