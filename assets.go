package browserkit

import (
	"context"
	"errors"
	"net/url"
	"strings"
)

const (
	assetsDefaultLimit = 100
	assetsMaxLimit     = 500
)

// AssetsRequest 是 browserAssets 的输入。
type AssetsRequest struct {
	SessionID string `json:"session_id" jsonschema_description:"浏览器 session 标识；原样使用 browserOpen 或 browserTabs 返回的 session_id。留空时使用当前活动 session。"`
	PageID    string `json:"page_id,omitempty" jsonschema_description:"可选的目标 page_id；省略时使用所选 session 的当前激活 Tab。"`
	Host      string `json:"host,omitempty" jsonschema_description:"可选的资源主机名过滤条件，不区分大小写且精确匹配，不自动匹配子域名；也可填写完整 URL，此时只取 hostname；省略时返回所有主机的资源。"`
	Limit     int    `json:"limit,omitempty" jsonschema_description:"最多返回的最近资源条数；省略或为 0 时默认 100，允许 1 到 500；更早记录被省略时响应 truncated 为 true。"`
}

// AssetsResponse 是 browserAssets 的有界只读响应。
type AssetsResponse struct {
	SessionID string  `json:"session_id"`
	PageID    string  `json:"page_id"`
	Host      string  `json:"host,omitempty"`
	Assets    []Asset `json:"assets"`
	Truncated bool    `json:"truncated,omitempty"`
}

func (manager *Manager) listAssets(ctx context.Context, request AssetsRequest) (AssetsResponse, error) {
	if manager == nil {
		return AssetsResponse{}, browserError("browser_unavailable", "Browser Manager 不存在", "调用 NewManager 创建浏览器运行时", true)
	}
	if request.Limit == 0 {
		request.Limit = assetsDefaultLimit
	}
	if request.Limit < 1 || request.Limit > assetsMaxLimit {
		return AssetsResponse{}, errors.New("browserAssets limit 必须位于 1 到 500")
	}
	host := strings.TrimSpace(request.Host)
	if host != "" {
		if strings.Contains(host, "://") {
			parsed, err := url.Parse(host)
			if err != nil || parsed.Hostname() == "" {
				return AssetsResponse{}, errors.New("browserAssets host 无效")
			}
			host = parsed.Hostname()
		}
		if len(host) > 253 || strings.ContainsAny(host, "/?#") {
			return AssetsResponse{}, errors.New("browserAssets host 无效")
		}
	}
	var current *session
	var value *page
	var err error
	if request.SessionID == "" {
		current, value, err = manager.devtoolsPage("", request.PageID)
	} else {
		current, value, err = manager.get(ctx, request.SessionID, request.PageID)
	}
	if err != nil {
		return AssetsResponse{}, err
	}
	page, ok := value.page.(AssetsPage)
	if !ok {
		return AssetsResponse{}, browserError("assets_unsupported", "当前浏览器页面不支持资源列表", "使用支持 BrowserKit AssetsPage 的 Browser Factory", true)
	}
	result, err := page.ListAssets(ctx, ListAssetsOptions{Host: host, Limit: request.Limit})
	if err != nil {
		return AssetsResponse{}, err
	}
	return AssetsResponse{SessionID: current.id, PageID: value.id, Host: host, Assets: result.Assets, Truncated: result.Truncated}, nil
}

// ListAssets 返回当前页面已经观测到的资源列表。
func (manager *Manager) ListAssets(ctx context.Context, request AssetsRequest) (AssetsResponse, error) {
	return manager.listAssets(ctx, request)
}
