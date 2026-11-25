package browserkit

import (
	"context"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/proto"
)

type recordingCDPClient struct {
	methods []string
	params  []any
}

func (client *recordingCDPClient) Call(_ context.Context, _, method string, params any) ([]byte, error) {
	client.methods = append(client.methods, method)
	client.params = append(client.params, params)
	return nil, nil
}

func TestContextWithTimeoutUsesConfiguredLimit(t *testing.T) {
	started := time.Now()
	ctx, cancel, err := contextWithTimeout(context.Background(), 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	<-ctx.Done()
	if !isDurationNear(time.Since(started), 20*time.Millisecond) {
		t.Fatalf("操作 context 没有使用配置时限: %v", time.Since(started))
	}
}

func TestContextWithTimeoutCanceledContextReturnsCallableCancel(t *testing.T) {
	parent, parentCancel := context.WithCancel(context.Background())
	parentCancel()
	_, cancel, err := contextWithTimeout(parent, time.Second)
	if err == nil {
		t.Fatal("已取消的 context 不应继续创建操作 context")
	}
	// Yaegi 无法把 CancelFunc 的裸 nil 结果包装成函数值；调用方仍需无条件调用。
	cancel()
}

func TestViewportValidateBoundsRenderingResources(t *testing.T) {
	for _, viewport := range []Viewport{{Width: 1, Height: 1}, {Width: 8_192, Height: 8_192}, {Width: 16_384, Height: 4_096}} {
		if err := viewport.Validate(); err != nil {
			t.Fatalf("Validate(%+v): %v", viewport, err)
		}
	}
	for _, viewport := range []Viewport{{}, {Width: 800}, {Width: -1, Height: 600}, {Width: 16_385, Height: 1}, {Width: 8_193, Height: 8_192}} {
		if err := viewport.Validate(); err == nil {
			t.Fatalf("Validate(%+v) 没有拒绝异常尺寸", viewport)
		}
	}
}

func TestContextWithTimeoutKeepsShorterCallerDeadline(t *testing.T) {
	parent, parentCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer parentCancel()
	started := time.Now()
	ctx, cancel, err := contextWithTimeout(parent, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	<-ctx.Done()
	if !isDurationNear(time.Since(started), 20*time.Millisecond) {
		t.Fatalf("操作 context 没有保留更短的调用方 deadline: %v", time.Since(started))
	}
}

func isDurationNear(value, expected time.Duration) bool {
	return value >= expected/2 && value < expected+500*time.Millisecond
}

func TestBrowserInputTypesMapToCDPActions(t *testing.T) {
	mouseCases := map[string]string{"down": "mousePressed", "up": "mouseReleased", "move": "mouseMoved", "wheel": "mouseWheel"}
	for action, expected := range mouseCases {
		actual, err := mouseEventType(action)
		if err != nil || string(actual) != expected {
			t.Fatalf("mouse %s=%q, %v", action, actual, err)
		}
	}
	touchCases := map[string]string{"start": "touchStart", "end": "touchEnd", "move": "touchMove", "cancel": "touchCancel"}
	for action, expected := range touchCases {
		actual, err := touchEventType(action)
		if err != nil || string(actual) != expected {
			t.Fatalf("touch %s=%q, %v", action, actual, err)
		}
	}
	if _, err := mouseEventType("click"); err == nil {
		t.Fatal("未知鼠标 action 没有被拒绝")
	}
	if _, err := touchEventType("tap"); err == nil {
		t.Fatal("未知 touch action 没有被拒绝")
	}
}

func TestBrowserShortcutKeys(t *testing.T) {
	for action, expected := range map[string]string{"select_all": "a", "copy": "c", "paste": "v"} {
		key, err := shortcutInputKey(action)
		if err != nil || string(rune(key)) != expected {
			t.Fatalf("快捷键 %s=%q, %v", action, key, err)
		}
	}
	if _, err := shortcutInputKey("cut"); err == nil {
		t.Fatal("未允许的剪切快捷键没有被拒绝")
	}
}

func TestAtomicClickUsesExplicitMouseSequence(t *testing.T) {
	client := &recordingCDPClient{}
	if err := dispatchAtomicClick(context.Background(), client, 120, 80, ClickOptions{}); err != nil {
		t.Fatal(err)
	}
	want := []proto.InputDispatchMouseEventType{
		proto.InputDispatchMouseEventTypeMouseMoved,
		proto.InputDispatchMouseEventTypeMousePressed,
		proto.InputDispatchMouseEventTypeMouseReleased,
	}
	if len(client.params) != len(want) {
		t.Fatalf("鼠标原子事件数=%d want=%d", len(client.params), len(want))
	}
	for index, expected := range want {
		event, ok := client.params[index].(proto.InputDispatchMouseEvent)
		if !ok || event.Type != expected || event.X != 120 || event.Y != 80 {
			t.Fatalf("鼠标事件[%d]=%#v", index, client.params[index])
		}
	}
}

func TestMovePointerUsesDeterministicMultiPointTrajectory(t *testing.T) {
	page := &rodPage{}
	client := &recordingCDPClient{}
	if err := page.movePointer(client, 240, 120); err != nil {
		t.Fatal(err)
	}
	if len(client.params) < 3 {
		t.Fatalf("轨迹点过少: %d", len(client.params))
	}
	last, ok := client.params[len(client.params)-1].(proto.InputDispatchMouseEvent)
	if !ok || last.X != 240 || last.Y != 120 {
		t.Fatalf("轨迹终点=%#v", client.params[len(client.params)-1])
	}
	firstCount := len(client.params)
	if err := page.movePointer(client, 240, 120); err != nil {
		t.Fatal(err)
	}
	if len(client.params)-firstCount != 3 {
		t.Fatalf("相同坐标的轨迹应保持最小步数: %d", len(client.params)-firstCount)
	}
}

func TestShortcutUsesPairedAtomicKeyEvents(t *testing.T) {
	client := &recordingCDPClient{}
	if err := dispatchShortcut(client, "copy", 4); err != nil {
		t.Fatal(err)
	}
	if len(client.params) != 4 {
		t.Fatalf("快捷键原子事件数=%d want=4", len(client.params))
	}
	want := []proto.InputDispatchKeyEventType{
		proto.InputDispatchKeyEventTypeRawKeyDown,
		proto.InputDispatchKeyEventTypeKeyDown,
		proto.InputDispatchKeyEventTypeKeyUp,
		proto.InputDispatchKeyEventTypeKeyUp,
	}
	for index, expected := range want {
		event, ok := client.params[index].(proto.InputDispatchKeyEvent)
		if !ok || event.Type != expected {
			t.Fatalf("快捷键事件[%d]=%#v", index, client.params[index])
		}
	}
	modifierDown := client.params[0].(proto.InputDispatchKeyEvent)
	keyDown := client.params[1].(proto.InputDispatchKeyEvent)
	if modifierDown.Key != "Meta" || modifierDown.Modifiers != 4 || keyDown.Modifiers != 4 ||
		len(keyDown.Commands) != 1 || keyDown.Commands[0] != "copy" || keyDown.Text != "" {
		t.Fatalf("Meta+Copy CDP 编码错误: modifier=%+v key=%+v", modifierDown, keyDown)
	}
}

func TestShortcutRejectsUnsupportedModifier(t *testing.T) {
	if err := dispatchShortcut(&recordingCDPClient{}, "copy", 1); err == nil {
		t.Fatal("没有拒绝 Alt 快捷键")
	}
}

func TestMouseButtonMaskMatchesCDPButtons(t *testing.T) {
	for button, want := range map[proto.InputMouseButton]int{
		proto.InputMouseButtonNone: 0, proto.InputMouseButtonLeft: 1,
		proto.InputMouseButtonRight: 2, proto.InputMouseButtonMiddle: 4,
		proto.InputMouseButtonBack: 8, proto.InputMouseButtonForward: 16,
	} {
		if got := mouseButtonMask(button); got != want {
			t.Fatalf("mouseButtonMask(%q)=%d want=%d", button, got, want)
		}
	}
}

func TestMapFramePoint(t *testing.T) {
	for _, quad := range [][]float64{
		{10, 20, 110, 20, 110, 60, 10, 60},
		{110, 20, 10, 20, 10, 60, 110, 60},
		{110, 60, 10, 60, 10, 20, 110, 20},
		{10, 20, 110, 40, 120, 80, 20, 60},
	} {
		x, y, ok := mapFramePoint(quad, 100, 40, 50, 20)
		if !ok || x != (quad[0]+quad[4])/2 || y != (quad[1]+quad[5])/2 {
			t.Fatalf("quad=%v mapped=(%v,%v),%v", quad, x, y, ok)
		}
	}
	for _, quad := range [][]float64{{10, 20, 30}, {0, 0, 100, 0, 80, 100, 0, 100}, {0, 0, 0, 0, 0, 0, 0, 0}} {
		if _, _, ok := mapFramePoint(quad, 100, 100, 50, 50); ok {
			t.Fatalf("invalid/perspective quad accepted: %v", quad)
		}
	}
}
