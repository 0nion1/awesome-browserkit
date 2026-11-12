package browserkit

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

const downloadCleanupTimeout = 2 * time.Second

type downloadWaiter struct {
	frames            map[string]bool
	dir               string
	openerTargetID    string
	createdTargets    map[string]bool
	allowTargetsUntil time.Time
	clickStarted      bool
	begin             *proto.BrowserDownloadWillBegin
	guid              string
	completed         bool
	result            chan downloadOutcome
}

func (session *rodSession) registerDownload(frames map[string]bool, dir, openerTargetID string) (*downloadWaiter, error) {
	session.downloadMu.Lock()
	defer session.downloadMu.Unlock()
	if session.downloadFrames == nil || session.downloadActive == nil {
		return nil, newError("download_blocked", "当前浏览器 session 未启用下载", "配置 DownloadDir 并重新创建 session", true)
	}
	waiter := &downloadWaiter{
		frames: frames, dir: dir, openerTargetID: openerTargetID,
		createdTargets: map[string]bool{}, allowTargetsUntil: time.Now().Add(defaultOperationTimeout),
		result: make(chan downloadOutcome, 1),
	}
	for frame := range frames {
		session.downloadFrames[frame] = waiter
	}
	session.downloadActive[waiter] = true
	return waiter, nil
}

type downloadOutcome struct {
	info DownloadInfo
	err  error
}

// startDownloadWatcher 常驻监听下载事件。下载目录只在单次 browserDownload
// 调用期间启用，其他来源的下载会被取消。
func (session *rodSession) startDownloadWatcher() {
	wait := session.browser.EachEvent(
		func(event *proto.BrowserDownloadWillBegin) { session.downloadWillBegin(event) },
		func(event *proto.BrowserDownloadProgress) { session.downloadProgress(event) },
		func(event *proto.TargetTargetCreated) { session.downloadTargetCreated(event) },
	)
	go wait()
}

func (session *rodSession) downloadTargetCreated(event *proto.TargetTargetCreated) {
	if event == nil || event.TargetInfo == nil || event.TargetInfo.Type != proto.TargetTargetInfoTypePage || event.TargetInfo.OpenerID == "" {
		return
	}
	session.downloadMu.Lock()
	defer session.downloadMu.Unlock()
	targetID := string(event.TargetInfo.TargetID)
	now := time.Now()
	for waiter := range session.downloadActive {
		if waiter.openerTargetID != "" && string(event.TargetInfo.OpenerID) == waiter.openerTargetID &&
			waiter.clickStarted && now.Before(waiter.allowTargetsUntil) {
			waiter.createdTargets[targetID] = true
		}
	}
}

func (session *rodSession) unregisterDownload(waiter *downloadWaiter) {
	session.downloadMu.Lock()
	for frame := range waiter.frames {
		if session.downloadFrames[frame] == waiter {
			delete(session.downloadFrames, frame)
		}
	}
	delete(session.downloadActive, waiter)
	guid := waiter.guid
	completed := waiter.completed
	if guid != "" && session.downloadGuids[guid] == waiter {
		delete(session.downloadGuids, guid)
	}
	session.downloadMu.Unlock()
	if guid != "" && !completed {
		session.cancelDownload(guid)
	}
}

func (session *rodSession) downloadWillBegin(event *proto.BrowserDownloadWillBegin) {
	if event == nil {
		return
	}
	session.downloadMu.Lock()
	frameID := string(event.FrameID)
	waiter := session.downloadFrames[frameID]
	if waiter == nil {
		// 新标签顶层 frame 的 ID 等于 target ID；只接受本次调用新建、
		// opener 指向被点击页面的 target。新标签内部 iframe 不在此集合中。
		if len(session.downloadActive) == 1 {
			for active := range session.downloadActive {
				if active.createdTargets[frameID] {
					waiter = active
				}
			}
		}
	}
	if waiter == nil || waiter.guid != "" {
		session.downloadMu.Unlock()
		// 非本次 browserDownload 触发的下载不允许落盘。取消请求放到
		// 事件循环外，避免阻塞后续下载事件。
		go session.cancelDownload(event.GUID)
		return
	}
	waiter.begin = event
	waiter.guid = event.GUID
	session.downloadGuids[event.GUID] = waiter
	session.downloadMu.Unlock()
}

func (session *rodSession) downloadProgress(event *proto.BrowserDownloadProgress) {
	if event == nil {
		return
	}
	session.downloadMu.Lock()
	waiter := session.downloadGuids[event.GUID]
	if waiter == nil || waiter.completed {
		session.downloadMu.Unlock()
		return
	}
	switch event.State {
	case proto.BrowserDownloadProgressStateCompleted:
		waiter.completed = true
		delete(session.downloadGuids, event.GUID)
		session.downloadMu.Unlock()
		info, err := finalizeDownload(waiter.dir, waiter.begin)
		waiter.result <- downloadOutcome{info: info, err: err}
	case proto.BrowserDownloadProgressStateCanceled:
		waiter.completed = true
		delete(session.downloadGuids, event.GUID)
		session.downloadMu.Unlock()
		waiter.result <- downloadOutcome{err: newError("download_canceled", "浏览器取消了下载", "检查下载目录写入权限或重新触发下载", true)}
	default:
		session.downloadMu.Unlock()
	}
}

func (session *rodSession) cancelDownload(guid string) {
	if guid == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), downloadCleanupTimeout)
	defer cancel()
	_ = (proto.BrowserCancelDownload{GUID: guid}).Call(session.browser.Context(ctx))
}

func finalizeDownload(dir string, begin *proto.BrowserDownloadWillBegin) (DownloadInfo, error) {
	if begin == nil || begin.GUID == "" {
		return DownloadInfo{}, errors.New("下载事件缺少 GUID")
	}
	if begin.GUID != filepath.Base(begin.GUID) || strings.ContainsAny(begin.GUID, `/\`) {
		return DownloadInfo{}, errors.New("下载 GUID 无效")
	}
	source := filepath.Join(dir, begin.GUID)
	info, err := os.Lstat(source)
	if err != nil {
		return DownloadInfo{}, err
	}
	if !info.Mode().IsRegular() {
		return DownloadInfo{}, errors.New("下载结果不是普通文件")
	}
	name := safeDownloadName(begin.SuggestedFilename, begin.GUID)
	path, err := moveDownloadFile(dir, source, name)
	if err != nil {
		return DownloadInfo{}, err
	}
	finalInfo, err := os.Lstat(path)
	if err != nil {
		return DownloadInfo{}, err
	}
	if !finalInfo.Mode().IsRegular() {
		return DownloadInfo{}, errors.New("下载结果不是普通文件")
	}
	return DownloadInfo{Filename: filepath.Base(path), Path: path, Bytes: finalInfo.Size()}, nil
}

// moveDownloadFile 用硬链接原子地占用唯一文件名，避免 Lstat+Rename 的竞态覆盖。
func moveDownloadFile(dir, source, name string) (string, error) {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for index := 0; index < 10_000; index++ {
		candidateName := name
		if index > 0 {
			candidateName = base + " (" + strconv.Itoa(index) + ")" + ext
		}
		candidate := filepath.Join(dir, candidateName)
		err := os.Link(source, candidate)
		if err == nil {
			_ = os.Remove(source)
			return candidate, nil
		}
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		// 不支持硬链接的文件系统：用 O_EXCL 占位后再 rename，
		// 避免覆盖其他并发下载。
		placeholder, createErr := os.OpenFile(candidate, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if createErr == nil {
			_ = placeholder.Close()
			if renameErr := os.Rename(source, candidate); renameErr != nil {
				_ = os.Remove(candidate)
				return "", renameErr
			}
			return candidate, nil
		}
		if errors.Is(createErr, fs.ErrExist) {
			continue
		}
		return "", createErr
	}
	return "", errors.New("无法生成唯一的下载文件名")
}

func safeDownloadName(name, fallback string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' {
			return -1
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." {
		name = fallback
	}
	const maxBytes = 240
	if len(name) > maxBytes {
		ext := filepath.Ext(name)
		if len(ext) > 40 {
			ext = ""
		}
		base := strings.TrimSuffix(name, ext)
		budget := maxBytes - len(ext)
		for budget > 0 && !utf8.RuneStart(base[budget]) {
			budget--
		}
		name = base[:budget] + ext
	}
	return name
}

func setDownloadBehavior(ctx context.Context, browser *rod.Browser, behavior proto.BrowserSetDownloadBehaviorBehavior, dir string) error {
	return (proto.BrowserSetDownloadBehavior{
		Behavior: behavior, DownloadPath: dir, EventsEnabled: true,
	}).Call(browser.Context(ctx))
}
