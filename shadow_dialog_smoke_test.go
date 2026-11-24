//go:build browser_smoke

package browserkit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestRealBrowserNestedShadowDialog 验证嵌套 open Shadow DOM 内的引用可以
// 完成输入和点击，原生 prompt 仍交给显式弹窗接口处理；同源 iframe 使用相同路径。
func TestRealBrowserNestedShadowDialog(t *testing.T) {
	const document = `<!doctype html><title>Shadow dialog</title><div id="host"></div>
<script>
const outer = document.querySelector('#host').attachShadow({mode:'open'});
outer.innerHTML = '<div id="nested"></div>';
const inner = outer.querySelector('#nested').attachShadow({mode:'open'});
inner.innerHTML = '<input aria-label="Shadow name"><button>Shadow submit</button><p>Shadow ready</p>';
inner.querySelector('button').onclick = () => {
  const answer = prompt('Shadow approval', 'default');
  inner.querySelector('p').textContent = 'Shadow result: ' + inner.querySelector('input').value + ' / ' + answer;
};
</script>`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Path == "/iframe" {
			_, _ = w.Write([]byte(`<!doctype html><title>Frame host</title><iframe src="/shadow" width="700" height="400"></iframe>`))
			return
		}
		_, _ = w.Write([]byte(document))
	}))
	defer server.Close()
	for _, path := range []string{"/shadow", "/iframe"} {
		t.Run(path, func(t *testing.T) {
			manager, err := NewManager(Config{AllowPrivateNetwork: true})
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Close(context.Background())
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			opened, err := manager.Open(ctx, OpenRequest{URL: server.URL + path})
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(snapshot.Text, "Shadow ready") {
				t.Fatalf("快照遗漏嵌套 Shadow DOM 正文: %q", snapshot.Text)
			}
			input := smokeElementRef(t, snapshot, "textbox", "Shadow name")
			if _, err := manager.Type(ctx, TypeRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: input, Revision: snapshot.Revision, Text: "BrowserKit"}); err != nil {
				t.Fatal(err)
			}
			snapshot, err = manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
			if err != nil {
				t.Fatal(err)
			}
			button := smokeElementRef(t, snapshot, "button", "Shadow submit")
			clicked, err := manager.Click(ctx, ClickRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: button, Revision: snapshot.Revision})
			if err != nil || clicked.Snapshot == nil || len(clicked.Snapshot.Dialogs) != 1 {
				t.Fatalf("Shadow DOM 点击未返回弹窗: %+v, err=%v", clicked, err)
			}
			dialog := clicked.Snapshot.Dialogs[0]
			if dialog.Type != "prompt" || dialog.Message != "Shadow approval" || dialog.Handled {
				t.Fatalf("弹窗状态错误: %+v", dialog)
			}
			handled, err := manager.HandleDialog(ctx, HandleDialogRequest{SessionID: opened.SessionID, PageID: opened.PageID, DialogID: dialog.ID, Action: "accept", PromptText: "approved"})
			if err != nil || handled.Snapshot == nil || !strings.Contains(handled.Snapshot.Text, "Shadow result: BrowserKit / approved") {
				t.Fatalf("输入、点击或 prompt 结果未生效: %+v, err=%v", handled, err)
			}
		})
	}
}
