package browserkit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/cdp"
	"github.com/go-rod/rod/lib/proto"
)

type proxyStartupClient struct{ pageBindingCDPClient }

func (client *proxyStartupClient) Call(ctx context.Context, sessionID string, method string, params any) ([]byte, error) {
	if method == (proto.FetchEnable{}).ProtoReq() {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return client.pageBindingCDPClient.Call(ctx, sessionID, method, params)
}

func TestProxyPolicyStartupHonorsOperationDeadline(t *testing.T) {
	client := &proxyStartupClient{pageBindingCDPClient: pageBindingCDPClient{events: make(chan *cdp.Event)}}
	lifetime, closeLifetime := context.WithCancel(context.Background())
	defer closeLifetime()
	browser := rod.New().Client(client).Context(lifetime)
	if err := browser.Connect(); err != nil {
		t.Fatal(err)
	}
	startup, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	_, err := startBrowserNetworkPolicy(lifetime, startup, browser, nil, browserProxy{authenticated: true})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("startup=%v", err)
	}
	if lifetime.Err() != nil {
		t.Fatal("单次启动context取消了独立生命周期")
	}
}

func TestProxyCredentialsStayAtProxyBoundary(t *testing.T) {
	proxy, err := parseBrowserProxy("http://user:p%40ss@127.0.0.1:8888")
	if err != nil {
		t.Fatal(err)
	}
	if proxy.server != "http://127.0.0.1:8888" || proxy.password != "p@ss" {
		t.Fatalf("proxy=%+v", proxy)
	}
	for _, test := range []struct {
		source           proto.FetchAuthChallengeSource
		origin           string
		retried, allowed bool
	}{
		{proto.FetchAuthChallengeSourceProxy, proxy.server, false, true},
		{proto.FetchAuthChallengeSourceServer, proxy.server, false, false},
		{proto.FetchAuthChallengeSourceProxy, "http://other.test:8888", false, false},
		{proto.FetchAuthChallengeSourceProxy, proxy.server, true, false},
	} {
		response := proxy.challengeResponse(&proto.FetchAuthChallenge{Source: test.source, Origin: test.origin}, test.retried)
		if (response.Response == proto.FetchAuthChallengeResponseResponseProvideCredentials) != test.allowed {
			t.Fatalf("challenge=%+v response=%+v", test, response)
		}
		if !test.allowed && (response.Username != "" || response.Password != "") {
			t.Fatal("凭据泄漏到非代理 challenge")
		}
	}
	if _, err := parseBrowserProxy("socks5://user:password@localhost:1234"); err == nil {
		t.Fatal("Chromium 不支持的 SOCKS 认证应明确拒绝")
	}
}
