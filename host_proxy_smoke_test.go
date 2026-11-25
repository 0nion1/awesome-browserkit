//go:build browser_smoke

package browserkit

import (
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRealHostAuthenticatedProxyAndCertificates(t *testing.T) {
	var targetCalls, proxyCalls, leaks atomic.Int64
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetCalls.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("Proxy-Authorization") != "" {
			leaks.Add(1)
		}
		if r.URL.Path == "/auth" {
			w.Header().Set("WWW-Authenticate", `Basic realm="target"`)
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, `<title>proxied HTTPS</title><input id="value">`)
	}))
	defer target.Close()
	defaultSession, err := NewSession(context.Background(), SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defaultPage, err := defaultSession.NewPage(context.Background(), PageOptions{})
	if err != nil {
		defaultSession.Close()
		t.Fatal(err)
	}
	if _, err := defaultPage.Navigate(context.Background(), target.URL); err == nil {
		defaultSession.Close()
		t.Fatal("默认配置不应忽略自签名证书")
	}
	defaultSession.Close()
	destination, _ := url.Parse(target.URL)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalls.Add(1)
		if r.Header.Get("Proxy-Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("sdk:secret")) {
			w.Header().Set("Proxy-Authenticate", `Basic realm="fixture"`)
			w.WriteHeader(407)
			return
		}
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", 400)
			return
		}
		upstream, err := net.Dial("tcp", destination.Host)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		client, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			upstream.Close()
			return
		}
		buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		buffer.Flush()
		go func() { defer client.Close(); defer upstream.Close(); io.Copy(upstream, buffer) }()
		go func() { defer client.Close(); defer upstream.Close(); io.Copy(client, upstream) }()
	}))
	defer proxy.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	configured := strings.Replace(proxy.URL, "://", "://sdk:secret@", 1)
	sessionValue, err := NewSession(ctx, SessionConfig{Proxy: configured, ProxyBypassList: []string{"<-loopback>"}, IgnoreCertificateErrors: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sessionValue.Close()
	for _, address := range []string{target.URL, "https://mismatched.test/", target.URL + "/auth"} {
		page, err := sessionValue.NewPage(ctx, PageOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := page.Navigate(ctx, address); err != nil && !strings.HasSuffix(address, "/auth") {
			t.Fatalf("代理 HTTPS %s: %v", address, err)
		}
	}
	if proxyCalls.Load() < 3 || targetCalls.Load() < 3 || leaks.Load() != 0 {
		t.Fatalf("proxy=%d target=%d credential_leaks=%d", proxyCalls.Load(), targetCalls.Load(), leaks.Load())
	}
}

func TestRealOperationCancellationPreservesSession(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `<title>alive</title>`) }))
	defer site.Close()
	sessionValue, err := NewSession(context.Background(), SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer sessionValue.Close()
	ctx, cancel := context.WithCancel(context.Background())
	page, err := sessionValue.NewPage(ctx, PageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := page.Navigate(ctx, site.URL); err != nil {
		t.Fatal(err)
	}
	_, err = page.Evaluate(ctx, `document.cookie="retained=yes"`, EvaluateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	operationCtx, operationCancel := context.WithTimeout(ctx, 40*time.Millisecond)
	_, timeoutErr := page.Evaluate(operationCtx, `new Promise(resolve => setTimeout(() => resolve(true), 250))`, EvaluateOptions{AwaitPromise: true})
	operationCancel()
	if timeoutErr == nil {
		t.Fatal("有副作用的异步操作没有遵循单次 deadline")
	}
	cancel()
	if _, err := page.Evaluate(ctx, `1`, EvaluateOptions{}); err == nil {
		t.Fatal("取消操作没有退出")
	}
	if sessionValue.Closed() {
		t.Fatal("单次取消关闭了会话")
	}
	result, err := page.Evaluate(context.Background(), `document.cookie`, EvaluateOptions{})
	if err != nil || !strings.Contains(string(result.Value), "retained=yes") {
		t.Fatalf("Cookie 现场丢失: %s %v", result.Value, err)
	}
}
