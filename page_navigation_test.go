package browserkit

import (
	"context"
	"errors"
	"testing"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/cdp"
	"github.com/go-rod/rod/lib/proto"
)

type navigationCDPClient struct {
	pageBindingCDPClient
	navigation []byte
}

func (client *navigationCDPClient) Call(ctx context.Context, sessionID, method string, params any) ([]byte, error) {
	switch method {
	case "Page.navigate":
		return client.navigation, nil
	case "Target.getTargetInfo":
		return []byte(`{"targetInfo":{"targetId":"target","type":"page","url":"about:blank","title":""}}`), nil
	}
	return client.pageBindingCDPClient.Call(ctx, sessionID, method, params)
}

func TestPageNavigateRequiresNativeDownloadFact(t *testing.T) {
	for _, test := range []struct {
		name, result string
		download     bool
	}{
		{"native", `{"frameId":"frame","errorText":"net::ERR_ABORTED","isDownload":true}`, true},
		{"missing", `{"frameId":"frame","errorText":"net::ERR_ABORTED"}`, false},
		{"false", `{"frameId":"frame","errorText":"net::ERR_ABORTED","isDownload":false}`, false},
		{"other-error", `{"frameId":"frame","errorText":"net::ERR_FAILED","isDownload":true}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &navigationCDPClient{pageBindingCDPClient: pageBindingCDPClient{events: make(chan *cdp.Event)}, navigation: []byte(test.result)}
			life, cancel := context.WithCancel(context.Background())
			defer cancel()
			browser := rod.New().Client(client).NoDefaultDevice().Context(life)
			if err := browser.Connect(); err != nil {
				t.Fatal(err)
			}
			native, err := browser.PageFromTarget(proto.TargetTargetID("target"))
			if err != nil {
				t.Fatal(err)
			}
			page := &rodPage{page: native, revision: 3, refs: map[string]struct{}{"existing": {}}}
			info, err := page.Navigate(context.Background(), "https://example.test/file")
			if test.download {
				if err != nil || !info.IsDownload || info.URL != "about:blank" || page.revision != 3 {
					t.Fatalf("info=%+v revision=%d err=%v", info, page.revision, err)
				}
			} else {
				var navigation *rod.NavigationError
				if !errors.As(err, &navigation) {
					t.Fatalf("unconfirmed aborted navigation became success: info=%+v err=%v", info, err)
				}
			}
		})
	}
}
