//go:build browser_smoke

package browserkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

func TestAcceptanceFrameTypePreservesQuerySelectorLiteral(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/frame" {
			fmt.Fprint(w, "<!doctype html><textarea aria-label='Payload' id='payload'></textarea>")
			return
		}
		fmt.Fprint(w, "<!doctype html><iframe src='/frame' width='600' height='300'></iframe>")
	}))
	defer site.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true, AllowEvaluate: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	opened, err := manager.Open(ctx, OpenRequest{URL: site.URL})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	ref := smokeElementRef(t, snapshot, "textbox", "Payload")
	want := "document.querySelector('input[name=q]')"
	_, err = manager.Type(ctx, TypeRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: ref, Revision: snapshot.Revision, Text: want})
	if err != nil {
		t.Fatal(err)
	}
	actual, err := manager.Evaluate(ctx, EvaluateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Code: "document.querySelector('iframe').contentDocument.getElementById('payload').value"})
	if err != nil {
		t.Fatal(err)
	}
	var got string
	if err := json.Unmarshal(actual.Value, &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("Type returned success but changed payload: got %q, want %q", got, want)
	}
}

func TestAcceptanceDownloadStartDoesNotAdoptOlderSameFrameDownload(t *testing.T) {
	releaseA := make(chan struct{})
	releaseB := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseB) })
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/a" {
			select {
			case <-releaseA:
			case <-r.Context().Done():
				return
			}
			fmt.Fprint(w, "A")
			return
		}
		if r.URL.Path == "/b" {
			close(releaseA)
			select {
			case <-releaseB:
			case <-r.Context().Done():
				return
			}
			fmt.Fprint(w, "B")
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<!doctype html><button onclick=\"start('/a','a.txt')\">Start A</button><button onclick=\"start('/b','b.txt')\">Start B</button><script>function start(url,name){fetch(url).then(r=>r.blob()).then(b=>{let a=document.createElement('a');a.href=URL.createObjectURL(b);a.download=name;a.click();});}</script>")
	}))
	defer site.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true, DownloadDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	opened, err := manager.Open(ctx, OpenRequest{URL: site.URL})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	first, firstErr := manager.Download(ctx, DownloadRequest{SessionID: opened.SessionID, PageID: opened.PageID, Action: "start", Ref: smokeElementRef(t, snapshot, "button", "Start A"), Revision: snapshot.Revision, TimeoutMS: 300})
	var firstFailure *Error
	if !errors.As(firstErr, &firstFailure) || firstFailure.Kind != "download_unconfirmed" || !first.Triggered || first.DownloadID != "" || first.Path != "" {
		t.Fatalf("unconfirmed first start=%+v err=%v", first, firstErr)
	}
	second, secondErr := manager.Download(ctx, DownloadRequest{SessionID: opened.SessionID, PageID: opened.PageID, Action: "start", Ref: smokeElementRef(t, snapshot, "button", "Start B"), Revision: snapshot.Revision, TimeoutMS: 3000})
	var secondFailure *Error
	if !errors.As(secondErr, &secondFailure) || secondFailure.Kind != "download_unconfirmed" || !second.Triggered || second.DownloadID != "" || second.Path != "" || second.FileName != "" {
		t.Fatalf("unconfirmed second start adopted a file: response=%+v err=%v", second, secondErr)
	}
	releaseOnce.Do(func() { close(releaseB) })
	deadline := time.Now().Add(5 * time.Second)
	var listed DownloadResponse
	for time.Now().Before(deadline) {
		listed, err = manager.Download(ctx, DownloadRequest{SessionID: opened.SessionID, Action: "list"})
		if err != nil {
			t.Fatal(err)
		}
		if len(listed.Downloads) == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(listed.Downloads) != 2 {
		t.Fatalf("actual downloads missing: %+v", listed)
	}
	files := map[string]string{}
	for _, download := range listed.Downloads {
		completed, err := manager.Download(ctx, DownloadRequest{SessionID: opened.SessionID, Action: "wait", DownloadID: download.ID, TimeoutMS: 5000})
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(completed.Path)
		if err != nil {
			t.Fatal(err)
		}
		files[completed.FileName] = string(data)
	}
	if len(files) != 2 || files["a.txt"] != "A" || files["b.txt"] != "B" {
		t.Fatalf("actual download files=%v", files)
	}
}

func TestAcceptanceSameOriginNumberRejectsRangeWithoutMutation(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/frame" {
			fmt.Fprint(w, "<!doctype html><input type='number' min='1' max='10' value='3' aria-label='Count' id='count'>")
			return
		}
		fmt.Fprint(w, "<!doctype html><iframe src='/frame' width='600' height='300'></iframe>")
	}))
	defer site.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true, AllowEvaluate: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	opened, err := manager.Open(ctx, OpenRequest{URL: site.URL})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	ref := smokeElementRef(t, snapshot, "spinbutton", "Count")
	response, typeErr := manager.Type(ctx, TypeRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: ref, Revision: snapshot.Revision, Text: "99"})
	actual, err := manager.Evaluate(ctx, EvaluateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Code: "document.querySelector('iframe').contentDocument.getElementById('count').value"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Type error=%v response=%+v value=%s", typeErr, response, actual.Value)
	if typeErr == nil || string(actual.Value) != "\"3\"" {
		t.Fatalf("same-origin number input lost range/rollback behavior: error=%v value=%s", typeErr, actual.Value)
	}
}
