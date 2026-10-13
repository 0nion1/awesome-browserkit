//go:build browser_smoke

package browserkit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-rod/rod/lib/launcher"
)

func TestRealBrowserOwnsAndRemovesLocalProfile(t *testing.T) {
	if _, ok := launcher.LookPath(); !ok {
		t.Skip("本机没有 Chrome")
	}
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	sessionValue, err := NewSession(context.Background(), SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	session := sessionValue.(*rodSession)
	dir := session.launcher.Get("user-data-dir")
	if filepath.Dir(dir) != root || !session.cleanup {
		t.Fatalf("浏览器未使用独立 profile：%q", dir)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("运行中的 profile 不存在：%v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("会话结束后 profile 未清理：%v", err)
	}
}

func TestRealBrowserProfileIdentityMatchesRequests(t *testing.T) {
	if _, ok := launcher.LookPath(); !ok {
		t.Skip("本机没有 Chrome")
	}
	headers := make(chan http.Header, 1)
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case headers <- r.Header.Clone():
		default:
		}
		_, _ = w.Write([]byte("<title>Profile identity</title>"))
	}))
	defer site.Close()
	ctx := context.Background()
	sessionValue, err := NewSession(ctx, SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer sessionValue.Close()
	page, err := sessionValue.NewPage(ctx, PageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := page.Navigate(ctx, site.URL); err != nil {
		t.Fatal(err)
	}
	result, err := page.Evaluate(ctx, `({ua:navigator.userAgent, webdriver:navigator.webdriver, language:navigator.language, platform:navigator.platform})`, EvaluateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var identity struct {
		UA        string `json:"ua"`
		Webdriver bool   `json:"webdriver"`
		Language  string `json:"language"`
		Platform  string `json:"platform"`
	}
	if err := json.Unmarshal(result.Value, &identity); err != nil {
		t.Fatal(err)
	}
	requestHeaders := <-headers
	if identity.UA == "" || strings.Contains(identity.UA, "HeadlessChrome") || requestHeaders.Get("User-Agent") != identity.UA || identity.Webdriver || identity.Language == "" || identity.Platform == "" || !strings.HasPrefix(requestHeaders.Get("Accept-Language"), identity.Language) {
		t.Fatalf("浏览器 profile 身份与请求不一致：identity=%+v headers=%v", identity, requestHeaders)
	}
	if !strings.Contains(requestHeaders.Get("Sec-Ch-Ua"), "v=\""+strings.Split(strings.Split(identity.UA, "Chrome/")[1], ".")[0]+"\"") {
		t.Fatalf("Client Hints 主版本与 UA 不一致：%q / %q", requestHeaders.Get("Sec-Ch-Ua"), identity.UA)
	}
}
