package browserkit

import (
	"context"
	"errors"
)

// Download 尝试一次原生点击，不把下载事件猜测归属到这次动作。
// Chromium 的下载事件没有动作/request 身份；成功点击返回 download_unconfirmed。
func (page *rodPage) Download(ctx context.Context, ref ElementRef, dir string) (DownloadInfo, error) {
	if page == nil || page.owner == nil {
		return DownloadInfo{}, errors.New("浏览器 page 没有所属 session")
	}
	if dir != page.owner.downloadDir {
		return DownloadInfo{}, errors.New("下载目录必须与 SessionConfig.DownloadDir 一致")
	}
	page.owner.downloadMu.Lock()
	enabled := page.owner.downloadGuids != nil && !page.owner.closed.Load()
	page.owner.downloadMu.Unlock()
	if !enabled {
		return DownloadInfo{}, newError("download_blocked", "当前浏览器 session 未启用下载", "配置 DownloadDir 并重新创建 session", true)
	}
	triggered, err := page.clickWithResult(ctx, ref, ClickOptions{})
	if err != nil {
		return DownloadInfo{Triggered: triggered}, errors.Join(newError("download_click_failed", "触发调用未正常返回，点击完成状态见 triggered: "+err.Error(), "查询下载列表和页面状态；不要自动重放", false), err)
	}
	return DownloadInfo{Triggered: triggered}, downloadUnconfirmedError()
}

func downloadUnconfirmedError() error {
	return newError("download_unconfirmed", "未将下载事件关联到此次触发；点击完成状态见 triggered", "查询会话下载列表并按实际下载身份查询或等待；不要自动重放点击", false)
}

func pageFrameIDs(ctx context.Context, page *rodPage) (map[string]bool, error) {
	frames := map[string]bool{string(page.page.FrameID): true}
	bindings, err := page.frameBindings(ctx)
	if err != nil {
		return nil, err
	}
	for _, binding := range bindings {
		frames[string(binding.FrameID)] = true
	}
	return frames, nil
}
