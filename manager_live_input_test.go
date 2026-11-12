package browserkit

import (
	"context"
	"errors"
	"testing"
	"time"
)

func waitLiveInputs(t *testing.T, page *fakePage, count int) []InputEvent {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		page.inputMu.Lock()
		inputs := append([]InputEvent(nil), page.inputs...)
		page.inputMu.Unlock()
		if len(inputs) >= count {
			return inputs
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 %d 个直播输入超时: %+v", count, inputs)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestManagerKeepsQueuedTrackpadSamplesLossless(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{
		URL: "https://example.com", Viewport: Viewport{Width: 800, Height: 600},
	})
	if err != nil {
		t.Fatal(err)
	}
	blocked := make(chan struct{})
	session.page.inputBlock = blocked
	if err := manager.DispatchLiveInput(context.Background(), opened.PageID, InputEvent{Kind: "mouse", Action: "wheel", X: 10, Y: 10, DeltaY: 1}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		current := manager.sessions[opened.SessionID]
		current.mu.Lock()
		started := current.inputCancel != nil
		current.mu.Unlock()
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("首条滚动没有开始投递")
		}
		time.Sleep(time.Millisecond)
	}
	for index := 2; index <= 31; index++ {
		if err := manager.DispatchLiveInput(context.Background(), opened.PageID, InputEvent{Kind: "mouse", Action: "wheel", X: float64(index), Y: 10, DeltaY: 1}); err != nil {
			t.Fatal(err)
		}
	}
	close(blocked)

	deadline = time.Now().Add(time.Second)
	for {
		session.page.inputMu.Lock()
		inputs := append([]InputEvent(nil), session.page.inputs...)
		session.page.inputMu.Unlock()
		if len(inputs) >= 31 {
			if len(inputs) != 31 || inputs[0].DeltaY != 1 || inputs[30].DeltaY != 1 || inputs[30].X != 31 {
				t.Fatalf("滚轮相对位移没有完整保序: %+v", inputs)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待聚合滚动投递超时: %+v", inputs)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestManagerKeepsQueuedTouchMovesLossless(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{
		URL: "https://example.com", Viewport: Viewport{Width: 800, Height: 600},
	})
	if err != nil {
		t.Fatal(err)
	}
	blocked := make(chan struct{})
	session.page.inputBlock = blocked
	start := InputEvent{Kind: "touch", Action: "start", Touches: []InputTouchPoint{{ID: 7, X: 10, Y: 10}}}
	if err := manager.DispatchLiveInput(context.Background(), opened.PageID, start); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		current := manager.sessions[opened.SessionID]
		current.mu.Lock()
		started := current.inputCancel != nil
		current.mu.Unlock()
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("触摸开始没有进入 CDP 投递")
		}
		time.Sleep(time.Millisecond)
	}
	for index := 2; index <= 31; index++ {
		move := InputEvent{Kind: "touch", Action: "move", Touches: []InputTouchPoint{{ID: 7, X: float64(index), Y: 10}}}
		if err := manager.DispatchLiveInput(context.Background(), opened.PageID, move); err != nil {
			t.Fatal(err)
		}
	}
	end := InputEvent{Kind: "touch", Action: "end"}
	if err := manager.DispatchLiveInput(context.Background(), opened.PageID, end); err != nil {
		t.Fatal(err)
	}
	close(blocked)

	deadline = time.Now().Add(time.Second)
	for {
		session.page.inputMu.Lock()
		inputs := append([]InputEvent(nil), session.page.inputs...)
		session.page.inputMu.Unlock()
		if len(inputs) >= 32 {
			if len(inputs) != 32 || inputs[0].Action != "start" || inputs[1].Action != "move" ||
				inputs[30].Action != "move" || inputs[30].Touches[0].X != 31 || inputs[31].Action != "end" {
				t.Fatalf("触摸输入没有完整保序: %+v", inputs)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待聚合触摸投递超时: %+v", inputs)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestManagerDoesNotEvictShortcutFromFullMoveQueue(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{
		URL: "https://example.com", Viewport: Viewport{Width: 800, Height: 600},
	})
	if err != nil {
		t.Fatal(err)
	}
	blocked := make(chan struct{})
	session.page.inputBlock = blocked
	if err := manager.DispatchLiveInput(context.Background(), opened.PageID, InputEvent{Kind: "mouse", Action: "move", X: 1, Y: 1}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		current := manager.sessions[opened.SessionID]
		current.mu.Lock()
		started := current.inputCancel != nil
		current.mu.Unlock()
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("首条移动没有开始投递")
		}
		time.Sleep(time.Millisecond)
	}
	if err := manager.DispatchLiveInput(context.Background(), opened.PageID, InputEvent{Kind: "shortcut", Action: "copy", Modifiers: 4}); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < liveInputQueueSize+32; index++ {
		if err := manager.DispatchLiveInput(context.Background(), opened.PageID, InputEvent{Kind: "mouse", Action: "move", X: float64(index + 2), Y: 1}); err != nil {
			t.Fatal(err)
		}
	}
	close(blocked)

	deadline = time.Now().Add(time.Second)
	for {
		session.page.inputMu.Lock()
		inputs := append([]InputEvent(nil), session.page.inputs...)
		session.page.inputMu.Unlock()
		for _, event := range inputs {
			if event.Kind == "shortcut" && event.Action == "copy" && event.Modifiers == 4 {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("满载移动队列丢失了快捷键: %+v", inputs)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestManagerInterpolatesPointerLocallyAndReachesExactTarget(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{
		URL: "https://example.com", Viewport: Viewport{Width: 800, Height: 600},
	})
	if err != nil {
		t.Fatal(err)
	}
	first := InputEvent{Kind: "mouse", Action: "move", X: 100, Y: 100}
	if err := manager.DispatchLiveInput(context.Background(), opened.PageID, first); err != nil {
		t.Fatal(err)
	}
	waitLiveInputs(t, session.page, 1)
	time.Sleep(64 * time.Millisecond)
	target := InputEvent{
		Kind: "mouse", Action: "move", X: 500, Y: 100,
		Button: "left", Buttons: 1, PointerType: "pen", Modifiers: 8,
	}
	if err := manager.DispatchLiveInput(context.Background(), opened.PageID, target); err != nil {
		t.Fatal(err)
	}
	time.Sleep(130 * time.Millisecond)
	session.page.inputMu.Lock()
	inputs := append([]InputEvent(nil), session.page.inputs...)
	session.page.inputMu.Unlock()
	maximum := 1 + int(livePointerMaxInterpolation/livePointerStep)
	if len(inputs) < 3 || len(inputs) > maximum {
		t.Fatalf("本地插值点数=%d，不在 [3,%d] 内: %+v", len(inputs), maximum, inputs)
	}
	for index, input := range inputs {
		if input.Y != 100 || index > 0 && input.X <= inputs[index-1].X {
			t.Fatalf("插值点没有单调接近目标: %+v", inputs)
		}
		if index > 0 && (input.Button != "left" || input.Buttons != 1 ||
			input.PointerType != "pen" || input.Modifiers != 8) {
			t.Fatalf("插值点没有继承指针状态: %+v", input)
		}
	}
	if inputs[len(inputs)-1].X != 500 {
		t.Fatalf("本地插值没有精确到达终点: %+v", inputs)
	}
}

func TestManagerReplacesUnfinishedPointerTrajectory(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{
		URL: "https://example.com", Viewport: Viewport{Width: 800, Height: 600},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []InputEvent{
		{Kind: "mouse", Action: "move", X: 100, Y: 100},
		{Kind: "mouse", Action: "move", X: 500, Y: 100},
	} {
		if err := manager.DispatchLiveInput(context.Background(), opened.PageID, event); err != nil {
			t.Fatal(err)
		}
		if event.X == 100 {
			waitLiveInputs(t, session.page, 1)
			time.Sleep(64 * time.Millisecond)
		}
	}
	waitLiveInputs(t, session.page, 2)
	if err := manager.DispatchLiveInput(context.Background(), opened.PageID, InputEvent{Kind: "mouse", Action: "move", X: 260, Y: 100}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(120 * time.Millisecond)
	session.page.inputMu.Lock()
	inputs := append([]InputEvent(nil), session.page.inputs...)
	session.page.inputMu.Unlock()
	if inputs[len(inputs)-1].X != 260 {
		t.Fatalf("新目标未精确到达: %+v", inputs)
	}
	for _, event := range inputs {
		if event.X > 260 {
			t.Fatalf("新目标到达后仍补播旧轨迹: %+v", inputs)
		}
	}
}

func TestManagerFlushesPointerTargetBeforeDiscreteEvents(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{
		URL: "https://example.com", Viewport: Viewport{Width: 800, Height: 600},
	})
	if err != nil {
		t.Fatal(err)
	}
	events := []InputEvent{
		{Kind: "mouse", Action: "move", X: 100, Y: 100},
		{Kind: "mouse", Action: "move", X: 500, Y: 100},
		{Kind: "mouse", Action: "down", X: 500, Y: 100, Button: "left", Buttons: 1},
		{Kind: "mouse", Action: "move", X: 700, Y: 100, Button: "left", Buttons: 1},
		{Kind: "mouse", Action: "up", X: 700, Y: 100, Button: "left"},
	}
	if err := manager.DispatchLiveInput(context.Background(), opened.PageID, events[0]); err != nil {
		t.Fatal(err)
	}
	waitLiveInputs(t, session.page, 1)
	for _, event := range events[1:] {
		if err := manager.DispatchLiveInput(context.Background(), opened.PageID, event); err != nil {
			t.Fatal(err)
		}
	}
	inputs := waitLiveInputs(t, session.page, 5)
	if len(inputs) != 5 {
		t.Fatalf("离散屏障产生了额外旧轨迹: %+v", inputs)
	}
	for index, want := range events {
		if inputs[index].Action != want.Action || inputs[index].X != want.X || inputs[index].Y != want.Y {
			t.Fatalf("输入 %d 顺序错误: got=%+v want=%+v", index, inputs[index], want)
		}
	}
}

func TestManagerInputEpochCancelsPointerTrajectory(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{
		URL: "https://example.com", Viewport: Viewport{Width: 800, Height: 600},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.DispatchLiveInput(context.Background(), opened.PageID, InputEvent{Kind: "mouse", Action: "move", X: 100, Y: 100}); err != nil {
		t.Fatal(err)
	}
	waitLiveInputs(t, session.page, 1)
	time.Sleep(64 * time.Millisecond)
	if err := manager.DispatchLiveInput(context.Background(), opened.PageID, InputEvent{Kind: "mouse", Action: "move", X: 500, Y: 100}); err != nil {
		t.Fatal(err)
	}
	waitLiveInputs(t, session.page, 2)
	current := manager.sessions[opened.SessionID]
	current.mu.Lock()
	invalidateInputsLocked(current)
	current.mu.Unlock()
	time.Sleep(120 * time.Millisecond)
	session.page.inputMu.Lock()
	inputs := append([]InputEvent(nil), session.page.inputs...)
	session.page.inputMu.Unlock()
	if len(inputs) != 2 || inputs[len(inputs)-1].X >= 500 {
		t.Fatalf("epoch 变化后旧轨迹仍在投递: %+v", inputs)
	}
}

func TestManagerControlChangeCancelsPointerTrajectory(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{
		URL: "https://example.com", Viewport: Viewport{Width: 800, Height: 600},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, leave, err := manager.JoinViewer("viewer-a", false); err != nil {
		t.Fatal(err)
	} else {
		defer leave()
	}
	if !manager.CanControl("viewer-a") {
		if _, err := manager.TakeControl("viewer-a"); err != nil {
			t.Fatal(err)
		}
	}
	if _, leave, err := manager.JoinViewer("viewer-b", false); err != nil {
		t.Fatal(err)
	} else {
		defer leave()
	}
	if err := manager.DispatchViewerInput(context.Background(), "viewer-a", opened.PageID, InputEvent{Kind: "mouse", Action: "move", X: 100, Y: 100}); err != nil {
		t.Fatal(err)
	}
	waitLiveInputs(t, session.page, 1)
	time.Sleep(64 * time.Millisecond)
	if err := manager.DispatchViewerInput(context.Background(), "viewer-a", opened.PageID, InputEvent{Kind: "mouse", Action: "move", X: 500, Y: 100}); err != nil {
		t.Fatal(err)
	}
	waitLiveInputs(t, session.page, 2)
	if _, err := manager.TakeControl("viewer-b"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(120 * time.Millisecond)
	session.page.inputMu.Lock()
	inputs := append([]InputEvent(nil), session.page.inputs...)
	session.page.inputMu.Unlock()
	if len(inputs) != 2 || inputs[len(inputs)-1].X >= 500 {
		t.Fatalf("控制权变化后旧轨迹仍在投递: %+v", inputs)
	}
}

func TestManagerViewportChangeCancelsPointerTrajectory(t *testing.T) {
	base := &fakeSession{page: &fakePage{info: PageInfo{URL: "about:blank", Title: "blank"}}}
	browser := &fakeViewportSession{fakeSession: base}
	manager, err := newManager(Config{
		AllowPrivateNetwork: true,
		Factory:             func(context.Context, SessionConfig) (Session, error) { return browser, nil },
	}, adjustedTiming(func(value *timing) { value.tabReconcileTimeout = 20 * time.Millisecond }))
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{
		URL: "https://example.com", Viewport: Viewport{Width: 800, Height: 600},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.DispatchLiveInput(context.Background(), opened.PageID, InputEvent{Kind: "mouse", Action: "move", X: 100, Y: 100}); err != nil {
		t.Fatal(err)
	}
	waitLiveInputs(t, base.page, 1)
	time.Sleep(64 * time.Millisecond)
	if err := manager.DispatchLiveInput(context.Background(), opened.PageID, InputEvent{Kind: "mouse", Action: "move", X: 500, Y: 100}); err != nil {
		t.Fatal(err)
	}
	waitLiveInputs(t, base.page, 2)
	if err := manager.SetViewport(context.Background(), ViewportRequest{
		SessionID: opened.SessionID,
		PageID:    opened.PageID,
		Viewport:  Viewport{Width: 900, Height: 700},
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(120 * time.Millisecond)
	base.page.inputMu.Lock()
	inputs := append([]InputEvent(nil), base.page.inputs...)
	base.page.inputMu.Unlock()
	if len(inputs) != 2 || inputs[len(inputs)-1].X >= 500 {
		t.Fatalf("viewport 变化后旧轨迹仍在投递: %+v", inputs)
	}
}

func TestManagerPageChangeCancelsPointerTrajectory(t *testing.T) {
	manager, browser := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{
		URL: "https://example.com/a", Viewport: Viewport{Width: 800, Height: 600},
	})
	if err != nil {
		t.Fatal(err)
	}
	current := manager.sessions[opened.SessionID]
	secondPage := &fakePage{info: PageInfo{URL: "https://example.com/b"}}
	current.mu.Lock()
	second := manager.registerPage(current, secondPage, Viewport{Width: 800, Height: 600}, "")
	current.mu.Unlock()
	if err := manager.DispatchLiveInput(context.Background(), opened.PageID, InputEvent{Kind: "mouse", Action: "move", X: 100, Y: 100}); err != nil {
		t.Fatal(err)
	}
	waitLiveInputs(t, browser.page, 1)
	time.Sleep(64 * time.Millisecond)
	if err := manager.DispatchLiveInput(context.Background(), opened.PageID, InputEvent{Kind: "mouse", Action: "move", X: 500, Y: 100}); err != nil {
		t.Fatal(err)
	}
	waitLiveInputs(t, browser.page, 2)
	if _, err := manager.ActivateTab(context.Background(), TabRequest{SessionID: opened.SessionID, PageID: second.id}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(120 * time.Millisecond)
	browser.page.inputMu.Lock()
	inputs := append([]InputEvent(nil), browser.page.inputs...)
	browser.page.inputMu.Unlock()
	if len(inputs) != 2 || inputs[len(inputs)-1].X >= 500 {
		t.Fatalf("页面变化后旧轨迹仍在投递: %+v", inputs)
	}
}

func TestLivePointerStepsAreBounded(t *testing.T) {
	if got := livePointerSteps(time.Millisecond); got != 1 {
		t.Fatalf("超密采样步数=%d，期望=1", got)
	}
	for _, span := range []time.Duration{0, 4 * time.Millisecond, 16 * time.Millisecond, 24 * time.Millisecond, 101 * time.Millisecond} {
		if got := livePointerSteps(span); got != 0 {
			t.Fatalf("直接提交间隔 %s 的步数=%d，期望=0", span, got)
		}
	}
	if got := livePointerSteps(32 * time.Millisecond); got != 2 {
		t.Fatalf("32ms 插值步数=%d，期望=2", got)
	}
	maximum := int(livePointerMaxInterpolation / livePointerStep)
	if got := livePointerSteps(100 * time.Millisecond); got != maximum {
		t.Fatalf("100ms 插值步数=%d，期望上限=%d", got, maximum)
	}
}

func TestCoalesceLiveInputsKeepsLatestPointerMove(t *testing.T) {
	page := &page{id: "page-a"}
	queue := make(chan liveInput, 3)
	queue <- liveInput{page: page, epoch: 1, event: InputEvent{Kind: "mouse", Action: "move", X: 20, Y: 30}}
	queue <- liveInput{page: page, epoch: 1, event: InputEvent{Kind: "mouse", Action: "move", X: 40, Y: 50}}
	queue <- liveInput{page: page, epoch: 1, event: InputEvent{Kind: "mouse", Action: "up", X: 40, Y: 50}}

	result, pending := coalesceLiveInputs(liveInput{page: page, epoch: 1, event: InputEvent{Kind: "mouse", Action: "move", X: 10, Y: 15}}, queue)
	if result.event.X != 40 || result.event.Y != 50 {
		t.Fatalf("移动没有收敛到最新坐标: %+v", result.event)
	}
	if pending == nil || pending.event.Action != "up" {
		t.Fatalf("离散事件没有保持在移动之后: %+v", pending)
	}
}

func TestCoalesceLiveInputsDoesNotConsumeTouchMove(t *testing.T) {
	page := &page{id: "page-a"}
	queue := make(chan liveInput, 3)
	queue <- liveInput{page: page, epoch: 1, event: InputEvent{Kind: "touch", Action: "move", Touches: []InputTouchPoint{{ID: 7, X: 20, Y: 30}}}}
	queue <- liveInput{page: page, epoch: 1, event: InputEvent{Kind: "touch", Action: "move", Touches: []InputTouchPoint{{ID: 7, X: 40, Y: 50}}}}
	queue <- liveInput{page: page, epoch: 1, event: InputEvent{Kind: "touch", Action: "end"}}

	result, pending := coalesceLiveInputs(liveInput{page: page, epoch: 1, event: InputEvent{Kind: "touch", Action: "move", Touches: []InputTouchPoint{{ID: 7, X: 10, Y: 15}}}}, queue)
	if len(result.event.Touches) != 1 || result.event.Touches[0].X != 10 || result.event.Touches[0].Y != 15 {
		t.Fatalf("首个触摸移动被改写: %+v", result.event)
	}
	if pending != nil || len(queue) != 3 {
		t.Fatalf("触摸移动不应由 mouse move 合并器预取: pending=%+v len=%d", pending, len(queue))
	}
}

func TestCoalesceLiveInputsDoesNotConsumeTrackpadWheel(t *testing.T) {
	page := &page{id: "page-a"}
	queue := make(chan liveInput, 3)
	queue <- liveInput{page: page, epoch: 1, event: InputEvent{Kind: "mouse", Action: "wheel", X: 11, Y: 21, DeltaX: 2, DeltaY: 4}}
	queue <- liveInput{page: page, epoch: 1, event: InputEvent{Kind: "mouse", Action: "wheel", X: 12, Y: 22, DeltaX: -1, DeltaY: 8}}
	queue <- liveInput{page: page, epoch: 1, event: InputEvent{Kind: "key", Action: "down", Key: "Escape"}}

	result, pending := coalesceLiveInputs(liveInput{page: page, epoch: 1, event: InputEvent{Kind: "mouse", Action: "wheel", X: 10, Y: 20, DeltaX: 3, DeltaY: 5}}, queue)
	if result.event.DeltaX != 3 || result.event.DeltaY != 5 || result.event.X != 10 || result.event.Y != 20 {
		t.Fatalf("首个滚轮输入被改写: %+v", result.event)
	}
	if pending != nil || len(queue) != 3 {
		t.Fatalf("滚轮不应由 mouse move 合并器预取: pending=%+v len=%d", pending, len(queue))
	}
}

func TestCoalesceLiveInputsDoesNotCrossPageEpoch(t *testing.T) {
	page := &page{id: "page-a"}
	queue := make(chan liveInput, 1)
	queue <- liveInput{page: page, epoch: 2, event: InputEvent{Kind: "mouse", Action: "move", X: 90, Y: 90}}

	result, pending := coalesceLiveInputs(liveInput{page: page, epoch: 1, event: InputEvent{Kind: "mouse", Action: "move", X: 10, Y: 10}}, queue)
	if result.epoch != 1 || pending == nil || pending.epoch != 2 {
		t.Fatalf("合并跨越了输入 epoch: result=%+v pending=%+v", result, pending)
	}
}

func TestCoalesceLiveInputsDoesNotMergeLosslessSamples(t *testing.T) {
	page := &page{id: "page-a"}
	queue := make(chan liveInput, 1)
	queue <- liveInput{page: page, epoch: 1, lossless: true, event: InputEvent{Kind: "mouse", Action: "move", X: 20, Y: 30}}

	result, pending := coalesceLiveInputs(
		liveInput{page: page, epoch: 1, lossless: true, event: InputEvent{Kind: "mouse", Action: "move", X: 10, Y: 15}},
		queue,
	)
	if result.event.X != 10 {
		t.Fatalf("第一批采样被合并了: %+v", result.event)
	}
	if pending != nil {
		t.Fatalf("批量采样不应预取并合并: %+v", pending)
	}
	next := <-queue
	if next.event.X != 20 || !next.lossless {
		t.Fatalf("后续采样没有保持独立: %+v", next)
	}
}

func TestInputReplayOffsetsSpreadBatch(t *testing.T) {
	offsets := inputReplayOffsets(30*time.Millisecond, 3)
	want := []time.Duration{0, 10 * time.Millisecond, 20 * time.Millisecond}
	if len(offsets) != len(want) {
		t.Fatalf("偏移数量=%d", len(offsets))
	}
	for index := range want {
		if offsets[index] != want[index] {
			t.Fatalf("偏移=%v，期望=%v", offsets, want)
		}
	}
	capped := inputReplayOffsets(time.Second, 2)
	if len(capped) != 2 || capped[0] != 0 || capped[1] != 50*time.Millisecond {
		t.Fatalf("回放跨度没有压缩上限: %v", capped)
	}
	if single := inputReplayOffsets(time.Second, 1); len(single) != 1 || single[0] != 0 {
		t.Fatalf("单点不应延迟: %v", single)
	}
}

func TestManagerReplaysEveryBatchSampleInOrder(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{
		URL: "https://example.com", Viewport: Viewport{Width: 800, Height: 600},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, leave, err := manager.JoinViewer("viewer", false); err != nil {
		t.Fatal(err)
	} else {
		defer leave()
	}
	if !manager.CanControl("viewer") {
		if _, err := manager.TakeControl("viewer"); err != nil {
			t.Fatal(err)
		}
	}
	events := make([]InputEvent, 12)
	for index := range events {
		events[index] = InputEvent{Kind: "mouse", Action: "move", X: float64(index + 1), Y: float64(index + 2)}
	}
	if err := manager.DispatchViewerInputs(context.Background(), "viewer", opened.PageID, events, 0); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		session.page.inputMu.Lock()
		inputs := append([]InputEvent(nil), session.page.inputs...)
		session.page.inputMu.Unlock()
		if len(inputs) >= len(events) {
			if len(inputs) != len(events) {
				t.Fatalf("批量采样多出事件: %+v", inputs)
			}
			for index, event := range events {
				if inputs[index].Kind != event.Kind || inputs[index].Action != event.Action ||
					inputs[index].X != event.X || inputs[index].Y != event.Y {
					t.Fatalf("采样 %d 顺序或内容改变: got=%+v want=%+v", index, inputs[index], event)
				}
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待批量采样投递超时: %+v", inputs)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestManagerPacesBatchReplayFromDispatcherAnchor(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{
		URL: "https://example.com", Viewport: Viewport{Width: 800, Height: 600},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, leave, err := manager.JoinViewer("viewer", false); err != nil {
		t.Fatal(err)
	} else {
		defer leave()
	}
	if !manager.CanControl("viewer") {
		if _, err := manager.TakeControl("viewer"); err != nil {
			t.Fatal(err)
		}
	}
	events := []InputEvent{
		{Kind: "mouse", Action: "move", X: 1, Y: 1},
		{Kind: "mouse", Action: "move", X: 2, Y: 2},
	}
	if err := manager.DispatchViewerInputs(context.Background(), "viewer", opened.PageID, events, 60*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	waitCount := func(want int) time.Time {
		deadline := time.Now().Add(time.Second)
		for {
			session.page.inputMu.Lock()
			count := len(session.page.inputs)
			session.page.inputMu.Unlock()
			if count >= want {
				return time.Now()
			}
			if time.Now().After(deadline) {
				t.Fatalf("等待第 %d 个采样投递超时", want)
			}
			time.Sleep(time.Millisecond)
		}
	}
	first := waitCount(1)
	second := waitCount(2)
	if elapsed := second.Sub(first); elapsed < 15*time.Millisecond {
		t.Fatalf("批量回放没有铺开，两个采样间隔 %s", elapsed)
	}
}

func TestManagerRejectsDiscreteInputWithoutBlockingWhenQueueIsFull(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{
		URL: "https://example.com", Viewport: Viewport{Width: 800, Height: 600},
	})
	if err != nil {
		t.Fatal(err)
	}
	blocked := make(chan struct{})
	defer close(blocked)
	session.page.inputBlock = blocked
	if err := manager.DispatchLiveInput(context.Background(), opened.PageID, InputEvent{Kind: "text", Text: "active"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		current := manager.sessions[opened.SessionID]
		current.mu.Lock()
		started := current.inputCancel != nil
		current.mu.Unlock()
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("首条离散输入没有开始投递")
		}
		time.Sleep(time.Millisecond)
	}
	for range liveInputQueueSize {
		if err := manager.DispatchLiveInput(context.Background(), opened.PageID, InputEvent{Kind: "text", Text: "queued"}); err != nil {
			t.Fatal(err)
		}
	}

	started := time.Now()
	err = manager.DispatchLiveInput(context.Background(), opened.PageID, InputEvent{Kind: "shortcut", Action: "copy", Modifiers: 4})
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("满队列使离散输入阻塞了 %s", elapsed)
	}
	var overloaded *Error
	if !errors.As(err, &overloaded) || overloaded.Kind != "input_overloaded" {
		t.Fatalf("满队列错误=%v", err)
	}
}

func TestManagerWaitViewerInputWaitsForApplication(t *testing.T) {
	manager, session := fakeManager(t, Config{AllowPrivateNetwork: true})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{
		URL: "https://example.com", Viewport: Viewport{Width: 800, Height: 600},
	})
	if err != nil {
		t.Fatal(err)
	}
	blocked := make(chan struct{})
	session.page.inputBlock = blocked
	if err := manager.DispatchLiveInput(context.Background(), opened.PageID, InputEvent{Kind: "mouse", Action: "move", X: 10, Y: 10}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- manager.WaitViewerInput(context.Background(), opened.PageID) }()
	select {
	case err := <-done:
		t.Fatalf("输入尚未应用时屏障提前返回: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(blocked)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("输入应用后屏障仍未返回")
	}
}
