package browserkit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const maxUploadFileBytes = 10 << 20

// UploadFile 将授权目录内一个文件设置到页面中的原生文件输入框。
func (manager *Manager) UploadFile(ctx context.Context, request UploadFileRequest) (UploadFileResponse, error) {
	if err := contextErr(ctx); err != nil {
		return UploadFileResponse{}, err
	}
	if manager.config.UploadDir == "" {
		return UploadFileResponse{}, browserError("upload_blocked", "宿主未启用文件上传", "配置 UploadDir 后重试", true)
	}
	if err := requirePage(request.SessionID, request.PageID, "browserUploadFile"); err != nil {
		return UploadFileResponse{}, err
	}
	if request.Ref == "" || request.Revision == 0 || !filepath.IsAbs(request.Path) {
		return UploadFileResponse{}, errors.New("browserUploadFile 需要 ref、revision 和绝对文件路径")
	}
	path, err := filepath.EvalSymlinks(request.Path)
	if err != nil {
		return UploadFileResponse{}, browserError("upload_file_not_found", "上传文件不存在", "检查 path", true)
	}
	relative, err := filepath.Rel(manager.config.UploadDir, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || relative == "." {
		return UploadFileResponse{}, browserError("upload_path_blocked", "文件不在授权上传目录内", "选择 UploadDir 中的文件", true)
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxUploadFileBytes {
		return UploadFileResponse{}, browserError("upload_file_invalid", "文件必须是 10 MiB 以内的普通文件", "选择较小的普通文件", true)
	}
	current, currentPage, err := manager.get(ctx, request.SessionID, request.PageID)
	if err != nil {
		return UploadFileResponse{}, err
	}
	uploader, ok := currentPage.page.(UploadFilePage)
	if !ok {
		return UploadFileResponse{}, browserError("upload_unsupported", "当前浏览器页面不支持文件上传", "使用支持 UploadFilePage 的 Browser Factory", true)
	}
	unlock, err := lockSessionInteraction(ctx, current)
	if err != nil {
		return UploadFileResponse{}, err
	}
	defer unlock()
	current.mu.Lock()
	refErr := validatePageRef(currentPage, request.Ref, request.Revision)
	current.mu.Unlock()
	if refErr != nil {
		return UploadFileResponse{}, refErr
	}
	if err := uploader.UploadFile(ctx, ElementRef{Ref: request.Ref, Revision: request.Revision}, path); err != nil {
		return UploadFileResponse{}, err
	}
	manager.emit(ctx, Event{Kind: "interaction_finished", SessionID: current.id, PageID: currentPage.id})
	unlock()
	_ = manager.reconcileTabs(ctx, current)
	response := UploadFileResponse{SessionID: current.id, PageID: currentPage.id, Ref: request.Ref, FileName: filepath.Base(path), Bytes: info.Size()}
	response.Snapshot, response.SnapshotError = manager.actionSnapshot(ctx, current, currentPage)
	return response, nil
}
