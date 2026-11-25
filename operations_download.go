package browserkit

import (
	"context"
	"errors"
	"time"
)

// Download 触发、列出、查询、续等或取消会话内下载；等待超时不会取消下载。
func (manager *Manager) Download(ctx context.Context, request DownloadRequest) (DownloadResponse, error) {
	if err := contextErr(ctx); err != nil {
		return DownloadResponse{}, err
	}
	if manager == nil || manager.config.DownloadDir == "" {
		return DownloadResponse{}, browserError("download_blocked", "宿主未启用文件下载", "配置 DownloadDir 后重试", true)
	}
	if request.SessionID == "" {
		return DownloadResponse{}, errors.New("browserDownload 需要 session_id")
	}
	action := request.Action
	if action == "" {
		switch {
		case request.Ref != "":
			action = "start"
		case request.DownloadID != "":
			action = "query"
		default:
			action = "list"
		}
	}
	if action != "start" && (request.Ref != "" || request.Revision != 0) {
		return DownloadResponse{}, errors.New("只有 start 可使用 ref 和 revision")
	}
	if action == "start" && request.DownloadID != "" {
		return DownloadResponse{}, errors.New("start 不能提供 download_id")
	}
	if action != "start" && action != "list" && action != "query" && action != "wait" && action != "cancel" {
		return DownloadResponse{}, errors.New("无效的下载 action")
	}
	if action == "list" && request.DownloadID != "" {
		return DownloadResponse{}, errors.New("list 不能提供 download_id")
	}
	if action != "start" && action != "list" && request.DownloadID == "" {
		return DownloadResponse{}, errors.New("query、wait、cancel 需要 download_id")
	}
	timeoutMS := request.TimeoutMS
	if timeoutMS == 0 {
		timeout := manager.config.Session.OperationTimeout
		if timeout == 0 {
			timeout = defaultOperationTimeout
		}
		timeoutMS = int(timeout / time.Millisecond)
	}
	downloadCtx, cancel, err := operationContext(ctx, timeoutMS)
	if err != nil {
		return DownloadResponse{}, err
	}
	defer cancel()
	current, err := manager.lockSession(request.SessionID)
	if err != nil {
		return DownloadResponse{}, err
	}
	current.mu.Unlock()
	var info DownloadInfo
	if action == "start" {
		if err := requirePage(request.SessionID, request.PageID, "browserDownload"); err != nil {
			return DownloadResponse{}, err
		}
		if request.Ref == "" || request.Revision == 0 {
			return DownloadResponse{}, errors.New("start 需要 ref 和 revision")
		}
		_, currentPage, getErr := manager.get(downloadCtx, request.SessionID, request.PageID)
		if getErr != nil {
			return DownloadResponse{}, getErr
		}
		downloader, ok := currentPage.page.(DownloadPage)
		if !ok {
			return DownloadResponse{}, browserError("download_unsupported", "当前页面不支持触发下载", "使用支持 DownloadPage 的 Browser Factory", true)
		}
		current.mu.Lock()
		refErr := validatePageRef(currentPage, request.Ref, request.Revision)
		current.mu.Unlock()
		if refErr != nil {
			return DownloadResponse{}, refErr
		}
		info, err = downloader.Download(downloadCtx, ElementRef{Ref: request.Ref, Revision: request.Revision}, manager.config.DownloadDir)
		// 触发回执只确认动作，下载事件没有可核对的动作身份，不能交付猜测的文件。
		info = DownloadInfo{Triggered: info.Triggered}
		info.State = "unconfirmed"
		if err == nil {
			err = downloadUnconfirmedError()
		}
	} else {
		downloads, ok := current.browser.(DownloadSession)
		if !ok {
			return DownloadResponse{}, browserError("download_unsupported", "当前 session 不支持下载生命周期", "使用支持 DownloadSession 的 Browser Factory", true)
		}
		targetID := ""
		if request.PageID != "" {
			_, selected, getErr := manager.get(downloadCtx, request.SessionID, request.PageID)
			if getErr != nil {
				return DownloadResponse{}, getErr
			}
			targetID = selected.targetID
			if targetID == "" {
				return DownloadResponse{}, errors.New("当前页面没有可核对的下载 target 身份")
			}
		}
		if action == "list" {
			listed, listErr := downloads.Downloads(downloadCtx)
			response := DownloadResponse{SessionID: current.id, PageID: request.PageID, Downloads: []DownloadInfo{}}
			for _, download := range listed {
				if targetID == "" || download.TargetID == targetID {
					response.Downloads = append(response.Downloads, download)
				}
			}
			return response, listErr
		}
		if targetID != "" {
			status, statusErr := downloads.DownloadStatus(downloadCtx, request.DownloadID)
			if statusErr != nil {
				return DownloadResponse{}, statusErr
			}
			if status.TargetID != targetID {
				return DownloadResponse{}, errors.New("download_id 不属于此 page_id，或来源尚未确认")
			}
		}
		switch action {
		case "query":
			info, err = downloads.DownloadStatus(downloadCtx, request.DownloadID)
		case "wait":
			info, err = downloads.WaitDownload(downloadCtx, request.DownloadID)
		case "cancel":
			info, err = downloads.CancelDownload(downloadCtx, request.DownloadID)
		}
	}
	response := DownloadResponse{
		Triggered: info.Triggered, SessionID: current.id, PageID: request.PageID, Ref: request.Ref,
		DownloadID: info.ID, State: info.State, FrameID: info.FrameID, TargetID: info.TargetID, URL: info.URL,
		FileName: info.Filename, Path: info.Path, Bytes: info.Bytes, TotalBytes: info.TotalBytes, Error: info.Error,
	}
	if err != nil {
		if action == "start" {
			response.Error = err.Error()
		} else {
			response.WaitError = err.Error()
		}
		if errors.Is(err, context.DeadlineExceeded) && action == "start" {
			return response, errors.Join(browserError("download_trigger_timeout", "触发下载的操作超时；点击完成状态见 triggered，下载关联未确认", "查询页面和会话下载列表；不要自动重放点击", false), err)
		}
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return response, browserError("download_timeout", "本次下载等待超时，下载不会因此取消", "使用 download_id 继续 query 或 wait；尚未收到身份时先 list，不要自动重放", false)
		}
		return response, err
	}
	switch info.State {
	case DownloadFailed, DownloadOverLimit, DownloadSessionClosed:
		return response, browserError("download_"+info.State, "下载状态: "+info.State+": "+info.Error, "已保留下载事实；不要自动重放", false)
	}
	return response, nil
}
