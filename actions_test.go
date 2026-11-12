package browserkit

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
)

type failingInputClient struct {
	recordingCDPClient
	failAt int
	cancel context.CancelFunc
}

func (client *failingInputClient) Call(ctx context.Context, sessionID, method string, params any) ([]byte, error) {
	_, _ = client.recordingCDPClient.Call(ctx, sessionID, method, params)
	if len(client.params) == client.failAt {
		if client.cancel != nil {
			client.cancel()
		}
		return nil, errors.New("输入连接中断")
	}
	return nil, ctx.Err()
}

func TestClickButtonsAndDoubleClickUseRealSequences(t *testing.T) {
	for _, options := range []ClickOptions{{}, {Button: "right"}, {Button: "middle"}, {ClickCount: 2}} {
		client := &recordingCDPClient{}
		if err := dispatchAtomicClick(context.Background(), client, 120, 80, options); err != nil {
			t.Fatal(err)
		}
		button, count, _ := clickParameters(options)
		if len(client.params) != 1+2*count {
			t.Fatalf("%+v 的事件序列不完整: %+v", options, client.params)
		}
		for index := 1; index <= count; index++ {
			down := client.params[index*2-1].(proto.InputDispatchMouseEvent)
			up := client.params[index*2].(proto.InputDispatchMouseEvent)
			if down.Button != button || down.ClickCount != index || *down.Buttons != mouseButtonMask(button) || up.Button != button || up.ClickCount != index || *up.Buttons != 0 {
				t.Fatalf("错误点击事件: down=%+v up=%+v", down, up)
			}
		}
	}
}

func TestCanceledMouseAndKeysAreReleasedWithFreshContext(t *testing.T) {
	t.Run("mouse", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		client := &failingInputClient{failAt: 2, cancel: cancel}
		if err := dispatchAtomicClick(ctx, client, 1, 2, ClickOptions{}); err == nil {
			t.Fatal("输入中断没有报错")
		}
		if len(client.params) != 3 || client.params[2].(proto.InputDispatchMouseEvent).Type != proto.InputDispatchMouseEventTypeMouseReleased {
			t.Fatalf("取消后未释放鼠标: %+v", client.params)
		}
	})
	t.Run("keys", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		client := &failingInputClient{failAt: 2, cancel: cancel}
		keys, err := pressParameters(PressOptions{Key: "a", Modifiers: []string{"Control"}})
		if err != nil {
			t.Fatal(err)
		}
		if err := dispatchKeys(ctx, client, keys); err == nil {
			t.Fatal("输入中断没有报错")
		}
		if len(client.params) != 4 {
			t.Fatalf("取消后未释放所有按键: %+v", client.params)
		}
		last := client.params[3].(*proto.InputDispatchKeyEvent)
		if last.Key != "Control" || last.Type != proto.InputDispatchKeyEventTypeKeyUp || last.Modifiers != 0 {
			t.Fatalf("修饰键未释放: %+v", last)
		}
	})
}

func TestPressValidatesBeforeInputAndEncodesModifiers(t *testing.T) {
	for _, options := range []PressOptions{{}, {Key: "F12"}, {Key: "ab"}, {Key: "a", Modifiers: []string{"Control", "Control"}}, {Key: "Enter", Modifiers: []string{"Unknown"}}} {
		if _, err := pressParameters(options); err == nil {
			t.Fatalf("无效按键未拒绝: %+v", options)
		}
	}
	for _, options := range []PressOptions{{Key: "a", Modifiers: []string{"Control"}}, {Key: "Tab", Modifiers: []string{"Shift"}}, {Key: "a", Modifiers: []string{"Shift"}}, {Key: "Home"}, {Key: "9"}, {Key: "Delete"}} {
		keys, err := pressParameters(options)
		if err != nil {
			t.Fatal(err)
		}
		client := &recordingCDPClient{}
		if err := dispatchKeys(context.Background(), client, keys); err != nil {
			t.Fatal(err)
		}
		if len(client.params) != len(keys)*2 {
			t.Fatalf("按键序列不平衡: %+v", client.params)
		}
		if options.Key == "a" && reflect.DeepEqual(options.Modifiers, []string{"Control"}) {
			down := client.params[1].(*proto.InputDispatchKeyEvent)
			if down.Modifiers != input.ModifierControl || down.Text != "" || !reflect.DeepEqual(down.Commands, []string{"selectAll"}) {
				t.Fatalf("组合键被编码为文本: %+v", down)
			}
		}
	}
}

func TestDragPairsMouseEventsAndPreservesNativeData(t *testing.T) {
	for _, native := range []bool{false, true} {
		client := &recordingCDPClient{}
		var intercepted <-chan *proto.InputDragData
		data := &proto.InputDragData{DragOperationsMask: 1}
		if native {
			ready := make(chan *proto.InputDragData, 1)
			ready <- data
			intercepted = ready
		}
		if err := dispatchDrag(context.Background(), client, elementPointResult{X: 10, Y: 20}, elementPointResult{X: 220, Y: 140}, intercepted); err != nil {
			t.Fatal(err)
		}
		down := client.params[0].(proto.InputDispatchMouseEvent)
		up := client.params[len(client.params)-1].(proto.InputDispatchMouseEvent)
		if down.Type != proto.InputDispatchMouseEventTypeMousePressed || up.Type != proto.InputDispatchMouseEventTypeMouseReleased || up.X != 220 || up.Y != 140 || *up.Buttons != 0 {
			t.Fatalf("拖放没有配对按下释放: %+v %+v", down, up)
		}
		for index := 1; index <= 12; index++ {
			move := client.params[index].(proto.InputDispatchMouseEvent)
			if move.Type != proto.InputDispatchMouseEventTypeMouseMoved || *move.Buttons != 1 {
				t.Fatalf("拖动过程中鼠标被释放: %+v", move)
			}
		}
		if native {
			for index, kind := range []proto.InputDispatchDragEventType{proto.InputDispatchDragEventTypeDragEnter, proto.InputDispatchDragEventTypeDragOver, proto.InputDispatchDragEventTypeDrop} {
				event := client.params[13+index].(proto.InputDispatchDragEvent)
				if event.Type != kind || event.Data != data || event.X != 220 || event.Y != 140 {
					t.Fatalf("原生拖放丢失数据或顺序: %+v", event)
				}
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &failingInputClient{failAt: 3, cancel: cancel}
	if err := dispatchDrag(ctx, client, elementPointResult{}, elementPointResult{X: 220, Y: 140}, nil); err == nil {
		t.Fatal("拖动中断未报错")
	}
	last := client.params[len(client.params)-1].(proto.InputDispatchMouseEvent)
	if last.Type != proto.InputDispatchMouseEventTypeMouseReleased || *last.Buttons != 0 {
		t.Fatalf("中断拖动后鼠标没有释放: %+v", last)
	}
}

func TestActionsReturnSnapshotAndPreserveCompletedClickOnReadFailure(t *testing.T) {
	manager, session := fakeManager(t, Config{})
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	request := ClickRequest{SessionID: snapshot.SessionID, PageID: snapshot.PageID, Ref: "e1", Revision: snapshot.Revision, Button: "right"}
	clicked, err := manager.Click(ctx, request)
	if err != nil || clicked.Snapshot == nil || clicked.Snapshot.PageID != request.PageID || clicked.SnapshotError != "" || session.page.clickOptions.Button != "right" {
		t.Fatalf("点击未返回同一页面快照: %+v %v", clicked, err)
	}
	session.page.snapshotErr = errors.New("页面暂时不可读取")
	clicked, err = manager.Click(ctx, request)
	if err != nil || !clicked.Changed || clicked.Snapshot != nil || clicked.SnapshotError == "" || session.page.clickCalls != 2 {
		t.Fatalf("读取失败覆盖了已完成点击: %+v calls=%d err=%v", clicked, session.page.clickCalls, err)
	}
	request.Button = "back"
	if _, err := manager.Click(ctx, request); err == nil || session.page.clickCalls != 2 {
		t.Fatal("错误点击参数到达执行层")
	}
	session.page.snapshotErr = nil
	pressed, err := manager.Press(ctx, PressRequest{SessionID: snapshot.SessionID, PageID: snapshot.PageID, Ref: "e1", Revision: snapshot.Revision, Key: "a", Modifiers: []string{"Control"}})
	if err != nil || pressed.Snapshot == nil || session.page.pressOptions.Ref == nil || session.page.pressOptions.Ref.Ref != "e1" {
		t.Fatalf("聚焦按键或快照失败: %+v %v", pressed, err)
	}
}

func TestSetCheckedAndDragValidatePageReferences(t *testing.T) {
	manager, session := fakeManager(t, Config{})
	defer manager.Close(context.Background())
	ctx := context.Background()
	opened, err := manager.Open(ctx, OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	session.page.snapshotData = &Snapshot{Revision: 1, Info: PageInfo{URL: opened.URL}, Elements: []Element{{Ref: "e1", Role: "checkbox"}, {Ref: "e2", Role: "button"}}}
	snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	check := SetCheckedRequest{SessionID: snapshot.SessionID, PageID: snapshot.PageID, Ref: "e1", Revision: 1, Checked: true}
	for index, changed := range []bool{true, false} {
		response, err := manager.SetChecked(ctx, check)
		if err != nil || response.Changed != changed || !response.Checked || response.Snapshot == nil {
			t.Fatalf("第 %d 次设置错误: %+v %v", index, response, err)
		}
	}
	drag := DragRequest{SessionID: snapshot.SessionID, PageID: snapshot.PageID, SourceRef: "e1", TargetRef: "e2", Revision: 1}
	response, err := manager.Drag(ctx, drag)
	if err != nil || response.Snapshot == nil || session.page.dragSource.Ref != "e1" || session.page.dragTarget.Ref != "e2" {
		t.Fatalf("拖放未执行或缺少快照: %+v %v", response, err)
	}
	selectorDrag := drag
	selectorDrag.TargetRef = "#drop-zone"
	response, err = manager.Drag(ctx, selectorDrag)
	if err != nil || response.Snapshot == nil || session.page.dragTarget.Ref != "#drop-zone" {
		t.Fatalf("selector 拖放未执行: %+v %v", response, err)
	}
	for _, invalid := range []DragRequest{
		{SessionID: drag.SessionID, PageID: drag.PageID, SourceRef: "e1", TargetRef: "e2", Revision: 2},
		{SessionID: drag.SessionID, PageID: drag.PageID, SourceRef: "e1", TargetRef: "eabc123_4", Revision: 1},
		{SessionID: drag.SessionID, PageID: drag.PageID, SourceRef: "e1", TargetRef: "e1", Revision: 1},
		{SessionID: drag.SessionID, PageID: "other-page", SourceRef: "e1", TargetRef: "e2", Revision: 1},
	} {
		if _, err := manager.Drag(ctx, invalid); err == nil {
			t.Fatalf("无效目标被接受: %+v", invalid)
		}
	}
	check.Revision = 2
	if _, err := manager.SetChecked(ctx, check); err == nil {
		t.Fatal("勾选接受过期 revision")
	}
}
