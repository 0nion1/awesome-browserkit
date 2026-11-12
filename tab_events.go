package browserkit

import "time"

// tabChangeSettle 合并连续 target 事件。导航期间标题和 URL 会密集变化，
// 静默一小段时间后再枚举，避免每个事件都触发一次 CDP 查询。
const tabChangeSettle = 50 * time.Millisecond

// watchTabEvents 把浏览器 target 事件转换为一次 Tab 枚举和状态通知。
// 事件只在真实变化时产生，平时没有轮询；没有状态订阅者时仍会同步 Tab 投影，
// 便于后续显式读取直接使用最新映射。
func (manager *Manager) watchTabEvents(current *session) {
	if manager == nil || current == nil {
		return
	}
	source, ok := current.browser.(TabChangeSession)
	if !ok {
		return
	}
	go func() {
		// 注册 CDP 监听可能需要启用 Target domain，不能占用创建 session 的调用路径。
		changes := source.TabChanges()
		if changes == nil {
			return
		}
		for {
			select {
			case <-current.ctx.Done():
				return
			case <-manager.lifecycleCtx.Done():
				return
			case _, ok := <-changes:
				if !ok {
					return
				}
			}
			if !manager.settleTabChanges(current, changes) {
				return
			}
			current.mu.Lock()
			before := tabsRevisionLocked(current)
			current.mu.Unlock()
			if err := manager.reconcileTabsWait(current.ctx, current); err != nil {
				continue
			}
			current.mu.Lock()
			after := tabsRevisionLocked(current)
			closed := current.closed || current.browser.Closed()
			current.mu.Unlock()
			if closed {
				return
			}
			// reconcile 会为 URL 变化发出事件；标题、状态等不会发事件的
			// 变化在这里补一次通知，确保推送覆盖所有可见投影。
			if after != before {
				manager.notifyState()
			}
		}
	}()
}

// settleTabChanges 在短暂静默后返回 true，期间到达的事件被合并。
func (manager *Manager) settleTabChanges(current *session, changes <-chan struct{}) bool {
	timer := time.NewTimer(tabChangeSettle)
	defer timer.Stop()
	for {
		select {
		case <-current.ctx.Done():
			return false
		case <-manager.lifecycleCtx.Done():
			return false
		case _, ok := <-changes:
			if !ok {
				return false
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(tabChangeSettle)
		case <-timer.C:
			return true
		}
	}
}
