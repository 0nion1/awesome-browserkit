// Package toollab 提供浏览器函数集成测试及手动调试共用的页面靶场。
package toollab

import (
	"embed"
	"io/fs"
	"net/http"
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
	mux.Handle("/", http.FileServer(http.FS(site)))
	return mux
}
