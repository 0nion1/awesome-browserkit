//go:build browser_smoke

package browserkit

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRealBrowserFrameInputSemantics(t *testing.T) {
	for _, location := range []string{"main", "same-origin", "cross-origin", "OOPIF"} {
		t.Run(location, func(t *testing.T) { testFrameInputSemantics(t, location) })
	}
}

func testFrameInputSemantics(t *testing.T, location string) {
	t.Helper()
	const form = `<textarea aria-label="Payload"></textarea>
<input aria-label="Number" type="number" min="1" max="10" step="2" value="3">
<input aria-label="Range" type="range" min="1" max="10" step="2" value="3">
<input aria-label="Date" type="date" min="2026-01-01" max="2026-12-31" value="2026-01-01">
<input aria-label="Month" type="month" min="2026-01" max="2026-12" value="2026-02">
<input aria-label="Week" type="week" min="2026-W01" max="2026-W52" value="2026-W02">
<input aria-label="Time" type="time" min="09:00" max="17:00" step="60" value="10:00">
<input aria-label="DateTime" type="datetime-local" min="2026-01-01T00:00" max="2026-12-31T23:59" value="2026-01-01T10:00">
<input aria-label="Color" type="color" value="#112233">
<input aria-label="Readonly" readonly value="original"><input aria-label="Disabled" disabled value="original">`
	document := `<!doctype html><div id="host"></div><template id="form">` + form + `</template><script>document.getElementById('host').attachShadow({mode:'open'}).append(document.getElementById('form').content);</script>`
	var frameURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if location == "main" || r.URL.Path == "/frame" {
			fmt.Fprint(w, document)
			return
		}
		fmt.Fprintf(w, `<!doctype html><iframe src="%s" width="1000" height="600"></iframe>`, frameURL)
	}))
	defer server.Close()
	if location == "same-origin" {
		frameURL = server.URL + "/frame"
	} else if location != "main" {
		frame := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, document)
		}))
		if location == "OOPIF" {
			listener, err := listenCrossSite("127.0.0.2:0")
			if err != nil {
				t.Fatal(err)
			}
			_ = frame.Listener.Close()
			frame.Listener = listener
		}
		frame.Start()
		defer frame.Close()
		frameURL = frame.URL
	}
	manager, err := NewManager(Config{AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	opened, err := manager.Open(ctx, OpenRequest{URL: server.URL, Viewport: Viewport{Width: 1200, Height: 900}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
	if err != nil {
		t.Fatal(err)
	}
	refFor := func(label string) string {
		for _, el := range snapshot.Elements {
			if el.Name == label {
				return el.Ref
			}
		}
		t.Fatalf("missing input %s: %+v", label, snapshot.Elements)
		return ""
	}
	typeValue := func(label, text string, appendValue bool) error {
		_, err := manager.Type(ctx, TypeRequest{SessionID: opened.SessionID, PageID: opened.PageID, Ref: refFor(label), Revision: snapshot.Revision, Text: text, Append: appendValue})
		return err
	}
	readValue := func(label string) string {
		current, err := manager.Snapshot(ctx, SnapshotRequest{SessionID: opened.SessionID, PageID: opened.PageID})
		if err != nil {
			t.Fatal(err)
		}
		for _, el := range current.Elements {
			if el.Name == label {
				return el.Value
			}
		}
		t.Fatalf("missing input after type: %s", label)
		return ""
	}
	payload := "document.querySelector('input[name=q]')\n__browserkit_find(\"汉字\") %s"
	if err := typeValue("Payload", payload, false); err != nil {
		t.Fatal(err)
	}
	if got := readValue("Payload"); got != payload {
		t.Fatalf("payload changed: %q != %q", got, payload)
	}
	if err := typeValue("Payload", " + document.querySelector('x')", true); err != nil {
		t.Fatal(err)
	}
	if got := readValue("Payload"); got != payload+" + document.querySelector('x')" {
		t.Fatalf("append payload changed: %q", got)
	}
	for _, tc := range []struct {
		label, valid string
		invalid      []string
	}{
		{"Number", "5", []string{"99", "4", "NaN"}},
		{"Range", "5", []string{"99", "4", "5."}},
		{"Date", "2026-07-01", []string{"2027-01-01", "2026-02-30"}},
		{"Month", "2026-03", []string{"2027-01", "2026-13"}},
		{"Week", "2026-W03", []string{"2027-W01", "2026-W54"}},
		{"Time", "12:30", []string{"18:00", "12:30:01", "25:00"}},
		{"DateTime", "2026-07-01T12:30", []string{"2027-01-01T12:30", "2026-02-30T12:30"}},
		{"Color", "#abcdef", []string{"red", ""}},
	} {
		if err := typeValue(tc.label, tc.valid, false); err != nil {
			t.Fatalf("%s valid: %v", tc.label, err)
		}
		for _, invalid := range tc.invalid {
			if err := typeValue(tc.label, invalid, false); err == nil {
				t.Fatalf("%s accepted invalid value %q", tc.label, invalid)
			}
			if got := readValue(tc.label); got != tc.valid {
				t.Fatalf("%s failed rollback: %q != %q", tc.label, got, tc.valid)
			}
		}
	}
	if err := typeValue("Number", "9", true); err == nil {
		t.Fatal("number append accepted 59 outside max")
	}
	if got := readValue("Number"); got != "5" {
		t.Fatalf("number append failed rollback: %q", got)
	}
	for _, label := range []string{"Readonly", "Disabled"} {
		if err := typeValue(label, "changed", false); err == nil || !strings.Contains(err.Error(), "不可编辑") {
			t.Fatalf("%s input error: %v", label, err)
		}
		if got := readValue(label); got != "original" {
			t.Fatalf("%s input mutated: %q", label, got)
		}
	}
}
