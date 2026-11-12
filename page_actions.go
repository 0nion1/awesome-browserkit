package browserkit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
)

const inputCleanupTimeout = 2 * time.Second

// callInput 显式传入 context，使正常输入和取消后的有界释放共用同一 CDP 路径。
func callInput(ctx context.Context, client proto.Client, request proto.Request) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	sessionID := ""
	if session, ok := client.(proto.Sessionable); ok {
		sessionID = string(session.GetSessionID())
	}
	_, err := client.Call(ctx, sessionID, request.ProtoReq(), request)
	return err
}

func clickParameters(options ClickOptions) (proto.InputMouseButton, int, error) {
	button := options.Button
	if button == "" {
		button = "left"
	}
	if button != "left" && button != "right" && button != "middle" {
		return "", 0, errors.New("button 必须是 left、right 或 middle")
	}
	count := options.ClickCount
	if count == 0 {
		count = 1
	}
	if count != 1 && count != 2 {
		return "", 0, errors.New("click_count 必须是 1 或 2")
	}
	return proto.InputMouseButton(button), count, nil
}

func (page *rodPage) clickElementLocked(ctx context.Context, element *rod.Element, options ClickOptions) error {
	if err := scrollElementNaturally(ctx, element); err != nil {
		return err
	}
	target, err := page.elementPoint(ctx, element)
	if err != nil {
		return err
	}
	if err := page.movePointer(page.page.Context(ctx), target.X, target.Y); err != nil {
		return err
	}
	return dispatchAtomicClick(ctx, page.page, target.X, target.Y, options)
}

// ClickPoint 在视口 CSS 坐标处发送真实鼠标点击。
func (page *rodPage) ClickPoint(ctx context.Context, x, y float64, options ClickOptions) error {
	if err := validateInputPoint(x, y); err != nil {
		return err
	}
	if _, _, err := clickParameters(options); err != nil {
		return err
	}
	operationCtx, cancel, err := page.ctx(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	if err := lockInteraction(operationCtx, &page.interactionMu); err != nil {
		return err
	}
	defer page.interactionMu.Unlock()
	return page.runUntilDialog(operationCtx, func(actionCtx context.Context) error {
		if err := page.movePointer(page.page.Context(actionCtx), x, y); err != nil {
			return err
		}
		return dispatchAtomicClick(actionCtx, page.page, x, y, options)
	})
}

func dispatchAtomicClick(ctx context.Context, client proto.Client, x, y float64, options ClickOptions) (resultErr error) {
	button, count, err := clickParameters(options)
	if err != nil {
		return err
	}
	pressed := mouseButtonMask(button)
	released := 0
	move := proto.InputDispatchMouseEvent{Type: proto.InputDispatchMouseEventTypeMouseMoved, X: x, Y: y, Button: proto.InputMouseButtonNone, Buttons: &released}
	if err := callInput(ctx, client, move); err != nil {
		return err
	}
	needsRelease := false
	defer func() {
		if needsRelease {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), inputCleanupTimeout)
			defer cancel()
			up := proto.InputDispatchMouseEvent{Type: proto.InputDispatchMouseEventTypeMouseReleased, X: x, Y: y, Button: button, Buttons: &released, ClickCount: 1}
			resultErr = errors.Join(resultErr, callInput(cleanupCtx, client, up))
		}
	}()
	for index := 1; index <= count; index++ {
		needsRelease = true // 请求失败时，浏览器仍可能已经接受按下事件。
		down := proto.InputDispatchMouseEvent{Type: proto.InputDispatchMouseEventTypeMousePressed, X: x, Y: y, Button: button, Buttons: &pressed, ClickCount: index}
		if err := callInput(ctx, client, down); err != nil {
			return err
		}
		up := proto.InputDispatchMouseEvent{Type: proto.InputDispatchMouseEventTypeMouseReleased, X: x, Y: y, Button: button, Buttons: &released, ClickCount: index}
		if err := callInput(ctx, client, up); err != nil {
			return err
		}
		needsRelease = false
	}
	return nil
}

func pressParameters(options PressOptions) ([]input.Key, error) {
	keys := map[string]input.Key{
		"Enter": input.Enter, "Tab": input.Tab, "Escape": input.Escape, "Backspace": input.Backspace,
		"Delete": input.Delete, "ArrowUp": input.ArrowUp, "ArrowDown": input.ArrowDown,
		"ArrowLeft": input.ArrowLeft, "ArrowRight": input.ArrowRight, "Space": input.Space,
		"Home": input.Home, "End": input.End, "PageUp": input.PageUp, "PageDown": input.PageDown,
	}
	key, ok := keys[options.Key]
	if !ok {
		if len(options.Key) != 1 || !strings.ContainsAny(options.Key, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") {
			return nil, newError("invalid_key", "不支持的按键", "使用单个英文字母、数字或编辑导航键", true)
		}
		key = input.Key(options.Key[0])
	}
	modifiers := map[string]input.Key{"Control": input.ControlLeft, "Meta": input.MetaLeft, "Alt": input.AltLeft, "Shift": input.ShiftLeft}
	seen := make(map[string]bool)
	result := make([]input.Key, 0, len(options.Modifiers)+1)
	for _, name := range options.Modifiers {
		modifier, exists := modifiers[name]
		if !exists || seen[name] {
			return nil, errors.New("modifiers 只允许不重复的 Control、Meta、Alt、Shift")
		}
		seen[name] = true
		result = append(result, modifier)
	}
	if seen["Shift"] {
		if shifted, exists := key.Shift(); exists {
			key = shifted
		}
	}
	return append(result, key), nil
}

func (page *rodPage) Press(ctx context.Context, options PressOptions) error {
	keys, err := pressParameters(options)
	if err != nil {
		return err
	}
	operationCtx, cancel, err := page.ctx(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	if err := lockInteraction(operationCtx, &page.interactionMu); err != nil {
		return err
	}
	defer page.interactionMu.Unlock()
	if options.Ref != nil {
		if binding, ok := page.frameBindingForRef(operationCtx, options.Ref.Ref); ok {
			if err := page.validateFrameRef(operationCtx, *options.Ref); err != nil {
				return err
			}
			if err := page.focusFrameElement(operationCtx, binding, options.Ref.Ref); err != nil {
				return err
			}
			return dispatchKeys(operationCtx, page.page, keys)
		}
		element, err := page.elementLocked(operationCtx, *options.Ref)
		if err != nil {
			return err
		}
		if err := scrollElementNaturally(operationCtx, element); err != nil {
			return err
		}
		if _, err := page.elementPoint(operationCtx, element); err != nil {
			return err
		}
		if _, err := element.Evaluate(rod.Eval(`function() { this.focus({preventScroll: true}); if (this.ownerDocument.activeElement !== this && this.getRootNode().activeElement !== this) throw new Error('目标元素无法聚焦'); }`).ByUser()); err != nil {
			return err
		}
	} else if err := page.syncState(operationCtx); err != nil {
		return err
	}
	return dispatchKeys(operationCtx, page.page, keys)
}

func dispatchKeys(ctx context.Context, client proto.Client, keys []input.Key) (resultErr error) {
	pressed := make([]input.Key, 0, len(keys))
	modifiers := 0
	defer func() {
		if len(pressed) == 0 {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), inputCleanupTimeout)
		defer cancel()
		for index := len(pressed) - 1; index >= 0; index-- {
			key := pressed[index]
			modifiers &^= key.Modifier()
			resultErr = errors.Join(resultErr, callInput(cleanupCtx, client, key.Encode(proto.InputDispatchKeyEventTypeKeyUp, modifiers)))
		}
	}()
	for _, key := range keys {
		modifiers |= key.Modifier()
		pressed = append(pressed, key)
		event := key.Encode(proto.InputDispatchKeyEventTypeKeyDown, modifiers)
		if modifiers&(input.ModifierControl|input.ModifierMeta|input.ModifierAlt) != 0 {
			event.Text = ""
			event.UnmodifiedText = ""
			event.Type = proto.InputDispatchKeyEventTypeRawKeyDown
			// Chromium 的编辑命令让远端浏览器也能按所给 Ctrl/Meta 语义执行。
			if key.Modifier() == 0 && (modifiers == input.ModifierControl || modifiers == input.ModifierMeta) {
				switch strings.ToLower(key.Info().Key) {
				case "a":
					event.Commands = []string{"selectAll"}
				case "c":
					event.Commands = []string{"copy"}
				case "v":
					event.Commands = []string{"paste"}
				case "x":
					event.Commands = []string{"cut"}
				case "z":
					event.Commands = []string{"undo"}
				}
			}
			if strings.ToLower(key.Info().Key) == "z" && (modifiers == input.ModifierControl|input.ModifierShift || modifiers == input.ModifierMeta|input.ModifierShift) {
				event.Commands = []string{"redo"}
			}
		}
		if err := callInput(ctx, client, event); err != nil {
			return err
		}
	}
	for len(pressed) > 0 {
		key := pressed[len(pressed)-1]
		modifiers &^= key.Modifier()
		if err := callInput(ctx, client, key.Encode(proto.InputDispatchKeyEventTypeKeyUp, modifiers)); err != nil {
			return err
		}
		pressed = pressed[:len(pressed)-1]
	}
	return nil
}

type checkedState struct {
	Supported bool `json:"supported"`
	Radio     bool `json:"radio"`
	Checked   bool `json:"checked"`
	Mixed     bool `json:"mixed"`
}

func readChecked(element *rod.Element) (checkedState, error) {
	result, err := element.Evaluate(rod.Eval(`function() {
		const native = this.tagName === 'INPUT' && (this.type === 'checkbox' || this.type === 'radio');
		const role = this.getAttribute('role');
		const checked = this.getAttribute('aria-checked');
		return {supported: native || (['checkbox','radio','switch'].includes(role) && ['true','false','mixed'].includes(checked)), radio: native ? this.type === 'radio' : role === 'radio', checked: native ? this.checked : checked === 'true', mixed: native ? this.indeterminate : checked === 'mixed'};
	}`).ByUser())
	if err != nil {
		return checkedState{}, err
	}
	var state checkedState
	err = result.Value.Unmarshal(&state)
	return state, err
}

func (page *rodPage) SetChecked(ctx context.Context, ref ElementRef, checked bool) (bool, error) {
	operationCtx, cancel, err := page.ctx(ctx)
	if err != nil {
		return false, err
	}
	defer cancel()
	if err := lockInteraction(operationCtx, &page.interactionMu); err != nil {
		return false, err
	}
	defer page.interactionMu.Unlock()
	if binding, ok := page.frameBindingForRef(operationCtx, ref.Ref); ok {
		if err := page.validateFrameRef(operationCtx, ref); err != nil {
			return false, err
		}
		return page.setFrameChecked(operationCtx, binding, ref, checked)
	}
	element, err := page.elementLocked(operationCtx, ref)
	if err != nil {
		return false, err
	}
	state, err := readChecked(element)
	if err != nil {
		return false, err
	}
	if !state.Supported || state.Radio && !checked {
		return false, newError("invalid_check_target", "目标必须是可勾选控件，radio 不能取消选中", "使用快照中的 checkbox、radio 或 switch；radio 只允许 checked=true", true)
	}
	if state.Checked == checked && !state.Mixed {
		return false, nil
	}
	if err := page.clickElementLocked(operationCtx, element, ClickOptions{}); err != nil {
		return false, err
	}
	state, err = readChecked(element)
	if err != nil {
		return true, err
	}
	if state.Checked != checked || state.Mixed {
		return true, newError("checked_state_mismatch", fmt.Sprintf("控件点击后未达到 checked=%t，点击已经发生", checked), "重新读取 browserSnapshot 确认页面状态，不要盲目重复点击", true)
	}
	return true, nil
}
