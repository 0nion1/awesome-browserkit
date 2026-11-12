package browserkit

import (
	"testing"

	"github.com/go-rod/rod/lib/proto"
)

func TestPickHistoryTargetSkipsBlankEntries(t *testing.T) {
	entries := []*proto.PageNavigationEntry{
		{ID: 0, URL: "about:blank"},
		{ID: 1, URL: "https://example.com/a"},
		{ID: 2, URL: "https://example.com/b"},
		{ID: 3, URL: "about:blank"},
		{ID: 4, URL: "https://example.com/c"},
	}
	target, ok := pickHistoryTarget(entries, 4, "back")
	if !ok || target.URL != "https://example.com/b" || target.ID != 2 {
		t.Fatalf("back 目标=%+v ok=%v", target, ok)
	}
	target, ok = pickHistoryTarget(entries, 2, "back")
	if !ok || target.URL != "https://example.com/a" {
		t.Fatalf("back 越过 about:blank 失败: %+v ok=%v", target, ok)
	}
	if _, ok := pickHistoryTarget(entries, 1, "back"); ok {
		t.Fatal("只有空白项时不应有 back 目标")
	}
	if _, ok := pickHistoryTarget(entries, 4, "forward"); ok {
		t.Fatal("末尾不应有 forward 目标")
	}
	if _, ok := pickHistoryTarget(entries, 0, "back"); ok {
		t.Fatal("首项不应有 back 目标")
	}
	if _, ok := pickHistoryTarget(entries, -1, "back"); ok {
		t.Fatal("非法索引不应有历史目标")
	}
}

func TestHistorySameDocumentMatchesEntryURL(t *testing.T) {
	entry := HistoryEntry{URL: "https://example.com/a"}
	if !historySameDocumentMatches(2, 1, "https://example.com/a", entry) {
		t.Fatal("目标 URL 的同文档事件应匹配")
	}
	if historySameDocumentMatches(2, 1, "https://example.com/b", entry) {
		t.Fatal("非目标 URL 的同文档事件不应匹配")
	}
	if historySameDocumentMatches(1, 1, "https://example.com/a", entry) {
		t.Fatal("基线之前的同文档事件不应匹配")
	}
}
