// Package toollab 提供浏览器函数集成测试及手动调试共用的页面靶场。
package toollab

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"strconv"
	"time"
)

//go:embed site/*
var siteFiles embed.FS

// Handler 返回自动化测试与手动调试共用的页面和 API。
func Handler() http.Handler {
	site, err := fs.Sub(siteFiles, "site")
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/items", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`["Alpha","Beta","Gamma"]`))
	})
	mux.HandleFunc("GET /api/snapshot/summary", func(w http.ResponseWriter, request *http.Request) {
		waitForSnapshotDelay(request)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ready", "total": 37, "owner": "Ada",
		})
	})
	mux.HandleFunc("GET /api/snapshot/details", func(w http.ResponseWriter, request *http.Request) {
		waitForSnapshotDelay(request)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": "A-17", "state": "queued"},
			{"id": "B-04", "state": "running"},
			{"id": "C-99", "state": "complete"},
		})
	})
	mux.HandleFunc("GET /api/snapshot/failure", func(w http.ResponseWriter, request *http.Request) {
		waitForSnapshotDelay(request)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"intentional snapshot lab failure"}`))
	})
	mux.Handle("/", http.FileServer(http.FS(site)))
	return mux
}

func waitForSnapshotDelay(request *http.Request) {
	delay, err := strconv.Atoi(request.URL.Query().Get("delay_ms"))
	if err != nil || delay < 0 {
		delay = 0
	}
	if delay > 2_000 {
		delay = 2_000
	}
	time.Sleep(time.Duration(delay) * time.Millisecond)
}
