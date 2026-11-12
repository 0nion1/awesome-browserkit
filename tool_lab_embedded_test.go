//go:build browser_smoke

package browserkit

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeoxisca/awesome-browserkit/internal/toollab"
)

func TestRealBrowserEmbeddedTargets(t *testing.T) {
	server := httptest.NewServer(toollab.Handler())
	defer server.Close()
	uploadDir := t.TempDir()
	uploadPath := filepath.Join(uploadDir, "sample.txt")
	if err := os.WriteFile(uploadPath, []byte("fixture-content"), 0600); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(Config{AllowPrivateNetwork: true, AllowEvaluate: true, UploadDir: uploadDir})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	lab := &toolLabRunner{t: t, ctx: ctx, manager: manager, called: make(map[string]bool)}
	opened := toolLabCall[OpenResponse](lab, "browserOpen", map[string]any{"url": server.URL + "/", "viewport": map[string]int{"width": 900, "height": 640}})
	snapshot := toolLabCall[SnapshotResponse](lab, "browserSnapshot", map[string]any{"session_id": opened.SessionID, "page_id": opened.PageID, "max_elements": 100})
	toolLabElement(t, snapshot, "textbox", "Shadow code")
	toolLabElement(t, snapshot, "button", "Shadow action")
	toolLabElement(t, snapshot, "textbox", "Frame code")
	toolLabElement(t, snapshot, "button", "Frame action")
	toolLabElement(t, snapshot, "button", "Frame shadow action")
	toolLabElement(t, snapshot, "button", "Frame attachment")
	toolLabElement(t, snapshot, "frame", "Embedded checkout")
	toolLabElement(t, snapshot, "frame", "External checkout")
	toolLabElement(t, snapshot, "button", "Attachment")
	for _, target := range []struct{ field, action, result string }{
		{"Shadow code", "Shadow action", "Shadow clicked: S7"},
		{"Frame code", "Frame action", "Frame clicked: S7"},
	} {
		field := toolLabElement(t, snapshot, "textbox", target.field)
		toolLabCall[TypeResponse](lab, "browserType", map[string]any{
			"session_id": opened.SessionID, "page_id": opened.PageID,
			"ref": field.Ref, "revision": snapshot.Revision, "text": "S7",
		})
		toolLabClick(lab, opened.SessionID, opened.PageID, snapshot, "button", target.action)
		toolLabWait(lab, opened.SessionID, opened.PageID, "text_contains", target.result)
	}
	toolLabClick(lab, opened.SessionID, opened.PageID, snapshot, "button", "Frame shadow action")
	toolLabWait(lab, opened.SessionID, opened.PageID, "text_contains", "Frame shadow clicked")
	attachment := toolLabElement(t, snapshot, "button", "Attachment")
	uploaded := toolLabCall[UploadFileResponse](lab, "browserUploadFile", map[string]any{
		"session_id": opened.SessionID, "page_id": opened.PageID,
		"ref": attachment.Ref, "revision": snapshot.Revision, "path": uploadPath,
	})
	if uploaded.FileName != "sample.txt" || uploaded.Bytes != int64(len("fixture-content")) {
		t.Fatalf("upload response: %+v", uploaded)
	}
	toolLabWait(lab, opened.SessionID, opened.PageID, "text_contains", "Uploaded: sample.txt; bytes: 15; content: fixture-content")
	frameAttachment := toolLabElement(t, snapshot, "button", "Frame attachment")
	toolLabCall[UploadFileResponse](lab, "browserUploadFile", map[string]any{
		"session_id": opened.SessionID, "page_id": opened.PageID,
		"ref": frameAttachment.Ref, "revision": snapshot.Revision, "path": uploadPath,
	})
	toolLabWait(lab, opened.SessionID, opened.PageID, "text_contains", "Frame uploaded: sample.txt")
	toolLabWait(lab, opened.SessionID, opened.PageID, "selector_visible", "#frame-action")
	toolLabWait(lab, opened.SessionID, opened.PageID, "selector_visible", "#shadow-action")
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := lab.invoke("browserUploadFile", map[string]any{
		"session_id": opened.SessionID, "page_id": opened.PageID,
		"ref": attachment.Ref, "revision": snapshot.Revision, "path": outside,
	}, new(UploadFileResponse)); err == nil || !strings.Contains(err.Error(), "upload_path_blocked") {
		t.Fatalf("outside upload: %v", err)
	}
	link := filepath.Join(uploadDir, "outside-link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err := lab.invoke("browserUploadFile", map[string]any{
		"session_id": opened.SessionID, "page_id": opened.PageID,
		"ref": attachment.Ref, "revision": snapshot.Revision, "path": link,
	}, new(UploadFileResponse)); err == nil || !strings.Contains(err.Error(), "upload_path_blocked") {
		t.Fatalf("symlink upload: %v", err)
	}
	big := filepath.Join(uploadDir, "large.bin")
	if err := os.WriteFile(big, make([]byte, maxUploadFileBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	if err := lab.invoke("browserUploadFile", map[string]any{
		"session_id": opened.SessionID, "page_id": opened.PageID,
		"ref": attachment.Ref, "revision": snapshot.Revision, "path": big,
	}, new(UploadFileResponse)); err == nil || !strings.Contains(err.Error(), "upload_file_invalid") {
		t.Fatalf("large upload: %v", err)
	}
	if err := lab.invoke("browserUploadFile", map[string]any{
		"session_id": opened.SessionID, "page_id": opened.PageID,
		"ref":      toolLabElement(t, snapshot, "button", "Frame action").Ref,
		"revision": snapshot.Revision, "path": uploadPath,
	}, new(UploadFileResponse)); err == nil || !strings.Contains(err.Error(), "invalid_upload_target") {
		t.Fatalf("wrong upload target: %v", err)
	}
	shadowAction := toolLabElement(t, snapshot, "button", "Shadow action")
	frameAction := toolLabElement(t, snapshot, "button", "Frame action")
	toolLabCall[EvaluateResponse](lab, "browserEvaluate", map[string]any{
		"session_id": opened.SessionID, "page_id": opened.PageID,
		"code": "document.querySelector('#shadow-host').remove(); document.querySelector('#same-origin-frame').src = '/detail.html'",
	})
	toolLabWait(lab, opened.SessionID, opened.PageID, "selector_hidden", "#shadow-action")
	toolLabWait(lab, opened.SessionID, opened.PageID, "selector_hidden", "#frame-action")
	fresh := toolLabCall[SnapshotResponse](lab, "browserSnapshot", map[string]any{
		"session_id": opened.SessionID, "page_id": opened.PageID, "max_elements": 100,
	})
	for _, old := range []Element{shadowAction, frameAction} {
		if err := lab.invoke("browserClick", map[string]any{
			"session_id": opened.SessionID, "page_id": opened.PageID,
			"ref": old.Ref, "revision": snapshot.Revision,
		}, new(ClickResponse)); err == nil || !strings.Contains(err.Error(), "reference") {
			t.Fatalf("removed embedded ref %s remains usable: %v", old.Ref, err)
		}
	}
	for _, el := range fresh.Elements {
		if el.Ref == shadowAction.Ref || el.Ref == frameAction.Ref {
			t.Fatalf("stale embedded ref remains in snapshot: %+v", el)
		}
	}
	cross := httptest.NewServer(toollab.Handler())
	defer cross.Close()
	crossURL := cross.URL + "/cross.html"
	quotedURL, err := json.Marshal(crossURL)
	if err != nil {
		t.Fatal(err)
	}
	toolLabCall[EvaluateResponse](lab, "browserEvaluate", map[string]any{
		"session_id": opened.SessionID, "page_id": opened.PageID,
		"code": "new Promise(resolve => { const frame = document.querySelector('#cross-origin-frame'); frame.onload = () => resolve(true); frame.src = " + string(quotedURL) + " })",
	})
	crossSnapshot := toolLabCall[SnapshotResponse](lab, "browserSnapshot", map[string]any{
		"session_id": opened.SessionID, "page_id": opened.PageID, "max_elements": 100,
	})
	if frame := toolLabElement(t, crossSnapshot, "frame", "External checkout"); frame.FrameURL != crossURL {
		t.Fatalf("cross-origin iframe URL missing: %+v", frame)
	}
	external := toolLabElement(t, crossSnapshot, "button", "External action")
	if !strings.HasPrefix(external.Ref, "f") || external.FrameURL != crossURL {
		t.Fatalf("跨域控件缺少独立 frame 引用: %+v", external)
	}
	toolLabClick(lab, opened.SessionID, opened.PageID, crossSnapshot, "button", "External action")
	result := toolLabCall[SnapshotResponse](lab, "browserSnapshot", map[string]any{"session_id": opened.SessionID, "page_id": opened.PageID})
	if !strings.Contains(result.Text, "External clicked") {
		t.Fatalf("跨域点击未改变子文档: %s", result.Text)
	}
}
