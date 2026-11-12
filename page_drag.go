package browserkit

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

func (page *rodPage) Drag(ctx context.Context, source, target ElementRef) (resultErr error) {
	operationCtx, cancel, err := page.ctx(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	if err := lockInteraction(operationCtx, &page.interactionMu); err != nil {
		return err
	}
	defer page.interactionMu.Unlock()
	if isFrameRef(source.Ref) || isFrameRef(target.Ref) {
		start, startErr := page.pointForRef(operationCtx, source)
		if startErr != nil {
			return startErr
		}
		end, endErr := page.pointForRef(operationCtx, target)
		if endErr != nil {
			return endErr
		}
		return page.dragPoints(operationCtx, start, end)
	}
	from, err := page.elementLocked(operationCtx, source)
	if err != nil {
		return err
	}
	to, err := page.elementLocked(operationCtx, target)
	if err != nil {
		return err
	}
	return page.dragElements(operationCtx, from, to)
}

// DragTo 允许放置目标使用 CSS selector，用于不在快照中的投放区域。
func (page *rodPage) DragTo(ctx context.Context, source ElementRef, selector string) (resultErr error) {
	operationCtx, cancel, err := page.ctx(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	if err := lockInteraction(operationCtx, &page.interactionMu); err != nil {
		return err
	}
	defer page.interactionMu.Unlock()
	if isFrameRef(source.Ref) {
		start, startErr := page.pointForRef(operationCtx, source)
		if startErr != nil {
			return startErr
		}
		to, selectorErr := page.resolveDragTarget(operationCtx, selector)
		if selectorErr != nil {
			return selectorErr
		}
		if err := scrollElementNaturally(operationCtx, to); err != nil {
			return err
		}
		end, pointErr := page.elementPoint(operationCtx, to)
		if pointErr != nil {
			return pointErr
		}
		return page.dragPoints(operationCtx, start, end)
	}
	from, err := page.elementLocked(operationCtx, source)
	if err != nil {
		return err
	}
	to, err := page.resolveDragTarget(operationCtx, selector)
	if err != nil {
		return err
	}
	return page.dragElements(operationCtx, from, to)
}

// DragBy 从快照元素中心按视口 CSS 位移执行指针拖拽。
func (page *rodPage) DragBy(ctx context.Context, source ElementRef, dx, dy float64) error {
	if err := validateDragDelta(dx, dy); err != nil {
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
	start, err := page.pointForRef(operationCtx, source)
	if err != nil {
		return err
	}
	end := elementPointResult{X: start.X + dx, Y: start.Y + dy, Visible: true, Enabled: true}
	return page.dragPointsHumanized(operationCtx, start, end)
}

// DragPoint 从视口 CSS 坐标按位移执行指针拖拽，用于没有快照 ref 的视觉目标。
func (page *rodPage) DragPoint(ctx context.Context, x, y, dx, dy float64) error {
	if err := validateDragDelta(dx, dy); err != nil {
		return err
	}
	if err := validateInputPoint(x, y); err != nil {
		return err
	}
	start := elementPointResult{X: x, Y: y, Visible: true, Enabled: true}
	end := elementPointResult{X: x + dx, Y: y + dy, Visible: true, Enabled: true}
	if err := validateInputPoint(end.X, end.Y); err != nil {
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
	return page.dragPointsHumanized(operationCtx, start, end)
}

func validateDragDelta(dx, dy float64) error {
	if math.IsNaN(dx) || math.IsNaN(dy) || math.IsInf(dx, 0) || math.IsInf(dy, 0) || math.Abs(dx) > 10000 || math.Abs(dy) > 10000 {
		return errors.New("拖拽位移无效")
	}
	if dx == 0 && dy == 0 {
		return errors.New("拖拽位移不能为零")
	}
	return nil
}

const dragSelectorRootsEval = `(selector) => {
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
	let count = 0;
	for (const root of roots) count += root.querySelectorAll(selector).length;
	return count;
}`

const dragSelectorUniqueEval = `(selector) => {
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
	let found = null;
	for (const root of roots) {
		for (const el of root.querySelectorAll(selector)) {
			if (found) return null;
			found = el;
		}
	}
	return found;
}`

func (page *rodPage) resolveDragTarget(ctx context.Context, selector string) (*rod.Element, error) {
	object, err := page.page.Context(ctx).Evaluate(rod.Eval(dragSelectorUniqueEval, selector).ByObject())
	if err != nil {
		return nil, dragSelectorError(ctx, err, selector)
	}
	if object.ObjectID == "" {
		countResult, countErr := page.page.Context(ctx).Eval(dragSelectorRootsEval, selector)
		if countErr != nil {
			return nil, dragSelectorError(ctx, countErr, selector)
		}
		var count int
		if err := countResult.Value.Unmarshal(&count); err != nil {
			return nil, err
		}
		if count == 0 {
			return nil, newError("target_not_found", "拖放目标 selector 未匹配元素: "+selector, "确认 selector 或先重新 browserSnapshot 获取 ref", true)
		}
		return nil, newError("ambiguous_target", "拖放目标 selector 匹配了多个元素: "+selector, "使用更精确的 selector 或快照 ref", true)
	}
	element, err := page.page.ElementFromObject(object)
	if err != nil {
		return nil, dragSelectorError(ctx, err, selector)
	}
	return element, nil
}

func dragSelectorError(ctx context.Context, err error, selector string) error {
	if ctxErr := contextErr(ctx); ctxErr != nil {
		return ctxErr
	}
	if strings.Contains(err.Error(), "SyntaxError") {
		return newError("invalid_selector", "拖放目标 selector 无效: "+selector, "检查 CSS selector 语法", true)
	}
	return newError("target_unavailable", "读取拖放目标失败: "+err.Error(), "确认页面仍然打开后重试", true)
}

func (page *rodPage) dragElements(operationCtx context.Context, from, to *rod.Element) (resultErr error) {
	if err := scrollElementNaturally(operationCtx, from); err != nil {
		return err
	}
	if err := scrollElementNaturally(operationCtx, to); err != nil {
		return err
	}
	start, err := page.elementPoint(operationCtx, from)
	if err != nil {
		return err
	}
	end, err := page.elementPoint(operationCtx, to)
	if err != nil {
		return err
	}
	native, err := from.Evaluate(rod.Eval(`function() { return this.draggable === true; }`).ByUser())
	if err != nil {
		return err
	}
	client := page.page.Context(operationCtx)
	isNative := native.Value.Bool()
	var intercepted <-chan *proto.InputDragData
	if isNative {
		eventCtx, stop := context.WithCancel(operationCtx)
		data := make(chan *proto.InputDragData, 1)
		intercepted = data
		wait := page.page.Context(eventCtx).EachEvent(func(event *proto.InputDragIntercepted) bool {
			data <- event.Data
			return true
		})
		done := make(chan struct{})
		go func() { defer close(done); wait() }()
		defer func() { stop(); <-done }()
		defer func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), inputCleanupTimeout)
			defer cleanupCancel()
			if resultErr != nil {
				resultErr = errors.Join(resultErr, callInput(cleanupCtx, page.page, proto.InputCancelDragging{}))
			}
			resultErr = errors.Join(resultErr, callInput(cleanupCtx, page.page, proto.InputSetInterceptDrags{Enabled: false}))
		}()
		if err := (proto.InputSetInterceptDrags{Enabled: true}).Call(client); err != nil {
			return err
		}
	}
	if err := page.movePointer(client, start.X, start.Y); err != nil {
		return err
	}
	if err := dispatchDrag(operationCtx, page.page, start, end, intercepted); err != nil {
		return err
	}
	page.pointerMu.Lock()
	page.pointerX = end.X
	page.pointerY = end.Y
	page.pointerKnown = true
	page.pointerMu.Unlock()
	return nil
}

func dispatchDrag(ctx context.Context, client proto.Client, start, end elementPointResult, intercepted <-chan *proto.InputDragData) error {
	return dispatchDragPath(ctx, client, start, end, linearDragPath(start, end, 12), intercepted, 0)
}

// dispatchDragPath 执行按下、移动、释放；path 不含起点本身。
func dispatchDragPath(ctx context.Context, client proto.Client, start, end elementPointResult, path []elementPointResult, intercepted <-chan *proto.InputDragData, moveDelay time.Duration) (resultErr error) {
	pressed, released := 1, 0
	lastX, lastY := start.X, start.Y
	needsRelease := true
	defer func() {
		if !needsRelease {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), inputCleanupTimeout)
		defer cleanupCancel()
		up := proto.InputDispatchMouseEvent{Type: proto.InputDispatchMouseEventTypeMouseReleased, X: lastX, Y: lastY, Button: proto.InputMouseButtonLeft, Buttons: &released, ClickCount: 1}
		resultErr = errors.Join(resultErr, callInput(cleanupCtx, client, up))
	}()
	down := proto.InputDispatchMouseEvent{Type: proto.InputDispatchMouseEventTypeMousePressed, X: start.X, Y: start.Y, Button: proto.InputMouseButtonLeft, Buttons: &pressed, ClickCount: 1}
	if err := callInput(ctx, client, down); err != nil {
		return err
	}
	// 分段移动跨过浏览器的拖拽阈值，保持左键按下，兼容自定义指针拖动。
	for _, point := range path {
		lastX, lastY = point.X, point.Y
		move := proto.InputDispatchMouseEvent{Type: proto.InputDispatchMouseEventTypeMouseMoved, X: lastX, Y: lastY, Button: proto.InputMouseButtonLeft, Buttons: &pressed}
		if err := callInput(ctx, client, move); err != nil {
			return err
		}
		if moveDelay > 0 {
			if err := waitContext(ctx, moveDelay); err != nil {
				return err
			}
		}
	}
	if intercepted != nil {
		var data *proto.InputDragData
		select {
		case data = <-intercepted:
		case <-ctx.Done():
			return ctx.Err()
		}
		for _, kind := range []proto.InputDispatchDragEventType{proto.InputDispatchDragEventTypeDragEnter, proto.InputDispatchDragEventTypeDragOver, proto.InputDispatchDragEventTypeDrop} {
			if err := callInput(ctx, client, proto.InputDispatchDragEvent{Type: kind, X: end.X, Y: end.Y, Data: data}); err != nil {
				return err
			}
		}
	}
	up := proto.InputDispatchMouseEvent{Type: proto.InputDispatchMouseEventTypeMouseReleased, X: end.X, Y: end.Y, Button: proto.InputMouseButtonLeft, Buttons: &released, ClickCount: 1}
	if err := callInput(ctx, client, up); err != nil {
		return err
	}
	needsRelease = false
	return nil
}

// pointForRef 返回 ref 在主视口中的交互点，支持主文档与跨域 frame。
func (page *rodPage) pointForRef(ctx context.Context, ref ElementRef) (elementPointResult, error) {
	if binding, ok := page.frameBindingForRef(ctx, ref.Ref); ok {
		if err := page.validateFrameRef(ctx, ref); err != nil {
			return elementPointResult{}, err
		}
		x, y, err := page.frameElementPoint(ctx, binding, ref.Ref)
		if err != nil {
			return elementPointResult{}, err
		}
		return elementPointResult{X: x, Y: y, Visible: true, Enabled: true}, nil
	}
	element, err := page.elementLocked(ctx, ref)
	if err != nil {
		return elementPointResult{}, err
	}
	if err := scrollElementNaturally(ctx, element); err != nil {
		return elementPointResult{}, err
	}
	return page.elementPoint(ctx, element)
}

func linearDragPath(start, end elementPointResult, steps int) []elementPointResult {
	path := make([]elementPointResult, 0, steps)
	for index := 1; index <= steps; index++ {
		fraction := float64(index) / float64(steps)
		path = append(path, elementPointResult{X: start.X + (end.X-start.X)*fraction, Y: start.Y + (end.Y-start.Y)*fraction})
	}
	return path
}

// humanizedDragPath 使用平滑加减速和小幅纵向抖动，避免完全等速、等间距的机械轨迹。
func humanizedDragPath(start, end elementPointResult, steps int) []elementPointResult {
	path := make([]elementPointResult, 0, steps)
	for index := 1; index <= steps; index++ {
		fraction := float64(index) / float64(steps)
		eased := fraction * fraction * (3 - 2*fraction)
		x := start.X + (end.X-start.X)*eased
		y := start.Y + (end.Y-start.Y)*eased
		if index < steps {
			y += math.Sin(float64(index)*1.7) * 0.6 * (1 - eased)
		}
		path = append(path, elementPointResult{X: x, Y: y})
	}
	return path
}

func (page *rodPage) dragPointsHumanized(ctx context.Context, start, end elementPointResult) error {
	if err := validateInputPoint(start.X, start.Y); err != nil {
		return err
	}
	if err := validateInputPoint(end.X, end.Y); err != nil {
		return err
	}
	if err := page.movePointer(page.page.Context(ctx), start.X, start.Y); err != nil {
		return err
	}
	path := humanizedDragPath(start, end, 24)
	if err := dispatchDragPath(ctx, page.page, start, end, path, nil, 6*time.Millisecond); err != nil {
		return err
	}
	page.pointerMu.Lock()
	page.pointerX = end.X
	page.pointerY = end.Y
	page.pointerKnown = true
	page.pointerMu.Unlock()
	return nil
}

// dragPoints 在已换算的主视口坐标之间执行指针拖拽。
func (page *rodPage) dragPoints(ctx context.Context, start, end elementPointResult) error {
	if err := page.movePointer(page.page.Context(ctx), start.X, start.Y); err != nil {
		return err
	}
	if err := dispatchDrag(ctx, page.page, start, end, nil); err != nil {
		return err
	}
	page.pointerMu.Lock()
	page.pointerX = end.X
	page.pointerY = end.Y
	page.pointerKnown = true
	page.pointerMu.Unlock()
	return nil
}
