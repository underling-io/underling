package claude

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFinalResult(t *testing.T) {
	tests := []struct {
		name        string
		stream      string
		wantSummary string
		wantOK      bool
	}{
		{"success", `{"type":"system","subtype":"init"}` + "\n" + `{"type":"result","subtype":"success","is_error":false,"result":"Fixed it."}` + "\n", "Fixed it.", true},
		{"no trailing newline", `{"type":"result","is_error":false,"result":"ok"}`, "ok", true},
		{"error result", `{"type":"result","is_error":true,"result":"boom"}` + "\n", "boom", false},
		{"result not last", `{"type":"result","is_error":false,"result":"ok"}` + "\n" + `{"type":"assistant"}` + "\n", "", false},
		{"no events", "", "", false},
		{"noise is skipped", `{"type":"result","is_error":false,"result":"ok"}` + "\nnot json\n\n", "ok", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := readStream(strings.NewReader(tt.stream))
			if err != nil {
				t.Fatalf("readStream: %v", err)
			}
			if out.summary != tt.wantSummary || out.ok != tt.wantOK {
				t.Fatalf("got (%q, %v), want (%q, %v)", out.summary, out.ok, tt.wantSummary, tt.wantOK)
			}
		})
	}
}

func TestFinalResultReadsLongLines(t *testing.T) {
	long := `{"type":"user","content":"` + strings.Repeat("x", 5<<20) + `"}` + "\n"
	out, err := readStream(strings.NewReader(long + `{"type":"result","is_error":false,"result":"ok"}` + "\n"))
	if err != nil || !out.ok || out.summary != "ok" {
		t.Fatalf("got (%q, %v, %v), want (ok, true, nil)", out.summary, out.ok, err)
	}
}

func TestReadStreamKeepsTheInitEvent(t *testing.T) {
	stream := `{"type":"system","subtype":"api_retry"}` + "\n" +
		`{"type":"system","subtype":"init","cwd":"/w/run/repo","permissionMode":"acceptEdits","plugins":[{"name":"codex","path":"/p/codex"}],"plugin_errors":[{"plugin":"at","type":"x","message":"m"}]}` + "\n" +
		`{"type":"result","is_error":false,"result":"ok"}` + "\n"

	out, err := readStream(strings.NewReader(stream))

	if err != nil || out.init == nil {
		t.Fatalf("got init %v, err %v", out.init, err)
	}
	if out.init.PermissionMode != "acceptEdits" || len(out.init.Plugins) != 1 || out.init.Plugins[0].Path != "/p/codex" || len(out.init.PluginErrors) != 1 {
		t.Fatalf("got init %+v", *out.init)
	}
}

func TestCheckInit(t *testing.T) {
	plugins := []string{"/p/codex", "/p/at"}
	good := func() *initEvent {
		return &initEvent{
			PermissionMode: "acceptEdits",
			Plugins:        []initPlugin{{Name: "codex", Path: "/p/codex"}, {Name: "at", Path: "/p/at/plugins/at"}},
		}
	}
	tests := []struct {
		name   string
		init   func() *initEvent
		reason string // empty: no error
	}{
		{"as configured", good, ""},
		{"missing", func() *initEvent { return nil }, "no system/init event"},
		{"other permission mode", func() *initEvent { e := good(); e.PermissionMode = "default"; return e }, `permission mode "default", not acceptEdits`},
		{"plugin missing", func() *initEvent { e := good(); e.Plugins = e.Plugins[:1]; return e }, "no plugin loaded from /p/at"},
		{"plugin errors", func() *initEvent {
			e := good()
			e.PluginErrors = []json.RawMessage{json.RawMessage(`{"plugin":"at","type":"hooks","message":"bad hooks file"}`)}
			return e
		}, "bad hooks file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkInit(tt.init(), "acceptEdits", plugins)
			if tt.reason == "" {
				if err != nil {
					t.Fatalf("checkInit: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.reason) {
				t.Fatalf("got %v, want an error containing %q", err, tt.reason)
			}
		})
	}
}

func TestCheckInitComparesAgainstTheModeTheCommandWasBuiltWith(t *testing.T) {
	init := &initEvent{PermissionMode: "acceptEdits"}

	err := checkInit(init, "dontAsk", nil)

	if err == nil || err.Error() != `permission mode "acceptEdits", not dontAsk` {
		t.Fatalf("got %v", err)
	}
	if err := checkInit(&initEvent{PermissionMode: "dontAsk"}, "dontAsk", nil); err != nil {
		t.Fatalf("checkInit: %v", err)
	}
}
