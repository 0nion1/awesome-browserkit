package browserkit

import (
	"context"
	"errors"
)

// Download 点击指定快照元素，并等待下载目录出现完整文件。
func (manager *Manager) Download(ctx context.Context, request DownloadRequest) (DownloadResponse, error) {
	if err := contextErr(ctx); err != nil {
		return DownloadResponse{}, err
	}
	if manager.config.DownloadDir == "" {
		return DownloadResponse{}, browserError("download_blocked", "宿主未启用文件下载", "配置 DownloadDir 后重试", true)
	}
	if err := requirePage(request.SessionID, request.PageID, "browserDownload"); err != nil {
		return DownloadResponse{}, err
	}
	if request.Ref == "" || request.Revision == 0 {
		return DownloadResponse{}, errors.New("browserDownload 需要 ref 和 revision")
	}
	downloadCtx, cancel, err := operationContext(ctx, request.TimeoutMS)
	if err != nil {
		return DownloadResponse{}, err
	}
	defer cancel()
	current, currentPage, err := manager.get(downloadCtx, request.SessionID, request.PageID)
	if err != nil {
		return DownloadResponse{}, err
	}
	downloader, ok := currentPage.page.(DownloadPage)
	if !ok {
		return DownloadResponse{}, browserError("download_unsupported", "当前浏览器页面不支持下载等待", "使用支持 DownloadPage 的 Browser Factory", true)
	}
	current.mu.Lock()
	refErr := validatePageRef(currentPage, request.Ref, request.Revision)
	current.mu.Unlock()
	if refErr != nil {
		return DownloadResponse{}, refErr
	}
	info, err := downloader.Download(downloadCtx, ElementRef{Ref: request.Ref, Revision: request.Revision}, manager.config.DownloadDir)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return DownloadResponse{}, browserError("download_timeout", "等待下载完成超时；下载可能仍在进行", "先检查 DownloadDir 是否已有文件，不要自动重放点击", false)
		}
		if ctx.Err() != nil {
			return DownloadResponse{}, ctx.Err()
		}
		return DownloadResponse{}, err
	}
	manager.emit(ctx, Event{Kind: "download_finished", SessionID: current.id, PageID: currentPage.id})
	return DownloadResponse{
		SessionID: current.id, PageID: currentPage.id, Ref: request.Ref,
		FileName: info.Filename, Path: info.Path, Bytes: info.Bytes,
	}, nil
}
