package browserkit

import (
	"context"
	"sync"
	"time"

	"github.com/go-rod/rod/lib/proto"
)

const screencastAckTimeout = 250 * time.Millisecond
const screencastStopTimeout = 500 * time.Millisecond

func (page *rodPage) Screenshot(ctx context.Context, options ScreenshotOptions) (Screenshot, error) {
	if err := contextErr(ctx); err != nil {
		return Screenshot{}, err
	}
	operationCtx, operationCancel, err := page.ctx(ctx)
	if err != nil {
		return Screenshot{}, err
	}
	defer operationCancel()
	// Chrome 新版的 headless target 在刚创建或导航后可能还不是 active page；
	// 先显式激活 target，避免 Page.captureScreenshot 返回 attach 错误。
	if err := page.activate(operationCtx); err != nil {
		return Screenshot{}, err
	}
	data, err := page.page.Context(operationCtx).Screenshot(options.FullPage, &proto.PageCaptureScreenshot{Format: proto.PageCaptureScreenshotFormatPng})
	if err != nil {
		return Screenshot{}, err
	}
	return Screenshot{Data: data, Format: "png"}, nil
}

func (page *rodPage) StartScreencast(ctx context.Context, options ScreencastOptions) (<-chan Screenshot, func(), error) {
	if err := contextErr(ctx); err != nil {
		return nil, nil, err
	}
	// 连续画面的订阅本身是由调用方 stop/ctx 结束的长生命周期操作，不能
	// 套用单次 30 秒默认时限；启动、激活、停止和每一帧 ack 仍分别通过
	// page.ctx 获得有界 context。
	streamCtx, cancel := context.WithCancel(ctx)
	frames := make(chan Screenshot, 1)
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			// 先解除本地事件订阅，再有界提交远端 stop。同一 Page 可能因
			// viewport 变化或 WebSocket 重连立即启动下一条 screencast；stop
			// 不能放到后台，否则旧 stop 可能晚于新 start 到达 Chrome，并把
			// 新流静默停掉。这里最多占用 live goroutine 500ms，不持有页面交互锁。
			cancel()
			stopCtx, stopCancel, err := page.controlCtx(context.Background(), screencastStopTimeout)
			if err != nil {
				return
			}
			defer stopCancel()
			_ = (proto.PageStopScreencast{}).Call(page.page.Context(stopCtx))
		})
	}
	quality := options.Quality
	if quality <= 0 || quality > 100 {
		quality = 100
	}
	// Rod 当前未提供 screencast 启停和帧确认的高层 API。
	start := proto.PageStartScreencast{Format: proto.PageStartScreencastFormatJpeg, Quality: &quality}
	if options.MaxWidth > 0 {
		start.MaxWidth = &options.MaxWidth
	}
	if options.MaxHeight > 0 {
		start.MaxHeight = &options.MaxHeight
	}
	if options.EveryNthFrame > 0 {
		start.EveryNthFrame = &options.EveryNthFrame
	}
	streamPage := page.page.Context(streamCtx)
	var callbackMu sync.Mutex
	wait := streamPage.EachEvent(func(event *proto.PageScreencastFrame) {
		callbackMu.Lock()
		defer callbackMu.Unlock()
		frame := Screenshot{Data: event.Data, Format: "jpeg"}
		// 只保留最新一帧，避免 CDP 生产速度超过消费速度时积压旧画面。
		select {
		case <-frames:
		default:
		}
		select {
		case frames <- frame:
		default:
		}
		ackCtx, ackCancel, err := page.controlCtx(streamCtx, screencastAckTimeout)
		if err == nil {
			_ = (proto.PageScreencastFrameAck{SessionID: event.SessionID}).Call(page.page.Context(ackCtx))
			ackCancel()
		}
	})
	setupCtx, setupCancel, err := page.ctx(streamCtx)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	if err := page.activate(setupCtx); err != nil {
		setupCancel()
		cancel()
		return nil, nil, err
	}
	if err := start.Call(page.page.Context(setupCtx)); err != nil {
		setupCancel()
		cancel()
		return nil, nil, err
	}
	setupCancel()
	go func() {
		wait()
		callbackMu.Lock()
		close(frames)
		callbackMu.Unlock()
	}()
	return frames, stop, nil
}

// DispatchInput 使用 CDP Input 域向页面发送真实浏览器输入，不执行页面 JavaScript。
