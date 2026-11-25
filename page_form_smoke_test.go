//go:build browser_smoke

package browserkit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRealBrowserNativeSelectAndFileChooser(t *testing.T) {
	for _, tc := range []struct {
		name   string
		nested bool
	}{{"top document", false}, {"OOPIF open shadow", true}} {
		t.Run(tc.name, func(t *testing.T) { testNativeSelectAndFileChooser(t, tc.nested) })
	}
}

func testNativeSelectAndFileChooser(t *testing.T, nested bool) {
	t.Helper()
	const form = `
<label>Single choice<select id="single"><option value="">Blank option</option><option value="x" selected>X</option></select></label>
<label>Multiple choices<select id="multiple" multiple><option value="a">Alpha</option><option value="b">Beta</option><option value="bad" disabled>Disabled</option></select></label>
<input type="file" multiple aria-label="Direct upload" id="directUpload" onchange="show(this, 'direct')">
<input type="file" multiple hidden id="chooserUpload" onchange="show(this, 'chooser')">
<button id="chooser" onclick="this.getRootNode().querySelector('#chooserUpload').click()">Open chooser</button>
<output id="out"></output>`
	const show = `<script>function show(input, kind) { input.getRootNode().querySelector('#out').textContent = kind + ':' + Array.from(input.files, f => f.name + '=' + f.size).join(','); }</script>`
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		body := form
		if nested {
			body = `<div id="host"></div><template id="form">` + form + `</template><script>document.getElementById('host').attachShadow({mode: 'open'}).append(document.getElementById('form').content);</script>`
		}
		_, _ = w.Write([]byte(`<!doctype html><title>Form fixture</title>` + body + show))
	}))
	if nested {
		listener, err := listenCrossSite("127.0.0.2:0")
		if err != nil {
			t.Fatal(err)
		}
		_ = server.Listener.Close()
		server.Listener = listener
	}
	server.Start()
	t.Cleanup(server.Close)
	url := server.URL
	if nested {
		top := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`<!doctype html><iframe src="` + server.URL + `" width="750" height="420"></iframe>`))
		}))
		t.Cleanup(top.Close)
		url = top.URL
	}
	uploadDir := t.TempDir()
	first := filepath.Join(uploadDir, "first.txt")
	second := filepath.Join(uploadDir, "second.txt")
	for path, content := range map[string]string{first: "one", second: "four"} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	large := filepath.Join(uploadDir, "oversize")
	f, err := os.Create(large)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(f.Truncate(maxUploadFileBytes+1), f.Close()); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(Config{AllowPrivateNetwork: true, UploadDir: uploadDir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	opened, err := manager.Open(ctx, OpenRequest{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := func() SnapshotResponse {
		t.Helper()
		view, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
		if err != nil {
			t.Fatal(err)
		}
		return view
	}
	refWithID := func(view SnapshotResponse, id string) string {
		t.Helper()
		for _, element := range view.Elements {
			if element.ID == id {
				return element.Ref
			}
		}
		t.Fatalf("snapshot missing %q: %+v", id, view.Elements)
		return ""
	}
	view := snapshot()
	selectReq := SelectOptionRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: refWithID(view, "single"), Revision: view.Revision}
	if _, err := manager.SelectOption(ctx, selectReq); err == nil {
		t.Fatal("missing selection accepted")
	}
	for _, want := range [][]string{{""}, {}} {
		selectReq.Values = &want
		response, err := manager.SelectOption(ctx, selectReq)
		if err != nil || response.Values == nil || !slices.Equal(response.Values, want) {
			t.Fatalf("selection %q: %+v, %v", want, response, err)
		}
	}
	view = snapshot()
	batch, err := manager.Act(ctx, ActRequest{SessionID: opened.SessionID, PageID: opened.PageID, Revision: view.Revision, Steps: []ActStep{{Op: "select", Ref: refWithID(view, "multiple"), Labels: &[]string{"Beta", "Alpha"}}}})
	if err != nil || batch.FailedStep != 0 || batch.Completed != 1 || batch.Steps[0].Values == nil || !slices.Equal(*batch.Steps[0].Values, []string{"a", "b"}) {
		t.Fatalf("multiple-select batch: %+v, %v", batch, err)
	}
	view = snapshot()
	upload := UploadFileRequest{SessionID: opened.SessionID, PageID: opened.PageID, Revision: view.Revision, Ref: refWithID(view, "directUpload"), Paths: []string{first, large}}
	if _, err := manager.UploadFile(ctx, upload); err == nil || !strings.Contains(err.Error(), "upload_file_invalid") {
		t.Fatalf("oversize file accepted in batch: %v", err)
	}
	if strings.Contains(snapshot().Text, "direct:first.txt") {
		t.Fatal("invalid batch changed the input")
	}
	upload.Paths = []string{first, second}
	for _, mode := range []struct{ id, text string }{{"directUpload", "direct:first.txt=3,second.txt=4"}, {"chooser", "chooser:first.txt=3,second.txt=4"}} {
		view = snapshot()
		upload.Revision, upload.Ref, upload.Chooser = view.Revision, refWithID(view, mode.id), mode.id == "chooser"
		result, err := manager.UploadFile(ctx, upload)
		if err != nil || !result.Uploaded || result.Bytes != 7 || !slices.Equal(result.Files, []UploadedFile{{FileName: "first.txt", Bytes: 3}, {FileName: "second.txt", Bytes: 4}}) {
			t.Fatalf("upload %s: %+v, %v", mode.id, result, err)
		}
		if view := snapshot(); !strings.Contains(view.Text, mode.text) {
			t.Fatalf("upload %s absent in browser: %s", mode.id, view.Text)
		}
	}
}
