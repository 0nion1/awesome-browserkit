//go:build browser_smoke

package browserkit

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRealBrowserFrameMetadataOnly(t *testing.T) {
	const content = `<input aria-label="Metadata input" value="SECRET_INPUT">
<select aria-label="Metadata select"><option value="SECRET_OPTION">SECRET_LABEL</option></select>
<textarea aria-label="Metadata notes">SECRET_NOTES</textarea>
<textarea id="unlabelled-notes">SECRET_UNLABELLED_NOTES</textarea>
<select id="unlabelled-select"><option value="SECRET_UNLABELLED_VALUE">SECRET_UNLABELLED_OPTION</option></select>
<div contenteditable id="unlabelled-editor">SECRET_UNLABELLED_EDITOR</div>
<input id="unlabelled-submit" type="submit" value="SECRET_SUBMIT"><p>SECRET_TEXT</p>`
	for _, hostStyle := range []string{"opacity:0.01", "position:absolute;left:100000px", "width:0;height:0"} {
		t.Run(hostStyle, func(t *testing.T) {
			for _, origin := range []string{"same-origin", "cross-origin", "OOPIF", "same-origin-through-OOPIF"} {
				t.Run(origin, func(t *testing.T) {
					sameOrigin := origin == "same-origin"
					wrapper := `<!doctype html><iframe srcdoc="` + html.EscapeString(content) + `" style="width:600px;height:200px"></iframe>`
					var topURL string
					frameServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if origin == "same-origin-through-OOPIF" {
							fmt.Fprintf(w, `<!doctype html><iframe src="%s/leaf" style="width:600px;height:200px"></iframe>`, topURL)
							return
						}
						fmt.Fprint(w, wrapper)
					}))
					if strings.Contains(origin, "OOPIF") {
						listener, err := listenCrossSite("127.0.0.2:0")
						if err != nil {
							t.Fatal(err)
						}
						_ = frameServer.Listener.Close()
						frameServer.Listener = listener
					}
					frameServer.Start()
					defer frameServer.Close()
					root := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/leaf" {
							fmt.Fprint(w, "<!doctype html>"+content)
							return
						}
						if r.URL.Path == "/wrapper" {
							fmt.Fprint(w, wrapper)
							return
						}
						frameURL := frameServer.URL
						if sameOrigin {
							frameURL = "/wrapper"
						}
						fmt.Fprintf(w, `<!doctype html><iframe src="%s" style="width:700px;height:300px;%s"></iframe>`, frameURL, hostStyle)
					}))
					topURL = root.URL
					defer root.Close()
					manager, err := NewManager(Config{AllowPrivateNetwork: true, AllowEvaluate: true})
					if err != nil {
						t.Fatal(err)
					}
					defer manager.Close(context.Background())
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					defer cancel()
					opened, err := manager.Open(ctx, OpenRequest{URL: root.URL})
					if err != nil {
						t.Fatal(err)
					}
					if sameOrigin {
						_, err = manager.Evaluate(ctx, EvaluateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Code: `document.querySelector('iframe').contentDocument.querySelector('iframe').contentDocument.querySelector('input').focus({preventScroll:true});scrollTo(0,0)`})
						if err != nil {
							t.Fatal(err)
						}
					}
					snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
					if err != nil {
						t.Fatal(err)
					}
					checkMetadataSnapshot(t, snapshot, sameOrigin)
					if !sameOrigin {
						for _, el := range snapshot.Elements {
							if strings.HasPrefix(el.Name, "Metadata ") {
								t.Fatalf("cross-origin hidden control was enumerated: %+v", el)
							}
						}
					}
					_, page, err := manager.get(ctx, opened.SessionID, opened.PageID)
					if err != nil {
						t.Fatal(err)
					}
					internal, err := page.page.Snapshot(ctx, SnapshotOptions{})
					if err != nil {
						t.Fatal(err)
					}
					if internal.ActiveElementValue != "" {
						t.Fatalf("metadata-only active value leaked: %q", internal.ActiveElementValue)
					}
					// 相同全局分页预算必须覆盖元数据，不能因隐藏内容重新起算。
					if sameOrigin {
						seen := map[string]bool{}
						for offset := 0; ; {
							part, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID, MaxElements: 1, Offset: offset})
							if err != nil {
								t.Fatal(err)
							}
							for _, element := range part.Elements {
								if seen[element.Ref] {
									t.Fatalf("duplicate metadata ref %s", element.Ref)
								}
								seen[element.Ref] = true
							}
							checkMetadataSnapshot(t, part, false)
							if part.NextOffset == 0 {
								break
							}
							offset = part.NextOffset
						}
						if len(seen) != len(snapshot.Elements) {
							t.Fatalf("metadata pagination %d != %d", len(seen), len(snapshot.Elements))
						}
					}
				})
			}
		})
	}
}

func checkMetadataSnapshot(t *testing.T, snapshot SnapshotResponse, requireMetadata bool) {
	t.Helper()
	encoded, _ := json.Marshal(snapshot)
	if strings.Contains(string(encoded), "SECRET_") {
		t.Fatalf("metadata-only content leaked: %s", encoded)
	}
	found := map[string]bool{}
	for _, element := range snapshot.Elements {
		if strings.HasPrefix(element.Name, "Metadata ") {
			if element.Ref == "" || element.Value != "" || len(element.Options) > 0 || element.OptionsTruncated {
				t.Fatalf("invalid metadata element: %+v", element)
			}
			found[element.Name] = true
		}
	}
	if requireMetadata {
		for _, id := range []string{"unlabelled-notes", "unlabelled-select", "unlabelled-editor", "unlabelled-submit"} {
			foundID := false
			for _, el := range snapshot.Elements {
				if el.ID == id {
					foundID = true
					if el.Ref == "" || el.Name != id || el.Locator.Name != id {
						t.Fatalf("unlabelled metadata not preserved: %+v", el)
					}
				}
			}
			if !foundID {
				t.Fatalf("missing metadata id %s", id)
			}
		}
		if len(found) != 3 {
			t.Fatalf("missing metadata controls: %+v", snapshot.Elements)
		}
		metadataFrames := 0
		for _, coverage := range snapshot.FrameCoverage {
			if coverage.Status == "partial" && coverage.Reason == "metadata_only" {
				metadataFrames++
			}
		}
		if metadataFrames != 2 {
			t.Fatalf("nested metadata coverage missing: %+v", snapshot.FrameCoverage)
		}
	}
}

func TestRealBrowserScrolledFrameKeepsValues(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/frame" {
			fmt.Fprintf(w, `<!doctype html><input aria-label="%s" value="%s"><p>%s_TEXT</p>`, r.URL.RawQuery, r.URL.RawQuery, r.URL.RawQuery)
			return
		}
		fmt.Fprint(w, `<!doctype html><body style="margin:0"><iframe src="/frame?SCROLLED" style="width:400px;height:200px"></iframe><iframe src="/frame?NEGATIVE" style="position:absolute;top:-10000px;width:400px;height:200px"></iframe><div style="height:6000px"></div>`)
	}))
	defer server.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true, AllowEvaluate: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	opened, err := manager.Open(ctx, OpenRequest{URL: server.URL, Viewport: Viewport{Width: 1000, Height: 800}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.Evaluate(ctx, EvaluateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Code: `scrollTo(0,4000)`})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, el := range snapshot.Elements {
		if el.Ref != "" {
			values[el.Name] = el.Value
		}
	}
	if value, ok := values["SCROLLED"]; !ok || value != "SCROLLED" {
		t.Fatalf("scrolled frame lost metadata/value: %+v", snapshot.Elements)
	}
	if value, ok := values["NEGATIVE"]; !ok || value != "" {
		t.Fatalf("negative document position leaked value or lost metadata: %+v", snapshot.Elements)
	}
	if strings.Contains(snapshot.Text, "SCROLLED_TEXT") || strings.Contains(snapshot.Text, "NEGATIVE_TEXT") {
		t.Fatalf("offscreen text leaked: %s", snapshot.Text)
	}
	foundTextOnly := false
	for _, coverage := range snapshot.FrameCoverage {
		if coverage.Reason == "text_not_visible" {
			foundTextOnly = true
		}
	}
	if !foundTextOnly {
		t.Fatalf("text-only omission missing: %+v", snapshot.FrameCoverage)
	}
}
