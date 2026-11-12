package browserkit

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"strings"
	"testing"
)

func point(value float64) *float64 { return &value }

func testPNG(t *testing.T) []byte {
	t.Helper()
	return testPNGSize(t, 4, 3)
}

func testPNGSize(t *testing.T, width, height int) []byte {
	t.Helper()
	var buffer bytes.Buffer
	imageValue := image.NewRGBA(image.Rect(0, 0, width, height))
	if err := png.Encode(&buffer, imageValue); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestSnapshotCanIncludeScreenshotAndViewport(t *testing.T) {
	manager, session := fakeManager(t, Config{})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com", Viewport: Viewport{Width: 800, Height: 600}})
	if err != nil {
		t.Fatal(err)
	}
	session.page.screenshotData = testPNG(t)
	response, err := manager.Snapshot(context.Background(), SnapshotRequest{
		SessionID: opened.SessionID, PageID: opened.PageID, IncludeScreenshot: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Screenshot == nil {
		t.Fatal("快照没有附带截图")
	}
	if response.Screenshot.Width != 4 || response.Screenshot.Height != 3 {
		t.Fatalf("截图像素尺寸=%dx%d", response.Screenshot.Width, response.Screenshot.Height)
	}
	if response.Screenshot.ViewportWidth != 800 || response.Screenshot.ViewportHeight != 600 {
		t.Fatalf("截图视口=%dx%d", response.Screenshot.ViewportWidth, response.Screenshot.ViewportHeight)
	}
}

func TestClickByPointDoesNotRequireRevision(t *testing.T) {
	manager, session := fakeManager(t, Config{})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	response, err := manager.Click(context.Background(), ClickRequest{
		SessionID: opened.SessionID, PageID: opened.PageID, X: point(120), Y: point(80), Button: "right", ClickCount: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !response.Changed || !session.page.clickPointCalled || session.page.clickPointX != 120 || session.page.clickPointY != 80 {
		t.Fatalf("坐标点击没有到达页面: %+v", response)
	}
	if session.page.clickOptions.Button != "right" || session.page.clickOptions.ClickCount != 2 {
		t.Fatalf("坐标点击参数丢失: %+v", session.page.clickOptions)
	}
}

func TestDragByDeltaAndScreenshot(t *testing.T) {
	manager, session := fakeManager(t, Config{})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	session.page.screenshotData = testPNG(t)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(context.Background(), SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	response, err := manager.Drag(context.Background(), DragRequest{
		SessionID: opened.SessionID, PageID: opened.PageID, SourceRef: "e1", Revision: snapshot.Revision,
		DeltaX: 140, DeltaY: -6, IncludeScreenshot: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !session.page.dragByCalled || session.page.dragByDeltaX != 140 || session.page.dragByDeltaY != -6 {
		t.Fatalf("位移拖拽没有到达页面: %+v", session.page)
	}
	if response.Screenshot == nil {
		t.Fatal("位移拖拽没有附带截图")
	}
}

func TestDragByPointDoesNotRequireSnapshot(t *testing.T) {
	manager, session := fakeManager(t, Config{})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.Drag(context.Background(), DragRequest{
		SessionID: opened.SessionID, PageID: opened.PageID, FromX: point(50), FromY: point(70), DeltaX: 160,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !session.page.dragPointCalled || session.page.dragPointX != 50 || session.page.dragPointY != 70 || session.page.dragByDeltaX != 160 {
		t.Fatalf("坐标拖拽没有到达页面: %+v", session.page)
	}
}

func TestDragRejectsAmbiguousOrEmptyTargets(t *testing.T) {
	manager, _ := fakeManager(t, Config{})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	base := DragRequest{SessionID: opened.SessionID, PageID: opened.PageID, SourceRef: "e1", Revision: 1}
	type testCase struct {
		request DragRequest
		want    string
	}
	cases := []testCase{}
	bothTargets := base
	bothTargets.TargetRef, bothTargets.DeltaX = "e2", 10
	cases = append(cases, testCase{request: bothTargets, want: "target_ref 与 delta_x/delta_y 只能二选一"})
	bothSources := base
	bothSources.FromX, bothSources.FromY = point(1), point(2)
	bothSources.TargetRef = "e2"
	cases = append(cases, testCase{request: bothSources, want: "source_ref 与 from_x/from_y 只能二选一"})
	zeroDelta := base
	cases = append(cases, testCase{request: zeroDelta, want: "需要 target_ref 或非零 delta_x/delta_y"})
	for _, test := range cases {
		_, err := manager.Drag(context.Background(), test.request)
		if err == nil {
			t.Fatalf("无效拖拽没有被拒绝: %+v", test.request)
		}
		if !strings.Contains(err.Error(), test.want) {
			t.Fatalf("拖拽错误信息不够明确: %v，期望包含 %q", err, test.want)
		}
	}
}

func TestActSupportsPointClickAndDeltaDrag(t *testing.T) {
	manager, session := fakeManager(t, Config{})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	response, err := manager.Act(context.Background(), ActRequest{
		SessionID: opened.SessionID, PageID: opened.PageID, Revision: 0,
		Steps: []ActStep{
			{Op: "click", X: point(90), Y: point(110)},
			{Op: "drag", X: point(50), Y: point(60), DeltaX: 120, DeltaY: 3},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.FailedStep != 0 || response.Completed != 2 {
		t.Fatalf("批量动作失败: %+v", response)
	}
	if !session.page.clickPointCalled || !session.page.dragPointCalled {
		t.Fatalf("批量动作没有到达页面: %+v", session.page)
	}
}

func TestActRequiresRevisionForReferenceSteps(t *testing.T) {
	manager, _ := fakeManager(t, Config{})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Act(context.Background(), ActRequest{
		SessionID: opened.SessionID, PageID: opened.PageID,
		Steps: []ActStep{{Op: "click", Ref: "e1"}},
	}); err == nil {
		t.Fatal("引用步骤没有 revision 却被接受")
	}
}

func TestActRejectsCoordinateSourceWithElementTargetBeforeSideEffects(t *testing.T) {
	manager, session := fakeManager(t, Config{})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	response, err := manager.Act(context.Background(), ActRequest{
		SessionID: opened.SessionID, PageID: opened.PageID,
		Steps: []ActStep{
			{Op: "click", X: point(10), Y: point(10)},
			{Op: "drag", X: point(20), Y: point(20), TargetRef: "e1"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.FailedStep != 2 || session.page.clickPointCalled {
		t.Fatalf("非法拖拽没有在预检阶段阻止副作用: %+v", response)
	}
}

func TestScreenshotRejectsFullPageGrid(t *testing.T) {
	manager, _ := fakeManager(t, Config{})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Screenshot(context.Background(), ScreenshotRequest{
		SessionID: opened.SessionID, PageID: opened.PageID, FullPage: true, Grid: true,
	}); err == nil {
		t.Fatal("full_page + grid 没有被拒绝")
	}
}

func TestSnapshotGridChangesScreenshot(t *testing.T) {
	manager, session := fakeManager(t, Config{})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com", Viewport: Viewport{Width: 800, Height: 600}})
	if err != nil {
		t.Fatal(err)
	}
	session.page.screenshotData = testPNGSize(t, 800, 600)
	response, err := manager.Snapshot(context.Background(), SnapshotRequest{
		SessionID: opened.SessionID, PageID: opened.PageID, IncludeScreenshot: true, ScreenshotGrid: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Screenshot == nil || bytes.Equal(response.Screenshot.Data, session.page.screenshotData) {
		t.Fatal("screenshot_grid 没有改变截图内容")
	}
}

func TestCoordinateActionsRejectPointsOutsideViewport(t *testing.T) {
	manager, _ := fakeManager(t, Config{})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com", Viewport: Viewport{Width: 800, Height: 600}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Click(context.Background(), ClickRequest{SessionID: opened.SessionID, PageID: opened.PageID, X: point(800), Y: point(10)}); err == nil {
		t.Fatal("越界点击没有被拒绝")
	}
	if _, err := manager.Drag(context.Background(), DragRequest{SessionID: opened.SessionID, PageID: opened.PageID, FromX: point(700), FromY: point(500), DeltaX: 200}); err == nil {
		t.Fatal("越界拖拽没有被拒绝")
	}
}

func TestActRejectsOutOfBoundsCoordinateBeforeSideEffects(t *testing.T) {
	manager, session := fakeManager(t, Config{})
	defer manager.Close(context.Background())
	opened, err := manager.Open(context.Background(), OpenRequest{URL: "https://example.com", Viewport: Viewport{Width: 800, Height: 600}})
	if err != nil {
		t.Fatal(err)
	}
	response, err := manager.Act(context.Background(), ActRequest{
		SessionID: opened.SessionID, PageID: opened.PageID,
		Steps: []ActStep{
			{Op: "click", X: point(10), Y: point(10)},
			{Op: "drag", X: point(700), Y: point(500), DeltaX: 200},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.FailedStep != 2 || session.page.clickPointCalled {
		t.Fatalf("越界坐标没有在预检阶段阻止副作用: %+v", response)
	}
}
