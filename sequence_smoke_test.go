//go:build browser_smoke

package browserkit

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zeoxisca/awesome-browserkit/internal/toollab"
)

func TestRealBrowserManualSequenceRecordingAndReplay(t *testing.T) {
	server := httptest.NewServer(toollab.Handler())
	defer server.Close()
	manager, err := NewManager(Config{AllowPrivateNetwork: true, AllowEvaluate: true, MaxSessions: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	opened, err := manager.Open(ctx, OpenRequest{URL: server.URL + "/"})
	if err != nil {
		t.Fatal(err)
	}
	center := func(selector string) (float64, float64) {
		result, err := manager.Evaluate(ctx, EvaluateRequest{SessionID: opened.SessionID, PageID: opened.PageID, Code: `(() => { const r = document.querySelector(` + jsonString(selector) + `).getBoundingClientRect(); return {x:r.left+r.width/2,y:r.top+r.height/2} })()`})
		if err != nil {
			t.Fatal(err)
		}
		var point struct{ X, Y float64 }
		if err := json.Unmarshal(result.Value, &point); err != nil {
			t.Fatal(err)
		}
		return point.X, point.Y
	}
	click := func(x, y float64) {
		for _, action := range []string{"down", "up"} {
			if err := manager.DispatchLiveInput(ctx, opened.PageID, InputEvent{Kind: "mouse", Action: action, X: x, Y: y, Button: "left", Buttons: map[bool]int{true: 1, false: 0}[action == "down"], ClickCount: 1}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := manager.StartSequenceRecording(ctx, SequenceRecordingStartRequest{PageID: opened.PageID, Goal: "手动填写客户姓名"}); err != nil {
		t.Fatal(err)
	}
	x, y := center("#customer")
	click(x, y)
	if err := manager.DispatchLiveInput(ctx, opened.PageID, InputEvent{Kind: "text", Text: "Ada Lovelace"}); err != nil {
		t.Fatal(err)
	}
	x, y = center("#customer-form button")
	click(x, y)
	finished, err := manager.FinishSequenceRecording(ctx, SequenceRecordingFinishRequest{})
	if err != nil {
		t.Fatal(err)
	}
	assertSavedSequence(t, finished)
	if got := len(finished.Sequence.Steps); got != 4 {
		t.Fatalf("手动录制步骤数=%d: %+v", got, finished.Sequence.Steps)
	}
	replayed, err := manager.ReplaySequence(ctx, SequenceReplayRequest{Sequence: finished.Sequence, Variables: map[string]string{"input_1": "Grace Hopper"}})
	if err != nil || replayed.Status != "completed" {
		t.Fatalf("手动序列回放失败: %+v %v", replayed, err)
	}
	if _, err := manager.Wait(ctx, WaitRequest{SessionID: replayed.SessionID, PageID: replayed.PageID, Condition: "text_contains", RefOrSelector: "Saved: Grace Hopper"}); err != nil {
		t.Fatal(err)
	}
}

func jsonString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func TestRealBrowserSequenceGenerateAndReplay(t *testing.T) {
	server := httptest.NewServer(toollab.Handler())
	defer server.Close()

	manager, err := NewManager(Config{AllowPrivateNetwork: true, MaxSessions: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	lab := &toolLabRunner{t: t, ctx: ctx, manager: manager, called: make(map[string]bool)}

	opened := toolLabCall[OpenResponse](lab, "browserOpen", map[string]any{"url": server.URL + "/"})
	started := toolLabCall[SequenceDraftResponse](lab, "sequenceStart", map[string]any{
		"session_id": opened.SessionID,
		"page_id":    opened.PageID,
		"goal":       "填写客户姓名并保存",
		"variables": []map[string]any{{
			"name":        "customer_name",
			"description": "待保存的客户姓名",
			"default":     "Ada Lovelace",
		}},
	})
	if len(started.Candidates) != 1 || started.Candidates[0].Step.Type != "navigate" {
		t.Fatalf("sequenceStart 没有生成初始导航候选: %+v", started)
	}

	snapshot := toolLabCall[SnapshotResponse](lab, "browserSnapshot", map[string]any{
		"session_id": opened.SessionID, "page_id": opened.PageID, "max_elements": 100,
	})
	name := toolLabElement(t, snapshot, "textbox", "Customer name")
	typed := toolLabCall[TypeResponse](lab, "browserType", map[string]any{
		"session_id": opened.SessionID, "page_id": opened.PageID,
		"ref": name.Ref, "revision": snapshot.Revision,
		"text": "Ada Lovelace", "sequence_variable": "customer_name",
	})
	save := toolLabElement(t, snapshot, "button", "Save customer")
	clicked := toolLabCall[ClickResponse](lab, "browserClick", map[string]any{
		"session_id": opened.SessionID, "page_id": opened.PageID,
		"ref": save.Ref, "revision": snapshot.Revision,
	})
	toolLabWait(lab, opened.SessionID, opened.PageID, "text_contains", "Saved: Ada Lovelace")
	if typed.CandidateID == "" || clicked.CandidateID == "" {
		t.Fatalf("探索动作没有形成候选: type=%+v click=%+v", typed, clicked)
	}

	draft := toolLabCall[SequenceDraftResponse](lab, "sequenceRead", map[string]any{"session_id": opened.SessionID})
	if len(draft.Candidates) != 3 {
		t.Fatalf("候选数量错误: %+v", draft.Candidates)
	}
	candidateIDs := make([]string, 0, len(draft.Candidates))
	for _, candidate := range draft.Candidates {
		candidateIDs = append(candidateIDs, candidate.CandidateID)
	}
	finished := toolLabCall[SequenceResponse](lab, "sequenceFinish", map[string]any{
		"session_id": opened.SessionID,
		"groups": []map[string]any{{
			"candidate_ids": candidateIDs,
			"purpose":       "填写并提交客户表单",
		}},
	})
	if len(finished.Sequence.Steps) != 3 || finished.Sequence.Steps[1].ValueTemplate != "{{customer_name}}" {
		t.Fatalf("最终序列错误: %+v", finished.Sequence)
	}
	assertSavedSequence(t, finished)

	replayed := toolLabCall[SequenceReplayResponse](lab, "sequenceReplay", map[string]any{
		"sequence":  finished.Sequence,
		"variables": map[string]string{"customer_name": "Grace Hopper"},
	})
	if replayed.Status != "completed" || replayed.SessionID == opened.SessionID || replayed.CompletedSteps != 3 {
		t.Fatalf("序列回放没有完成或没有使用独立 session: %+v", replayed)
	}
	toolLabWait(lab, replayed.SessionID, replayed.PageID, "text_contains", "Saved: Grace Hopper")
	toolLabWait(lab, opened.SessionID, opened.PageID, "text_contains", "Saved: Ada Lovelace")

	if !toolLabCall[CloseResponse](lab, "browserClose", map[string]any{"session_id": replayed.SessionID}).Closed {
		t.Fatal("没有关闭回放 session")
	}
	if !toolLabCall[CloseResponse](lab, "browserClose", map[string]any{"session_id": opened.SessionID}).Closed {
		t.Fatal("没有关闭探索 session")
	}
	t.Logf("生成并回放成功：%d 个候选 -> %d 个步骤，变量覆盖为 Grace Hopper", len(draft.Candidates), replayed.CompletedSteps)
}
