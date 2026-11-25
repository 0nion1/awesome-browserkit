package browserkit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const maxUploadFileBytes = 10 << 20

// UploadFile 将授权目录内的文件设置到原生文件输入框或文件选择器。
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
	if request.Ref == "" || request.Revision == 0 {
		return UploadFileResponse{}, errors.New("browserUploadFile 需要 ref 和 revision")
	}
	if (request.Path != "") == (request.Paths != nil) {
		return UploadFileResponse{}, errors.New("browserUploadFile 需要 path 或 paths 其中之一")
	}
	requested := request.Paths
	if request.Path != "" {
		requested = []string{request.Path}
	}
	if len(requested) == 0 {
		return UploadFileResponse{}, errors.New("browserUploadFile 至少需要一个文件")
	}
	current, currentPage, err := manager.get(ctx, request.SessionID, request.PageID)
	if err != nil {
		return UploadFileResponse{}, err
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
	paths := make([]string, 0, len(requested))
	files := make([]UploadedFile, 0, len(requested))
	var totalBytes int64
	for _, path := range requested {
		if err := contextErr(ctx); err != nil {
			return UploadFileResponse{}, err
		}
		resolved, file, err := validateUploadFile(manager.config.UploadDir, path)
		if err != nil {
			return UploadFileResponse{}, err
		}
		paths = append(paths, resolved)
		files = append(files, file)
		totalBytes += file.Bytes
	}
	ref := ElementRef{Ref: request.Ref, Revision: request.Revision}
	response := UploadFileResponse{SessionID: current.id, PageID: currentPage.id, Ref: request.Ref}
	if uploader, ok := currentPage.page.(UploadFilesPage); ok {
		err = uploader.UploadFiles(ctx, ref, paths, request.Chooser)
	} else if uploader, ok := currentPage.page.(UploadFilePage); ok && !request.Chooser && len(paths) == 1 {
		err = uploader.UploadFile(ctx, ref, paths[0])
	} else {
		err = browserError("upload_unsupported", "当前浏览器页面不支持该上传方式", "使用支持 UploadFilesPage 的 Browser Factory", true)
	}
	if err != nil {
		return response, err
	}
	response.Uploaded, response.Files, response.Bytes = true, files, totalBytes
	if len(response.Files) == 1 {
		response.FileName = response.Files[0].FileName
	}
	manager.emit(ctx, Event{Kind: "interaction_finished", SessionID: current.id, PageID: currentPage.id})
	unlock()
	_ = manager.reconcileTabs(ctx, current)
	response.Snapshot, response.SnapshotError = manager.actionSnapshot(ctx, current, currentPage)
	return response, nil
}

func validateUploadFile(dir, path string) (string, UploadedFile, error) {
	if !filepath.IsAbs(path) {
		return "", UploadedFile{}, errors.New("browserUploadFile 需要绝对文件路径")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", UploadedFile{}, browserError("upload_file_not_found", "上传文件不存在", "检查文件路径", true)
	}
	relative, err := filepath.Rel(dir, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || relative == "." {
		return "", UploadedFile{}, browserError("upload_path_blocked", "文件不在授权上传目录内", "选择 UploadDir 中的文件", true)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxUploadFileBytes {
		return "", UploadedFile{}, browserError("upload_file_invalid", "文件必须是 10 MiB 以内的普通文件", "选择较小的普通文件", true)
	}
	return resolved, UploadedFile{FileName: filepath.Base(resolved), Bytes: info.Size()}, nil
}
