//go:build browser_smoke

package browserkit

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRealBrowserFrameBaseline(t *testing.T) {
	leaf := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!doctype html><body style="margin:0"><div id="host"></div><script>
  const shadow = host.attachShadow({mode:'open'}); shadow.innerHTML = '<label>Password <input type="password" aria-label="Nested password"></label><button id="apply">Nested apply</button><button onclick="this.replaceWith(this.cloneNode(true))">Replace node</button><p id="out">Nested ready</p>';
  shadow.querySelector('button').onclick = () => shadow.querySelector('p').textContent = 'Nested clicked: ' + shadow.querySelector('input').value;
  </script>`)
	}))
	listener, err := net.Listen("tcp", "127.0.0.3:0")
	if err != nil {
		t.Fatal(err)
	}
	leaf.Listener = listener
	leaf.Start()
	defer leaf.Close()
	middle := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<!doctype html><body style="margin:0"><iframe src="%s" style="width:500px;height:200px;border:0"></iframe>`, leaf.URL)
	}))
	listener, err = net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Fatal(err)
	}
	middle.Listener = listener
	middle.Start()
	defer middle.Close()
	root := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/leaf" {
			fmt.Fprintf(w, `<!doctype html><button onclick="this.textContent='Changed %s'">Business %s</button>`, r.URL.RawQuery, r.URL.RawQuery)
			return
		}
		fmt.Fprintf(w, `<!doctype html><body style="margin:0"><iframe src="%s" style="width:600px;height:260px;border:0"></iframe>`, middle.URL)
		for i := 0; i < 8; i++ {
			fmt.Fprintf(w, `<iframe src="/leaf?%d" style="width:180px;height:60px;border:0"></iframe>`, i)
		}
	}))
	defer root.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true, AllowEvaluate: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	opened, err := manager.Open(ctx, OpenRequest{URL: root.URL, Viewport: Viewport{Width: 1200, Height: 900}})
	if err != nil {
		t.Fatal(err)
	}
	snapshotCtx, snapshotCancel := context.WithTimeout(ctx, 5*time.Second)
	snapshot, err := manager.Snapshot(snapshotCtx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID, MaxElements: 100})
	snapshotCancel()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.FrameCoverage) != 11 {
		t.Fatalf("frame coverage count=%d: %+v", len(snapshot.FrameCoverage), snapshot.FrameCoverage)
	}
	if !strings.Contains(snapshot.Text, "Nested ready") {
		t.Fatalf("nested shadow text missing: %s coverage=%+v", snapshot.Text, snapshot.FrameCoverage)
	}
	password := smokeElementRef(t, snapshot, "textbox", "Nested password")
	button := smokeElementRef(t, snapshot, "button", "Nested apply")
	if _, err = manager.Type(ctx, TypeRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: password, Revision: snapshot.Revision, Text: "secret"}); err != nil {
		t.Fatal(err)
	}
	if _, err = manager.Click(ctx, ClickRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: button, Revision: snapshot.Revision}); err != nil {
		t.Fatal(err)
	}
	snapshot, err = manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID, MaxElements: 100})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(snapshot.Text, "Nested clicked: secret") {
		t.Fatalf("nested click missing: %s", snapshot.Text)
	}
	// Pagination covers all documents using one Tab-wide element offset.
	seen := map[string]bool{}
	offset := 0
	for n := 0; n < 20; n++ {
		part, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID, MaxElements: 3, Offset: offset})
		if err != nil {
			t.Fatal(err)
		}
		for _, el := range part.Elements {
			if seen[el.Ref] {
				t.Fatalf("duplicate paged ref %s", el.Ref)
			}
			seen[el.Ref] = true
		}
		if part.NextOffset == 0 {
			break
		}
		offset = part.NextOffset
		if n == 19 {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != len(snapshot.Elements) {
		t.Fatalf("pagination covered %d of %d elements", len(seen), len(snapshot.Elements))
	}
	limited, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID, MaxElements: 1, MaxChars: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(limited.FrameCoverage) != 11 || !limited.Truncated || limited.NextOffset == 0 {
		t.Fatalf("budget did not report uncovered documents: %+v", limited)
	}
	for _, coverage := range limited.FrameCoverage {
		if coverage.ShadowDOM != "open_only" {
			t.Fatalf("shadow scope missing: %+v", coverage)
		}
	}
	// A cloned node with copied attributes must never resurrect an issued ref.
	replace := smokeElementRef(t, snapshot, "button", "Replace node")
	changed, err := manager.Click(ctx, ClickRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: replace, Revision: snapshot.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if changed.Snapshot == nil {
		t.Fatal("replacement snapshot missing")
	}
	fresh := smokeElementRef(t, *changed.Snapshot, "button", "Replace node")
	if fresh == replace {
		t.Fatal("cloned node reused old ref")
	}
	if _, err = manager.Click(ctx, ClickRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: replace, Revision: snapshot.Revision}); err == nil {
		t.Fatal("stale cloned ref accepted")
	}
}

func TestRealBrowserAffineFrameClicks(t *testing.T) {
	for _, transform := range []string{"scale(0.6)", "scaleX(-1)", "rotate(20deg)", "rotate(180deg)", "skewX(15deg)"} {
		t.Run(transform, func(t *testing.T) {
			inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, `<!doctype html><body style="margin:0"><button style="position:absolute;left:100px;top:70px;width:100px;height:35px" onclick="this.textContent='Applied'">Apply</button>`)
			}))
			defer inner.Close()
			outer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `<!doctype html><iframe src="%s" style="position:absolute;left:200px;top:200px;width:400px;height:240px;border:0;transform:%s;transform-origin:center"></iframe>`, inner.URL, transform)
			}))
			defer outer.Close()
			manager, err := NewManager(Config{AllowPrivateNetwork: true})
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Close(context.Background())
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			opened, err := manager.Open(ctx, OpenRequest{URL: outer.URL, Viewport: Viewport{Width: 1000, Height: 800}})
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
			if err != nil {
				t.Fatal(err)
			}
			ref := smokeElementRef(t, snapshot, "button", "Apply")
			clicked, err := manager.Click(ctx, ClickRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: ref, Revision: snapshot.Revision})
			if err != nil {
				t.Fatal(err)
			}
			if clicked.Snapshot == nil || !strings.Contains(clicked.Snapshot.Text, "Applied") {
				t.Fatalf("click failed: %+v", clicked)
			}
		})
	}
}

func TestRealBrowserClosedShadowScopeAndClipping(t *testing.T) {
	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!doctype html><body style="margin:0"><button style="position:absolute;left:10px;top:10px;width:100px;height:30px" onclick="this.textContent='Applied'">Clipped button</button>`)
	}))
	defer inner.Close()
	outer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<!doctype html><div id="closed"></div><script>document.getElementById('closed').attachShadow({mode:'closed'}).innerHTML='<button>Closed secret</button>';document.getElementById('closed').dataset.created='yes';</script><iframe src="%s" style="width:400px;height:240px;border:0;clip-path:inset(0 0 0 200px)"></iframe>`, inner.URL)
	}))
	defer outer.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true, AllowEvaluate: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	opened, err := manager.Open(ctx, OpenRequest{URL: outer.URL})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	created, err := manager.Evaluate(ctx, EvaluateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Code: `document.getElementById('closed').dataset.created === 'yes' && document.getElementById('closed').shadowRoot === null`})
	if err != nil || string(created.Value) != "true" {
		t.Fatalf("closed shadow fixture was not established: %s %v", created.Value, err)
	}
	if strings.Contains(snapshot.Text, "Closed secret") {
		t.Fatal("closed shadow reported as captured")
	}
	for _, el := range snapshot.Elements {
		if el.Name == "Closed secret" {
			t.Fatal("closed shadow unexpectedly has ref")
		}
	}
	if len(snapshot.FrameCoverage) == 0 || snapshot.FrameCoverage[0].ShadowDOM != "open_only" {
		t.Fatal("closed shadow scope not explicit")
	}
	ref := smokeElementRef(t, snapshot, "button", "Clipped button")
	if _, err := manager.Click(ctx, ClickRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: ref, Revision: snapshot.Revision}); err == nil || !strings.Contains(err.Error(), "裁剪") {
		t.Fatalf("clipped click should be rejected: %v", err)
	}
}
