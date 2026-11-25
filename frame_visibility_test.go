package browserkit

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/cdp"
	"github.com/go-rod/rod/lib/proto"
)

type frameVisibilityCDPClient struct {
	pageBindingCDPClient
	failure      string
	cancelProbe  context.CancelFunc
	released     map[proto.RuntimeRemoteObjectID]bool
	releaseBound bool
}

func (client *frameVisibilityCDPClient) Call(ctx context.Context, sessionID, method string, params any) ([]byte, error) {
	switch request := params.(type) {
	case proto.PageGetFrameTree:
		return []byte(`{"frameTree":{"frame":{"id":"top","loaderId":"l0"},"childFrames":[{"frame":{"id":"parent","parentId":"top","loaderId":"l1"},"childFrames":[{"frame":{"id":"child","parentId":"parent","loaderId":"l2"}}]}]}}`), nil
	case proto.DOMGetFrameOwner:
		if request.FrameID == "parent" {
			return []byte(`{"backendNodeId":1}`), nil
		}
		return []byte(`{"backendNodeId":2}`), nil
	case proto.DOMResolveNode:
		if request.BackendNodeID == 1 {
			if client.failure == "resolve" {
				return nil, errors.New("owner resolution failed")
			}
			return []byte(`{"object":{"type":"object","objectId":"parent-owner"}}`), nil
		}
		return []byte(`{"object":{"type":"object","objectId":"child-owner"}}`), nil
	case proto.RuntimeCallFunctionOn:
		if request.ObjectID == "parent-owner" {
			switch client.failure {
			case "visibility":
				return []byte(`{"result":{"type":"object","value":null},"exceptionDetails":{"text":"visibility probe failed"}}`), nil
			case "origin":
				return []byte(`{"result":{"type":"object","value":{"hidden":false,"metadata_only":true,"suppress_text":true}}}`), nil
			case "cancel":
				client.cancelProbe()
				return nil, ctx.Err()
			}
		}
		return []byte(`{"result":{"type":"object","value":{"hidden":false}}}`), nil
	case proto.RuntimeReleaseObject:
		_, bounded := ctx.Deadline()
		client.releaseBound = bounded && ctx.Err() == nil
		client.released[request.ObjectID] = true
		return []byte(`{}`), nil
	case proto.PageCreateIsolatedWorld:
		return []byte(`{"executionContextId":1}`), nil
	case proto.RuntimeEvaluate:
		if strings.Contains(request.Expression, "view !== view.top") {
			return nil, errors.New("origin probe failed")
		}
		return []byte(`{"result":{"type":"object","value":{"text":"unverified child text","elements":[{"ref":"unexpected","value":"unverified child value"}]}}}`), nil
	}
	return client.pageBindingCDPClient.Call(ctx, sessionID, method, params)
}

func TestFrameCoverageDoesNotAssumeVisibilityWhenCDPFails(t *testing.T) {
	for _, tc := range []struct{ failure, reason string }{
		{"resolve", "frame_visibility_unavailable"},
		{"visibility", "frame_visibility_unavailable"},
		{"origin", "frame_origin_unavailable"},
	} {
		t.Run(tc.failure, func(t *testing.T) {
			page, client := newFrameVisibilityTestPage(t, tc.failure)
			raw := domSnapshot{URL: "https://example.test/", Text: "top text"}
			coverage := page.mergeFrameSnapshots(context.Background(), &raw, SnapshotOptions{MaxChars: 1000, MaxElements: 10})
			if len(coverage) != 3 || coverage[1].Status != "unavailable" || coverage[1].Reason != tc.reason || coverage[2].Status != "unavailable" || coverage[2].Reason != "frame_ancestor_visibility_unavailable" {
				t.Fatalf("unexpected frame coverage: %+v", coverage)
			}
			if raw.Text != "top text" || !raw.TextTruncated || len(raw.Elements) != 0 {
				t.Fatalf("unverified document was included: %+v", raw)
			}
			if client.released["parent-owner"] != (tc.failure != "resolve") || !client.released["child-owner"] {
				t.Fatalf("unexpected remote object cleanup: %+v", client.released)
			}
		})
	}
}

func TestFrameOwnerReleaseSurvivesCanceledProbe(t *testing.T) {
	page, client := newFrameVisibilityTestPage(t, "cancel")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.cancelProbe = cancel
	if _, ok := page.frameOwnerVisibilityIn(ctx, page.page, 1); ok || ctx.Err() != context.Canceled {
		t.Fatal("canceled visibility probe was treated as confirmed")
	}
	if !client.released["parent-owner"] || !client.releaseBound {
		t.Fatal("remote owner was not released with its own bounded context")
	}
}

func newFrameVisibilityTestPage(t *testing.T, failure string) (*rodPage, *frameVisibilityCDPClient) {
	t.Helper()
	client := &frameVisibilityCDPClient{
		pageBindingCDPClient: pageBindingCDPClient{events: make(chan *cdp.Event)},
		failure:              failure,
		released:             make(map[proto.RuntimeRemoteObjectID]bool),
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	browser := rod.New().Client(client).NoDefaultDevice().Context(ctx)
	if err := browser.Connect(); err != nil {
		t.Fatal(err)
	}
	nativePage, err := browser.PageFromTarget("top")
	if err != nil {
		t.Fatal(err)
	}
	return &rodPage{page: nativePage}, client
}
