package browserkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/go-rod/rod/lib/proto"
)

func (page *rodPage) Evaluate(ctx context.Context, code string, options EvaluateOptions) (EvaluateResult, error) {
	if err := contextErr(ctx); err != nil {
		return EvaluateResult{}, err
	}
	if strings.TrimSpace(code) == "" {
		return EvaluateResult{}, errors.New("JavaScript 代码不能为空")
	}
	operationCtx, operationCancel, err := page.ctx(ctx)
	if err != nil {
		return EvaluateResult{}, err
	}
	defer operationCancel()
	evalCtx := operationCtx
	if options.Timeout > 0 {
		var cancel context.CancelFunc
		evalCtx, cancel = context.WithTimeout(operationCtx, options.Timeout)
		defer cancel()
	}
	// Rod Evaluate 只接受函数形式并固定若干 Runtime 参数，无法保持本接口
	// 的任意表达式、AwaitPromise、CSP 和 command-line API 语义，因此这里
	// 使用 Runtime.evaluate；调用仍绑定 evalCtx 的独立超时。
	result, err := (proto.RuntimeEvaluate{
		Expression:                  code,
		IncludeCommandLineAPI:       true,
		ReturnByValue:               true,
		AwaitPromise:                options.AwaitPromise,
		UserGesture:                 true,
		AllowUnsafeEvalBlockedByCSP: true,
	}).Call(page.page.Context(evalCtx))
	if err != nil {
		return EvaluateResult{}, err
	}
	if result.ExceptionDetails != nil {
		return EvaluateResult{}, fmt.Errorf("JavaScript 执行失败: %s", result.ExceptionDetails.Text)
	}
	if result.Result == nil {
		return EvaluateResult{}, errors.New("JavaScript 没有返回值")
	}
	if result.Result.UnserializableValue != "" {
		return EvaluateResult{}, newError("unserializable_result", "JavaScript 返回值不可序列化", "只返回 JSON、字符串、数字、布尔值或 null", true)
	}
	value, err := json.Marshal(result.Result.Value)
	if err != nil || !json.Valid(value) {
		return EvaluateResult{}, newError("unserializable_result", "JavaScript 返回值不是有效 JSON", "不要返回 DOM 节点、函数或循环引用对象", true)
	}
	return EvaluateResult{Value: value, Type: string(result.Result.Type)}, nil
}

func (page *rodPage) ListAssets(ctx context.Context, options ListAssetsOptions) (ListAssetsResult, error) {
	if err := contextErr(ctx); err != nil {
		return ListAssetsResult{}, err
	}
	if options.Limit <= 0 || options.Limit > 500 {
		return ListAssetsResult{}, errors.New("资源列表 limit 必须位于 1 到 500")
	}
	operationCtx, operationCancel, err := page.ctx(ctx)
	if err != nil {
		return ListAssetsResult{}, err
	}
	defer operationCancel()
	encodedHost, _ := json.Marshal(strings.TrimSpace(options.Host))
	expression := `(function(host, limit) {
		const wanted = String(host || '').toLowerCase();
		const entries = performance.getEntriesByType('resource');
		const result = [];
		for (const item of entries) {
			if (!item || !item.name) continue;
			let url;
			try { url = new URL(item.name, location.href); } catch { continue; }
			if (wanted && url.hostname.toLowerCase() !== wanted) continue;
			const path = url.pathname.split('/').filter(Boolean);
			result.push({
				name: path[path.length - 1] || url.hostname,
				url: url.href,
				initiator_type: item.initiatorType || '',
				response_status: Number(item.responseStatus) || 0,
				transfer_bytes: Number(item.transferSize) || 0,
				encoded_bytes: Number(item.encodedBodySize) || 0,
				decoded_bytes: Number(item.decodedBodySize) || 0,
				duration_ms: Number(item.duration) || 0
			});
		}
		return result.slice(-Math.max(1, Math.min(501, Number(limit) || 100)));
	})(` + string(encodedHost) + `,` + fmt.Sprint(options.Limit+1) + `)`
	result, err := (proto.RuntimeEvaluate{Expression: expression, ReturnByValue: true}).Call(page.page.Context(operationCtx))
	if err != nil {
		return ListAssetsResult{}, err
	}
	if result.ExceptionDetails != nil || result.Result == nil {
		return ListAssetsResult{}, errors.New("读取页面资源列表失败")
	}
	value, err := json.Marshal(result.Result.Value)
	if err != nil {
		return ListAssetsResult{}, err
	}
	var assets []Asset
	if err := json.Unmarshal(value, &assets); err != nil {
		return ListAssetsResult{}, fmt.Errorf("解码页面资源列表: %w", err)
	}
	resultValue := ListAssetsResult{Assets: assets, Truncated: len(assets) > options.Limit}
	if resultValue.Truncated {
		resultValue.Assets = assets[len(assets)-options.Limit:]
	}
	return resultValue, nil
}

func (page *rodPage) AddScriptToEvaluateOnNewDocument(ctx context.Context, script string) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	if strings.TrimSpace(script) == "" {
		return errors.New("注入 JavaScript 不能为空")
	}
	operationCtx, operationCancel, err := page.ctx(ctx)
	if err != nil {
		return err
	}
	defer operationCancel()
	// EvalOnNewDocument 是 Rod 对 Page.addScriptToEvaluateOnNewDocument 的
	// 包装；这里只需安装脚本，不需要向调用方暴露 remove 回调。
	_, err = page.page.Context(operationCtx).EvalOnNewDocument(script)
	return err
}
