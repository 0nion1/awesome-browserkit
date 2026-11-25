package browserkit

import (
	"context"
	"errors"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/ysmood/gson"
)

const selectNativeOptions = `function(criteria) {
	'use strict';
	if (!this) return {error: 'stale_reference'};
	if (this.tagName !== 'SELECT' || this.matches(':disabled')) return {error: 'unsupported_select'};
	const options = Array.from(this.options);
	const selected = new Set();
	for (const match of criteria.matches) {
		const index = options.findIndex(option => !option.matches(':disabled') &&
			(criteria.byLabel ? option.label.trim() : option.value) === match);
		if (index < 0) return {error: 'option_not_found'};
		selected.add(index);
	}
	if (this.multiple) {
		options.forEach((option, index) => { option.selected = selected.has(index); });
	} else {
		if (selected.size > 1) return {error: 'select_single_only'};
		this.selectedIndex = selected.size === 0 ? -1 : selected.values().next().value;
	}
	this.dispatchEvent(new Event('input', {bubbles: true, composed: true}));
	this.dispatchEvent(new Event('change', {bubbles: true}));
	return {values: Array.from(this.selectedOptions, option => option.value)};
}`

func (page *rodPage) SelectOption(ctx context.Context, ref ElementRef, value, label string) (string, error) {
	matches, byLabel, err := selectMatches(value, label, nil, nil)
	if err != nil {
		return "", err
	}
	values, err := page.SelectOptions(ctx, ref, matches, byLabel)
	if len(values) == 0 {
		return "", err
	}
	return values[0], err
}

func (page *rodPage) SelectOptions(ctx context.Context, ref ElementRef, matches []string, byLabel bool) ([]string, error) {
	operationCtx, cancel, err := page.ctx(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	if err := lockInteraction(operationCtx, &page.interactionMu); err != nil {
		return nil, err
	}
	defer page.interactionMu.Unlock()
	if matches == nil {
		matches = []string{}
	}
	criteria := map[string]any{"matches": matches, "byLabel": byLabel}
	var result struct {
		Values []string `json:"values"`
		Error  string   `json:"error"`
	}
	if binding, ok := page.frameBindingForRef(operationCtx, ref.Ref); ok {
		if err := page.validateFrameRef(operationCtx, ref); err != nil {
			return nil, err
		}
		client, objectID, err := page.formFrameElement(operationCtx, binding, ref.Ref)
		if err != nil {
			return nil, err
		}
		defer func() { _ = (proto.RuntimeReleaseObject{ObjectID: objectID}).Call(client) }()
		response, err := (proto.RuntimeCallFunctionOn{
			ObjectID: objectID, FunctionDeclaration: selectNativeOptions, ReturnByValue: true,
			Arguments: []*proto.RuntimeCallArgument{{Value: gson.New(criteria)}},
		}).Call(client)
		if err != nil {
			return nil, err
		}
		if response.Result == nil || response.ExceptionDetails != nil {
			return nil, newError("select_unconfirmed", "无法确认选中项状态", "重新调用 browserSnapshot 查看状态", false)
		}
		if err := response.Result.Value.Unmarshal(&result); err != nil {
			return nil, err
		}
	} else {
		el, err := page.elementLocked(operationCtx, ref)
		if err != nil {
			return nil, err
		}
		defer func() { _ = el.Release() }()
		if err := scrollElementNaturally(operationCtx, el); err != nil {
			return nil, err
		}
		if _, err := page.elementPoint(operationCtx, el); err != nil {
			return nil, err
		}
		response, err := el.Evaluate(rod.Eval(selectNativeOptions, criteria).ByUser())
		if err != nil {
			return nil, err
		}
		if err := response.Value.Unmarshal(&result); err != nil {
			return nil, err
		}
	}
	switch result.Error {
	case "":
		return result.Values, nil
	case "stale_reference":
		return nil, newError(result.Error, "frame 中已找不到该元素", "重新调用 browserSnapshot", true)
	case "unsupported_select":
		return nil, newError(result.Error, "目标不是可用的原生下拉框", "重新调用 browserSnapshot 获取 select 的 ref", true)
	case "select_single_only":
		return nil, newError(result.Error, "单选下拉框只能选一项", "选择单个选项", true)
	default:
		return nil, newError(result.Error, "找不到可选的匹配选项", "重新调用 browserSnapshot 获取可用选项", true)
	}
}

func (page *rodPage) UploadFile(ctx context.Context, ref ElementRef, path string) error {
	return page.UploadFiles(ctx, ref, []string{path}, false)
}

const uploadInputMode = `function() {
	if (!this || this.tagName !== 'INPUT' || this.type !== 'file' || this.matches(':disabled')) return '';
	return this.multiple ? 'multiple' : 'single';
}`

func checkUploadInput(mode string, count int) error {
	if mode == "" {
		return newError("invalid_upload_target", "目标不是可用的文件输入框", "重新调用 browserSnapshot 获取 file input 的 ref", true)
	}
	if mode == "single" && count > 1 {
		return newError("upload_single_only", "该文件输入框只支持单个文件", "上传单个文件或选择支持多文件的控件", true)
	}
	return nil
}

func (page *rodPage) formFrameElement(ctx context.Context, binding frameBinding, ref string) (*rod.Page, proto.RuntimeRemoteObjectID, error) {
	objectID, ok := page.frameObject(ctx, binding, frameRefExpression(ref))
	if !ok {
		return nil, "", newError("stale_reference", "跨域 frame 中已找不到该元素", "重新调用 browserSnapshot", true)
	}
	client := binding.Page
	if client == nil {
		client = page.page
	}
	return client.Context(ctx), objectID, nil
}

func (page *rodPage) UploadFiles(ctx context.Context, ref ElementRef, paths []string, chooser bool) error {
	operationCtx, cancel, err := page.ctx(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	if err := lockInteraction(operationCtx, &page.interactionMu); err != nil {
		return err
	}
	defer page.interactionMu.Unlock()
	if binding, ok := page.frameBindingForRef(operationCtx, ref.Ref); ok {
		if err := page.validateFrameRef(operationCtx, ref); err != nil {
			return err
		}
		client := page.page
		if binding.Page != nil {
			client = binding.Page
		}
		if chooser {
			return page.uploadViaChooser(operationCtx, client, binding.FrameID, paths, func(ctx context.Context) error {
				return page.clickFrameElement(ctx, binding, ref.Ref, ClickOptions{})
			})
		}
		client, objectID, err := page.formFrameElement(operationCtx, binding, ref.Ref)
		if err != nil {
			return err
		}
		defer func() { _ = (proto.RuntimeReleaseObject{ObjectID: objectID}).Call(client) }()
		mode, err := (proto.RuntimeCallFunctionOn{ObjectID: objectID, FunctionDeclaration: uploadInputMode, ReturnByValue: true}).Call(client)
		if err != nil {
			return err
		}
		if mode.Result == nil || mode.ExceptionDetails != nil {
			return newError("invalid_upload_target", "无法读取文件输入框状态", "重新调用 browserSnapshot", true)
		}
		if err := checkUploadInput(mode.Result.Value.Str(), len(paths)); err != nil {
			return err
		}
		return (proto.DOMSetFileInputFiles{ObjectID: objectID, Files: paths}).Call(client)
	}
	el, err := page.elementLocked(operationCtx, ref)
	if err != nil {
		return err
	}
	defer func() { _ = el.Release() }()
	if chooser {
		return page.uploadViaChooser(operationCtx, page.page, page.page.FrameID, paths, func(ctx context.Context) error {
			return page.clickElementLocked(ctx, el.Context(ctx), ClickOptions{})
		})
	}
	mode, err := el.Evaluate(rod.Eval(uploadInputMode).ByUser())
	if err != nil {
		return err
	}
	if err := checkUploadInput(mode.Value.Str(), len(paths)); err != nil {
		return err
	}
	return el.SetFiles(paths)
}

func (page *rodPage) uploadViaChooser(ctx context.Context, client *rod.Page, frameID proto.PageFrameID, paths []string, click func(context.Context) error) error {
	chooser := make(chan proto.PageFileChooserOpened, 1)
	eventCtx, stopEvents := context.WithCancel(ctx)
	defer stopEvents()
	wait := client.Context(eventCtx).EachEvent(func(event *proto.PageFileChooserOpened) bool {
		if frameID != "" && event.FrameID != frameID {
			return false
		}
		chooser <- *event
		return true
	})
	go wait()
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), inputCleanupTimeout)
		defer cancel()
		_ = (proto.PageSetInterceptFileChooserDialog{Enabled: false}).Call(client.Context(cleanupCtx))
	}()
	client = client.Context(ctx)
	if err := (proto.PageSetInterceptFileChooserDialog{Enabled: true}).Call(client); err != nil {
		return err
	}
	if err := page.runUntilDialog(ctx, click); err != nil {
		return errors.Join(newError("upload_chooser_unconfirmed", "无法确认触发操作是否已打开文件选择器", "检查页面状态", false), err)
	}
	if page.PendingDialog() != nil {
		return newError("dialog_open", "触发元素操作后出现 JavaScript 弹窗，文件尚未设置", "查看弹窗状态", false)
	}
	select {
	case event := <-chooser:
		if event.BackendNodeID == 0 {
			return newError("upload_chooser_unsupported", "文件选择器未提供原生文件输入框", "选择页面中的 file input", false)
		}
		if len(paths) > 1 && event.Mode != proto.PageFileChooserOpenedModeSelectMultiple {
			return newError("upload_single_only", "该文件选择器只支持单个文件", "检查页面状态后选择单个文件", false)
		}
		return (proto.DOMSetFileInputFiles{BackendNodeID: event.BackendNodeID, Files: paths}).Call(client)
	case <-ctx.Done():
		return errors.Join(newError("upload_chooser_unconfirmed", "未收到文件选择器事件，触发点击可能已经发生", "检查页面状态", false), ctx.Err())
	}
}

const editableInputState = `function() {
		const tag = this.tagName;
		const type = (this.type || '').toLowerCase();
		const textInput = tag === 'INPUT' && ['text', 'search', 'tel', 'url', 'email', 'password', 'number'].includes(type);
		const valueInput = tag === 'INPUT' && ['date', 'month', 'week', 'time', 'datetime-local', 'color', 'range', 'number'].includes(type);
		const editable = this.isContentEditable || tag === 'TEXTAREA' || textInput || valueInput;
		const readonlyCapable = this.isContentEditable || tag === 'TEXTAREA' || textInput || (valueInput && type !== 'range' && type !== 'color');
		const readonly = readonlyCapable && (this.readOnly === true || this.getAttribute('aria-readonly') === 'true');
		const disabled = this.matches(':disabled') || this.getAttribute('aria-disabled') === 'true';
		if (disabled || readonly || !editable) return {error: disabled ? 'element_disabled' : readonly ? 'element_readonly' : 'element_not_editable', tag: tag, inputType: type};
		return {type: type};
	}`

// editableInputType 在任何文档中使用同一编辑性检查。
func editableInputType(ctx context.Context, element *rod.Element) (string, error) {
	state, err := callFormElement(ctx, element, editableInputState)

	if err != nil {
		return "", err
	}
	var editState struct {
		Error     string `json:"error"`
		Type      string `json:"type"`
		Tag       string `json:"tag"`
		InputType string `json:"inputType"`
	}
	if err := state.Value.Unmarshal(&editState); err != nil {
		return "", err
	}
	if editState.Error != "" {
		next := "重新调用 browserSnapshot 选择可编辑的输入框或内容编辑区域"
		switch {
		case editState.Tag == "SELECT":
			next = "使用 browserSelectOption 选择原生下拉框选项"
		case editState.Tag == "INPUT" && (editState.InputType == "checkbox" || editState.InputType == "radio"):
			next = "使用 browserSetChecked 设置勾选状态"
		case editState.Tag == "INPUT" && editState.InputType == "file":
			next = "文件输入请使用 browserUploadFile（需要配置 upload_dir）"
		case editState.Tag == "BUTTON" || editState.Tag == "A" || (editState.Tag == "INPUT" && (editState.InputType == "button" || editState.InputType == "submit" || editState.InputType == "reset" || editState.InputType == "image")):
			next = "该元素不是输入框，使用 browserClick 操作"
		}
		return "", newError(editState.Error, "元素不可编辑", next, true)
	}
	return editState.Type, nil
}

// callFormElement 把输入作为 CDP 参数发送，JS 模板不加工输入内容。
func callFormElement(ctx context.Context, element *rod.Element, function string, arguments ...any) (*proto.RuntimeRemoteObject, error) {
	args := make([]*proto.RuntimeCallArgument, 0, len(arguments))
	for _, argument := range arguments {
		args = append(args, &proto.RuntimeCallArgument{Value: gson.New(argument)})
	}
	result, err := (proto.RuntimeCallFunctionOn{
		ObjectID: element.Object.ObjectID, FunctionDeclaration: function,
		Arguments: args, ReturnByValue: true, AwaitPromise: true, UserGesture: true,
	}).Call(element.Context(ctx))
	if err != nil {
		return nil, err
	}
	if result.Result == nil || result.ExceptionDetails != nil {
		return nil, newError("input_unconfirmed", "无法确认控件操作结果", "重新调用 browserSnapshot 检查状态，不自动重放", false)
	}
	return result.Result, nil
}
