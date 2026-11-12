package browserkit

import (
	"context"
	"strings"
	"time"

	"github.com/go-rod/rod/lib/proto"
)

const historySettleInterval = 25 * time.Millisecond
const historySettleDelay = 75 * time.Millisecond
const historySettleTimeout = 5 * time.Second

// mainFrame 返回主文档地址和 loader ID。loader 变化表示提交了新文档，
// 比仅观察历史索引更能区分成功导航和被阻止/尚未提交的导航。
func (page *rodPage) mainFrame(ctx context.Context) (string, string, error) {
	tree, err := (proto.PageGetFrameTree{}).Call(page.page.Context(ctx))
	if err != nil {
		return "", "", err
	}
	if tree == nil || tree.FrameTree == nil || tree.FrameTree.Frame == nil {
		return "", "", nil
	}
	frame := tree.FrameTree.Frame
	return frame.URL + frame.URLFragment, string(frame.LoaderID), nil
}

func pickHistoryTarget(entries []*proto.PageNavigationEntry, current int, direction string) (HistoryEntry, bool) {
	if direction != "back" && direction != "forward" {
		return HistoryEntry{}, false
	}
	if current < 0 || current >= len(entries) {
		return HistoryEntry{}, false
	}
	start := current - 1
	end := -1
	step := -1
	if direction == "forward" {
		start = current + 1
		end = len(entries)
		step = 1
	}
	for index := start; index != end; index += step {
		entry := entries[index]
		if entry == nil || strings.TrimSpace(entry.URL) == "" || strings.HasPrefix(strings.ToLower(entry.URL), "about:") {
			continue
		}
		return HistoryEntry{ID: entry.ID, URL: entry.URL}, true
	}
	return HistoryEntry{}, false
}

// HistoryTarget 解析 back/forward 对应的历史项，尚未执行导航。
func (page *rodPage) HistoryTarget(ctx context.Context, direction string) (HistoryEntry, error) {
	if direction != "back" && direction != "forward" {
		return HistoryEntry{}, newError("invalid_direction", "历史方向只能是 back 或 forward", "使用 back 或 forward", true)
	}
	operationCtx, operationCancel, err := page.ctx(ctx)
	if err != nil {
		return HistoryEntry{}, err
	}
	defer operationCancel()
	history, err := (proto.PageGetNavigationHistory{}).Call(page.page.Context(operationCtx))
	if err != nil {
		return HistoryEntry{}, err
	}
	entry, ok := pickHistoryTarget(history.Entries, history.CurrentIndex, direction)
	if !ok {
		return HistoryEntry{}, newError("history_unavailable", "没有可用的"+direction+"历史", "使用 browserNavigate 打开目标地址", true)
	}
	entry.fromIndex = history.CurrentIndex
	if history.CurrentIndex >= 0 && history.CurrentIndex < len(history.Entries) && history.Entries[history.CurrentIndex] != nil {
		entry.fromURL = history.Entries[history.CurrentIndex].URL
	}
	return entry, nil
}

// historySameDocumentMatches 只接受本次导航后出现、且地址等于历史目标的
// 同文档导航事件；不能用当前页 URL 兜底，否则慢导航会被页面自身的
// replaceState 提前判成功。
func historySameDocumentMatches(sequence, baseline uint64, eventURL string, entry HistoryEntry) bool {
	return sequence > baseline && eventURL == entry.URL
}

func isBrowserErrorURL(address string) bool {
	return strings.HasPrefix(address, "chrome-error://") || strings.HasPrefix(address, "about:neterror")
}

func (page *rodPage) sameDocumentSequence() (uint64, string) {
	page.stateMu.Lock()
	defer page.stateMu.Unlock()
	return page.sameDocSeq, page.sameDocURL
}

// NavigateHistory 使用 CDP 历史记录前进或后退，并等待当前历史序号变化。
// 不通过 history.back() 脚本，避免依赖页面脚本执行和 AllowEvaluate。
func (page *rodPage) NavigateHistory(ctx context.Context, entry HistoryEntry) (PageInfo, error) {
	operationCtx, operationCancel, err := page.ctx(ctx)
	if err != nil {
		return PageInfo{}, err
	}
	defer operationCancel()
	baselineSameDoc, _ := page.sameDocumentSequence()
	_, beforeLoader, _ := page.mainFrame(operationCtx)
	if err := page.runUntilDialog(operationCtx, func(actionCtx context.Context) error {
		return (proto.PageNavigateToHistoryEntry{EntryID: entry.ID}).Call(page.page.Context(actionCtx))
	}); err != nil {
		return PageInfo{}, err
	}
	if page.PendingDialog() != nil {
		return PageInfo{URL: entry.fromURL}, nil
	}
	settleCtx, settleCancel := context.WithTimeout(operationCtx, historySettleTimeout)
	defer settleCancel()
	nextErrorCheck := time.Now().Add(250 * time.Millisecond)
	var settledSince time.Time
	for {
		if page.PendingDialog() != nil {
			return PageInfo{URL: entry.fromURL}, nil
		}
		var current *proto.PageGetNavigationHistoryResult
		var currentErr error
		var info PageInfo
		var infoErr error
		if err := page.runUntilDialog(operationCtx, func(actionCtx context.Context) error {
			current, currentErr = (proto.PageGetNavigationHistory{}).Call(page.page.Context(actionCtx))
			if currentErr == nil {
				info, infoErr = page.info(actionCtx)
			}
			return nil
		}); err != nil {
			return PageInfo{}, err
		}
		if page.PendingDialog() != nil {
			return PageInfo{URL: entry.fromURL}, nil
		}
		indexChanged := currentErr == nil && current.CurrentIndex != entry.fromIndex
		targetCurrent := false
		if indexChanged && current.CurrentIndex >= 0 && current.CurrentIndex < len(current.Entries) {
			currentEntry := current.Entries[current.CurrentIndex]
			targetCurrent = currentEntry != nil && currentEntry.ID == entry.ID
		}
		if page.PendingDialog() != nil {
			return PageInfo{URL: entry.fromURL}, nil
		}
		// 错误页可能不产生新的历史索引；定期检查主 frame，尽早给出
		// navigation_failed，而不是等满 history_timeout。
		if time.Now().After(nextErrorCheck) {
			nextErrorCheck = time.Now().Add(250 * time.Millisecond)
			var frameURL string
			var frameErr error
			if err := page.runUntilDialog(operationCtx, func(actionCtx context.Context) error {
				frameURL, _, frameErr = page.mainFrame(actionCtx)
				return nil
			}); err != nil {
				return PageInfo{}, err
			}
			if page.PendingDialog() != nil {
				return PageInfo{URL: entry.fromURL}, nil
			}
			if frameErr == nil && isBrowserErrorURL(frameURL) {
				return PageInfo{}, newError("navigation_failed", "历史导航进入了浏览器错误页", "检查目标地址是否可访问；需要时重新输入 URL", true)
			}
		}
		// 历史索引可能在导航开始时先变化、被阻止后再回退，因此不能只看
		// 索引；成功还要求提交了新文档（loader 变化）、地址已改变，或发生
		// 了同文档导航（pushState/replaceState 后退时 loader 和 URL 都可能不变）。
		if indexChanged {
			if settledSince.IsZero() {
				settledSince = time.Now()
			}
			if time.Since(settledSince) >= historySettleDelay {
				var frameURL, loaderID string
				var frameErr error
				if err := page.runUntilDialog(operationCtx, func(actionCtx context.Context) error {
					frameURL, loaderID, frameErr = page.mainFrame(actionCtx)
					return nil
				}); err != nil {
					return PageInfo{}, err
				}
				if page.PendingDialog() != nil {
					return PageInfo{URL: entry.fromURL}, nil
				}
				urlChanged := infoErr == nil && info.URL != "" && info.URL != entry.fromURL
				loaderChanged := frameErr == nil && beforeLoader != "" && loaderID != "" && loaderID != beforeLoader
				sameDocSeq, sameDocURL := page.sameDocumentSequence()
				sameDocChanged := historySameDocumentMatches(sameDocSeq, baselineSameDoc, sameDocURL, entry)
				if loaderChanged || urlChanged || sameDocChanged {
					if isBrowserErrorURL(frameURL) {
						return PageInfo{}, newError("navigation_failed", "历史导航进入了浏览器错误页", "检查目标地址是否可访问；需要时重新输入 URL", true)
					}
					if targetCurrent {
						// 目标项成为当前项：慢页面尚未提交时用目标地址兜底。
						if infoErr != nil || info.URL == "" || info.URL == entry.fromURL {
							info = PageInfo{URL: entry.URL}
						}
					} else if infoErr != nil || info.URL == "" {
						info = PageInfo{URL: entry.URL}
					}
					return info, nil
				}
				settledSince = time.Time{}
			}
		} else {
			settledSince = time.Time{}
		}
		if currentErr != nil && contextErr(operationCtx) != nil {
			return PageInfo{}, currentErr
		}
		select {
		case <-operationCtx.Done():
			return PageInfo{}, operationCtx.Err()
		case <-settleCtx.Done():
			if err := operationCtx.Err(); err != nil {
				return PageInfo{}, err
			}
			if frameURL, _, frameErr := page.mainFrame(operationCtx); frameErr == nil && isBrowserErrorURL(frameURL) {
				return PageInfo{}, newError("navigation_failed", "历史导航进入了浏览器错误页", "检查目标地址是否可访问；需要时重新输入 URL", true)
			}
			return PageInfo{}, newError("history_timeout", "浏览器历史没有在预期时间内切换", "确认页面是否被弹窗或 beforeunload 阻止；必要时重新 snapshot", true)
		case <-time.After(historySettleInterval):
		}
	}
}
