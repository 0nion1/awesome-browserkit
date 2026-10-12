package browserkit

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
)

func (page *rodPage) DispatchInput(ctx context.Context, event InputEvent) error {
	if err := contextErr(ctx); err != nil {
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
	client := page.page.Context(operationCtx)
	switch event.Kind {
	case "mouse":
		typeValue, err := mouseEventType(event.Action)
		if err != nil {
			return err
		}
		button, err := mouseButton(event.Button)
		if err != nil {
			return err
		}
		pointerType := proto.InputDispatchMouseEventPointerTypeMouse
		if event.PointerType == "pen" {
			pointerType = proto.InputDispatchMouseEventPointerTypePen
		} else if event.PointerType != "" && event.PointerType != "mouse" {
			return fmt.Errorf("不支持的鼠标 pointer_type: %q", event.PointerType)
		}
		if err := validateInputPoint(event.X, event.Y); err != nil {
			return err
		}
		buttons := event.Buttons
		if event.Action == "down" && buttons == 0 {
			buttons = mouseButtonMask(button)
		}
		err = (proto.InputDispatchMouseEvent{
			Type: typeValue, X: event.X, Y: event.Y, DeltaX: event.DeltaX, DeltaY: event.DeltaY,
			Button: button, Buttons: &buttons, ClickCount: event.ClickCount, Modifiers: event.Modifiers,
			PointerType: pointerType,
		}).Call(client)
		if err == nil {
			page.pointerMu.Lock()
			page.pointerX, page.pointerY, page.pointerKnown = event.X, event.Y, true
			page.pointerMu.Unlock()
		}
		return err
	case "touch":
		typeValue, err := touchEventType(event.Action)
		if err != nil {
			return err
		}
		points := make([]*proto.InputTouchPoint, 0, len(event.Touches))
		for _, touch := range event.Touches {
			if err := validateInputPoint(touch.X, touch.Y); err != nil {
				return err
			}
			id := float64(touch.ID)
			point := &proto.InputTouchPoint{X: touch.X, Y: touch.Y, ID: &id}
			if touch.RadiusX > 0 {
				point.RadiusX = &touch.RadiusX
			}
			if touch.RadiusY > 0 {
				point.RadiusY = &touch.RadiusY
			}
			if touch.Force > 0 {
				point.Force = &touch.Force
			}
			points = append(points, point)
		}
		if (event.Action == "start" || event.Action == "move") && len(points) == 0 {
			return errors.New("touch start/move 必须包含活动触点")
		}
		if (event.Action == "end" || event.Action == "cancel") && len(points) != 0 {
			return errors.New("touch end/cancel 不能包含活动触点")
		}
		return (proto.InputDispatchTouchEvent{Type: typeValue, TouchPoints: points, Modifiers: event.Modifiers}).Call(client)
	case "key":
		typeValue := proto.InputDispatchKeyEventTypeKeyUp
		if event.Action == "down" {
			typeValue = proto.InputDispatchKeyEventTypeKeyDown
			if event.Text == "" {
				typeValue = proto.InputDispatchKeyEventTypeRawKeyDown
			}
		} else if event.Action != "up" {
			return fmt.Errorf("不支持的键盘 action: %q", event.Action)
		}
		location := event.Location
		// KeyEvent 携带 code、location、native key code 等 Rod Keyboard API
		// 无法完整表达的字段，必须保留 CDP Input.dispatchKeyEvent。
		return (proto.InputDispatchKeyEvent{
			Type: typeValue, Modifiers: event.Modifiers, Text: event.Text, UnmodifiedText: event.Text,
			Code: event.Code, Key: event.Key, WindowsVirtualKeyCode: event.KeyCode,
			NativeVirtualKeyCode: event.KeyCode, AutoRepeat: event.AutoRepeat,
			IsKeypad: event.Location == 3, IsSystemKey: event.Modifiers&1 != 0, Location: &location,
		}).Call(client)
	case "shortcut":
		return dispatchShortcut(client, event.Action, event.Modifiers)
	case "composition":
		if event.SelectionStart < 0 || event.SelectionEnd < event.SelectionStart {
			return errors.New("输入法选区无效")
		}
		// Rod 没有输入法 composition 包装，只能调用 CDP Input.imeSetComposition。
		return (proto.InputImeSetComposition{
			Text: event.Text, SelectionStart: event.SelectionStart, SelectionEnd: event.SelectionEnd,
		}).Call(client)
	case "text":
		if event.Text == "" {
			return nil
		}
		return (proto.InputInsertText{Text: event.Text}).Call(client)
	default:
		return fmt.Errorf("不支持的浏览器输入 kind: %q", event.Kind)
	}
}

func dispatchShortcut(client proto.Client, action string, modifiers int) error {
	key, err := shortcutInputKey(action)
	if err != nil {
		return err
	}
	modifierKey := input.ControlLeft
	switch modifiers {
	case 0:
		if input.IsMac {
			modifierKey = input.MetaLeft
		}
	case input.ModifierControl:
	case input.ModifierMeta:
		modifierKey = input.MetaLeft
	default:
		return errors.New("快捷键只允许 Ctrl 或 Meta 修饰键")
	}
	modifier := modifierKey.Modifier()
	modifierUp := modifierKey.Encode(proto.InputDispatchKeyEventTypeKeyUp, 0)
	if err := modifierKey.Encode(proto.InputDispatchKeyEventTypeKeyDown, modifier).Call(client); err != nil {
		return err
	}
	keyDown := key.Encode(proto.InputDispatchKeyEventTypeKeyDown, modifier)
	keyDown.Text = ""
	keyDown.Commands = []string{shortcutInputCommand(action)}
	if err := keyDown.Call(client); err != nil {
		_ = modifierUp.Call(client)
		return err
	}
	if err := key.Encode(proto.InputDispatchKeyEventTypeKeyUp, modifier).Call(client); err != nil {
		_ = modifierUp.Call(client)
		return err
	}
	return modifierUp.Call(client)
}

func shortcutInputCommand(action string) string {
	switch action {
	case "select_all":
		return "selectAll"
	case "copy":
		return "copy"
	case "paste":
		return "paste"
	default:
		return ""
	}
}

func mouseButtonMask(button proto.InputMouseButton) int {
	switch button {
	case proto.InputMouseButtonLeft:
		return 1
	case proto.InputMouseButtonRight:
		return 2
	case proto.InputMouseButtonMiddle:
		return 4
	case proto.InputMouseButtonBack:
		return 8
	case proto.InputMouseButtonForward:
		return 16
	default:
		return 0
	}
}

func shortcutInputKey(action string) (input.Key, error) {
	switch action {
	case "select_all":
		return input.Key('a'), nil
	case "copy":
		return input.Key('c'), nil
	case "paste":
		return input.Key('v'), nil
	default:
		return 0, fmt.Errorf("不支持的浏览器快捷键: %q", action)
	}
}

func mouseEventType(action string) (proto.InputDispatchMouseEventType, error) {
	switch action {
	case "down":
		return proto.InputDispatchMouseEventTypeMousePressed, nil
	case "up":
		return proto.InputDispatchMouseEventTypeMouseReleased, nil
	case "move":
		return proto.InputDispatchMouseEventTypeMouseMoved, nil
	case "wheel":
		return proto.InputDispatchMouseEventTypeMouseWheel, nil
	default:
		return "", fmt.Errorf("不支持的鼠标 action: %q", action)
	}
}

func touchEventType(action string) (proto.InputDispatchTouchEventType, error) {
	switch action {
	case "start":
		return proto.InputDispatchTouchEventTypeTouchStart, nil
	case "end":
		return proto.InputDispatchTouchEventTypeTouchEnd, nil
	case "move":
		return proto.InputDispatchTouchEventTypeTouchMove, nil
	case "cancel":
		return proto.InputDispatchTouchEventTypeTouchCancel, nil
	default:
		return "", fmt.Errorf("不支持的 touch action: %q", action)
	}
}

func mouseButton(value string) (proto.InputMouseButton, error) {
	if value == "" {
		return proto.InputMouseButtonNone, nil
	}
	switch button := proto.InputMouseButton(value); button {
	case proto.InputMouseButtonNone, proto.InputMouseButtonLeft, proto.InputMouseButtonMiddle,
		proto.InputMouseButtonRight, proto.InputMouseButtonBack, proto.InputMouseButtonForward:
		return button, nil
	default:
		return "", fmt.Errorf("不支持的鼠标 button: %q", value)
	}
}

func validateInputPoint(x, y float64) error {
	if math.IsNaN(x) || math.IsInf(x, 0) || math.IsNaN(y) || math.IsInf(y, 0) || x < 0 || y < 0 {
		return errors.New("浏览器输入坐标无效")
	}
	return nil
}
