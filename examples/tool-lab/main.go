// browserkit tool lab 启动可手动调用浏览器函数的本地靶场。
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/zeoxisca/awesome-browserkit/internal/toollab"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8879", "靶场监听地址")
	flag.Parse()
	server := &http.Server{Addr: *addr, Handler: toollab.Handler(), ReadHeaderTimeout: 5 * time.Second}
	log.Printf("BrowserKit 工具靶场：http://%s/", *addr)
	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
