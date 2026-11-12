package browserkit

import (
	"context"
	"errors"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// Download 点击一个快照元素，并通过 CDP 下载事件等待本次下载完成。
// 文件先按 GUID 落盘，完成后重命名为浏览器建议的文件名。
func (page *rodPage) Download(ctx context.Context, ref ElementRef, dir string) (DownloadInfo, error) {
	if page == nil || page.owner == nil {
		return DownloadInfo{}, errors.New("浏览器 page 没有所属 session")
	}
	operationCtx, operationCancel, err := page.ctx(ctx)
	if err != nil {
		return DownloadInfo{}, err
	}
	defer operationCancel()
	if err := lockInteraction(operationCtx, &page.owner.downloadCallMu); err != nil {
		return DownloadInfo{}, downloadPrepareError(err)
	}
	defer page.owner.downloadCallMu.Unlock()
	if err := lockInteraction(operationCtx, &page.interactionMu); err != nil {
		return DownloadInfo{}, downloadPrepareError(err)
	}
	locked := true
	defer func() {
		if locked {
			page.interactionMu.Unlock()
		}
	}()
	element, err := page.elementLocked(operationCtx, ref)
	if err != nil {
		return DownloadInfo{}, downloadPrepareError(err)
	}
	frames, err := pageFrameIDs(operationCtx, page.page)
	if err != nil {
		return DownloadInfo{}, downloadPrepareError(err)
	}
	browser := page.page.Browser()
	// 即使 allowAndName 请求报错或超时，也可能已经在浏览器端生效；
	// 无条件注册 deny 清理，避免下载策略停留在允许状态。
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), downloadCleanupTimeout)
		defer cancel()
		_ = setDownloadBehavior(cleanupCtx, browser, proto.BrowserSetDownloadBehaviorBehaviorDeny, "")
	}()
	waiter, err := page.owner.registerDownload(frames, dir, string(page.page.TargetID))
	if err != nil {
		return DownloadInfo{}, err
	}
	defer page.owner.unregisterDownload(waiter)
	if err := setDownloadBehavior(operationCtx, browser, proto.BrowserSetDownloadBehaviorBehaviorAllowAndName, dir); err != nil {
		return DownloadInfo{}, downloadPrepareError(err)
	}
	page.owner.downloadMu.Lock()
	waiter.clickStarted = true
	page.owner.downloadMu.Unlock()
	if err := page.clickElementLocked(operationCtx, element, ClickOptions{}); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return DownloadInfo{}, newError("download_click_timeout", "触发下载的点击超时", "检查页面状态或增大 timeout_ms；不要自动重放", false)
		}
		return DownloadInfo{}, newError("download_click_failed", "触发下载的点击失败: "+err.Error(), "检查元素状态后重试", true)
	}
	// 只把点击前后短窗口内创建的新 target 视为本次点击结果，
	// 避免页面定时器在调用期间自动弹出的下载被误归属。
	page.owner.downloadMu.Lock()
	waiter.allowTargetsUntil = time.Now().Add(time.Second)
	page.owner.downloadMu.Unlock()
	// 下载可能较慢；等待期间释放页面交互锁，让直播输入和其他页面操作继续。
	page.interactionMu.Unlock()
	locked = false
	select {
	case outcome := <-waiter.result:
		return outcome.info, outcome.err
	case <-operationCtx.Done():
		return DownloadInfo{}, operationCtx.Err()
	}
}

func downloadPrepareError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return newError("download_prepare_timeout", "下载准备阶段超时，尚未触发点击", "确认页面可操作或增大 timeout_ms 后重试", true)
	}
	return err
}

func pageFrameIDs(ctx context.Context, page *rod.Page) (map[string]bool, error) {
	tree, err := (proto.PageGetFrameTree{}).Call(page.Context(ctx))
	if err != nil {
		return nil, err
	}
	frames := map[string]bool{}
	var walk func(*proto.PageFrameTree)
	walk = func(node *proto.PageFrameTree) {
		if node == nil || node.Frame == nil {
			return
		}
		frames[string(node.Frame.ID)] = true
		for _, child := range node.ChildFrames {
			walk(child)
		}
	}
	walk(tree.FrameTree)
	if len(frames) == 0 {
		return nil, errors.New("页面没有可用的 frame")
	}
	return frames, nil
}
