package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

//
// CODE
//

// writeMocks drops a mocks.json in a temp dir and returns a Server using it.
func writeMocks(t *testing.T, mocks string) *Server {
	t.Helper()

	path := filepath.Join(t.TempDir(), "mocks.json")
	if err := os.WriteFile(path, []byte(mocks), 0o644); err != nil {
		t.Fatalf("write mocks: %v", err)
	}

	return NewServer(path, "", "", []int{200})
}

// call sends one request through the server and returns the recorder.
func call(server *Server, method, path string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(method, path, nil))
	return recorder
}

// TestServerGlob covers how mocks.json path patterns match.
func TestServerGlob(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		path    string
		want    bool
	}{
		{"star crosses slashes", "/meta/*/messages", "/meta/a/b/messages", true},
		{"star matches one segment", "/meta/*/messages", "/meta/1/messages", true},
		{"suffix must match", "/meta/*/messages", "/meta/1/other", false},
		{"dot is not a wildcard", "/a.b", "/axb", false},
		{"exact path matches", "/bilhetis/tickets", "/bilhetis/tickets", true},
	}

	server := NewServer("", "", "", []int{200})

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			// matched?
			got := server.glob(testCase.pattern).MatchString(testCase.path)
			if got != testCase.want {
				t.Fatalf("got %v, want %v", got, testCase.want)
			}
		})
	}
}

// TestServerSequence covers a mock answering a cycling sequence.
func TestServerSequence(t *testing.T) {
	server := writeMocks(t, `[{
		"method": "POST",
		"path": "/tickets",
		"sequence": [
			{"status": 500, "body": {"error": "boom"}},
			{"status": 200, "body": {"id": "tkt-1"}}
		]
	}]`)

	t.Run("should cycle through the steps", func(t *testing.T) {
		want := []int{500, 200, 500, 200}

		for index, expected := range want {
			// stepped?
			if got := call(server, "POST", "/tickets").Code; got != expected {
				t.Fatalf("call %d: got %d, want %d", index, got, expected)
			}
		}
	})

	t.Run("should answer the step body", func(t *testing.T) {
		body := call(server, "POST", "/tickets").Body.String()

		// answered?
		if !strings.Contains(body, "boom") {
			t.Fatalf("got %q, want a body containing boom", body)
		}
	})
}

// TestServerOverride covers the status forced from the TUI.
func TestServerOverride(t *testing.T) {
	server := writeMocks(t,
		`[{"method": "GET", "path": "/prices", "status": 200, "body": {}}]`)

	t.Run("should force the status while set", func(t *testing.T) {
		server.SetOverride("GET /prices", 429)

		// forced?
		if got := call(server, "GET", "/prices").Code; got != 429 {
			t.Fatalf("got %d, want 429", got)
		}
	})

	t.Run("should restore the mock status once cleared", func(t *testing.T) {
		server.SetOverride("GET /prices", 0)

		// restored?
		if got := call(server, "GET", "/prices").Code; got != 200 {
			t.Fatalf("got %d, want 200", got)
		}
	})
}

// TestServerSecret covers the silent 404 guarding a public deployment.
func TestServerSecret(t *testing.T) {
	server := writeMocks(t,
		`[{"method": "GET", "path": "/prices", "status": 201, "body": {}}]`)
	server.Secret = "s3cr3t"

	t.Run("should drop a call without the prefix", func(t *testing.T) {
		// dropped?
		if got := call(server, "GET", "/prices").Code; got != http.StatusNotFound {
			t.Fatalf("got %d, want 404", got)
		}
	})

	t.Run("should serve the stripped path", func(t *testing.T) {
		// served?
		if got := call(server, "GET", "/s3cr3t/prices").Code; got != 201 {
			t.Fatalf("got %d, want 201", got)
		}
	})
}

// TestServerCapture covers what the TUI and the log file receive.
func TestServerCapture(t *testing.T) {
	server := writeMocks(t, `[{"method": "GET", "path": "/prices"}]`)

	var captured []Request
	server.Sink = func(request Request) { captured = append(captured, request) }

	t.Run("should name the endpoint of a matched call", func(t *testing.T) {
		call(server, "GET", "/prices")

		// named?
		if captured[0].Endpoint != "GET /prices" {
			t.Fatalf("got %q, want %q", captured[0].Endpoint, "GET /prices")
		}
	})

	t.Run("should send an unknown path to the unmatched screen", func(t *testing.T) {
		call(server, "GET", "/nope")

		// unmatched?
		if captured[1].Endpoint != unmatched {
			t.Fatalf("got %q, want %q", captured[1].Endpoint, unmatched)
		}
	})

	t.Run("should strip control characters from the path", func(t *testing.T) {
		// a raw ESC never survives the URL parser, percent-encoding does
		call(server, "GET", "/no%1b[31mpe")

		// cleaned?
		if strings.Contains(captured[2].Path, "\x1b") {
			t.Fatalf("got %q, want no escape sequences", captured[2].Path)
		}
	})
}

// TestServerAppendLog covers the .jsonl written next to the binary.
func TestServerAppendLog(t *testing.T) {
	server := writeMocks(t, `[{"method": "GET", "path": "/prices"}]`)
	server.LogFile = filepath.Join(t.TempDir(), "webhook.jsonl")

	t.Run("should write one json line per call", func(t *testing.T) {
		call(server, "GET", "/prices")
		call(server, "GET", "/prices")

		raw, err := os.ReadFile(server.LogFile)
		if err != nil {
			t.Fatalf("read log: %v", err)
		}

		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")

		// two lines?
		if len(lines) != 2 {
			t.Fatalf("got %d lines, want 2", len(lines))
		}

		// parsable?
		var logged Request
		if err := json.Unmarshal([]byte(lines[0]), &logged); err != nil {
			t.Fatalf("parse line: %v", err)
		}

		if logged.Path != "/prices" {
			t.Fatalf("got %q, want %q", logged.Path, "/prices")
		}
	})
}

// TestColorizeJSON covers the jq-style painting of the detail pane.
func TestColorizeJSON(t *testing.T) {
	t.Run("should paint keys and strings differently", func(t *testing.T) {
		got := colorizeJSON(`{"name":"pecazap"}`)

		// keys painted?
		if !strings.Contains(got, keyStyle.Render(`"name":`)) {
			t.Fatalf("got %q, want a painted key", got)
		}

		// strings painted?
		if !strings.Contains(got, stringStyle.Render(`"pecazap"`)) {
			t.Fatalf("got %q, want a painted string", got)
		}
	})

	t.Run("should indent nested json", func(t *testing.T) {
		got := colorizeJSON(`{"a":{"b":1}}`)

		// indented?
		if !strings.Contains(got, "\n") {
			t.Fatalf("got %q, want indented output", got)
		}
	})

	t.Run("should return non json untouched", func(t *testing.T) {
		got := colorizeJSON("not json at all")

		// untouched?
		if got != "not json at all" {
			t.Fatalf("got %q, want the raw text", got)
		}
	})
}

// TestParseStatusCodes covers the STATUS environment variable.
func TestParseStatusCodes(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []int
	}{
		{"list", "[200,500]", []int{200, 500}},
		{"spaces", " 200 , 404 ", []int{200, 404}},
		{"empty", "", []int{200}},
		{"garbage", "[abc]", []int{200}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := parseStatusCodes(testCase.raw)

			// parsed?
			if len(got) != len(testCase.want) {
				t.Fatalf("got %v, want %v", got, testCase.want)
			}

			for index, code := range testCase.want {
				if got[index] != code {
					t.Fatalf("got %v, want %v", got, testCase.want)
				}
			}
		})
	}
}

// newTestModel builds a sized TUI over a one-mock server.
func newTestModel(t *testing.T) *model {
	t.Helper()

	ui := newModel(writeMocks(t, `[{"method": "GET", "path": "/prices"}]`))
	ui.resize(120, 40)
	return ui
}

// TestModelScreens covers how the TUI files requests into screens.
func TestModelScreens(t *testing.T) {
	ui := newTestModel(t)

	t.Run("should pre-create a screen per mock", func(t *testing.T) {
		// two screens?
		if len(ui.endpoints) != 2 {
			t.Fatalf("got %d screens, want All plus one mock", len(ui.endpoints))
		}

		// All first?
		if ui.endpoints[0].name != allScreen {
			t.Fatalf("got %q first, want %q", ui.endpoints[0].name, allScreen)
		}
	})

	t.Run("should file a request into All and its own screen", func(t *testing.T) {
		ui.add(Request{Method: "GET", Path: "/prices", Endpoint: "GET /prices"})

		// in All?
		if len(ui.endpoints[0].requests) != 1 {
			t.Fatalf("got %d in All, want 1", len(ui.endpoints[0].requests))
		}

		// in its own screen?
		if len(ui.endpoints[1].requests) != 1 {
			t.Fatalf("got %d in the mock screen, want 1", len(ui.endpoints[1].requests))
		}
	})

}

// TestModelUnread covers the badge counting calls on unwatched screens.
func TestModelUnread(t *testing.T) {
	ui := newTestModel(t)
	ui.add(Request{Method: "GET", Path: "/prices", Endpoint: "GET /prices"})

	t.Run("should count unread on unwatched screens", func(t *testing.T) {
		// unread counted?
		if ui.endpoints[1].unread != 1 {
			t.Fatalf("got %d unread, want 1", ui.endpoints[1].unread)
		}

		// watched screen stays read?
		if ui.endpoints[0].unread != 0 {
			t.Fatalf("got %d unread on the watched screen, want 0",
				ui.endpoints[0].unread)
		}
	})

	t.Run("should clear the badge once the screen is opened", func(t *testing.T) {
		ui.nextEndpoint()

		// cleared?
		if ui.current().unread != 0 {
			t.Fatalf("got %d unread, want the badge cleared", ui.current().unread)
		}
	})
}

// TestModelDiscovery covers screens created by calls no mock declared.
func TestModelDiscovery(t *testing.T) {
	ui := newTestModel(t)

	t.Run("should create a screen for an unknown endpoint", func(t *testing.T) {
		ui.add(Request{Method: "GET", Path: "/nope", Endpoint: unmatched})

		// created?
		if ui.screen(unmatched).name != unmatched {
			t.Fatalf("got no screen for %q", unmatched)
		}
	})
}

// TestModelPause covers holding requests back while the list is frozen.
func TestModelPause(t *testing.T) {
	ui := newModel(NewServer("", "", "", []int{200}))
	ui.resize(120, 40)

	t.Run("should hold requests while paused", func(t *testing.T) {
		ui.togglePause()
		ui.add(Request{Method: "GET", Path: "/a", Endpoint: "GET /a"})

		// held?
		if len(ui.endpoints[0].requests) != 0 {
			t.Fatalf("got %d rows, want the list frozen",
				len(ui.endpoints[0].requests))
		}
	})

	t.Run("should flush the queue on resume", func(t *testing.T) {
		ui.togglePause()

		// flushed?
		if len(ui.endpoints[0].requests) != 1 {
			t.Fatalf("got %d rows, want 1", len(ui.endpoints[0].requests))
		}
	})
}

// TestModelFilter covers the search bar hiding rows.
func TestModelFilter(t *testing.T) {
	ui := newModel(NewServer("", "", "", []int{200}))
	ui.resize(120, 40)
	ui.add(Request{Method: "GET", Path: "/prices", Endpoint: "GET /prices"})
	ui.add(Request{Method: "POST", Path: "/tickets", Endpoint: "POST /tickets"})

	t.Run("should hide rows missing the needle", func(t *testing.T) {
		ui.filter.SetValue("tickets")

		// filtered?
		if len(ui.visible()) != 1 {
			t.Fatalf("got %d rows, want 1", len(ui.visible()))
		}
	})

	t.Run("should search the body too", func(t *testing.T) {
		ui.filter.SetValue("wamid")
		ui.add(Request{Method: "POST", Path: "/m", Body: `{"id":"wamid.1"}`,
			Endpoint: "POST /m"})

		// found?
		if len(ui.visible()) != 1 {
			t.Fatalf("got %d rows, want 1", len(ui.visible()))
		}
	})

	t.Run("should show everything once cleared", func(t *testing.T) {
		ui.clearFilter()

		// restored?
		if len(ui.visible()) != 3 {
			t.Fatalf("got %d rows, want 3", len(ui.visible()))
		}
	})
}

// TestModelFocus covers the arrows driving whichever pane holds the focus.
func TestModelFocus(t *testing.T) {
	ui := newTestModel(t)
	ui.add(Request{Method: "GET", Path: "/a", Endpoint: "GET /prices"})
	ui.add(Request{Method: "GET", Path: "/b", Endpoint: "GET /prices"})

	press := func(key tea.KeyType) { ui.handleKey(tea.KeyMsg{Type: key}) }

	t.Run("should start on the sidebar", func(t *testing.T) {
		// focused?
		if ui.focus != paneSidebar {
			t.Fatalf("got focus %d, want the sidebar", ui.focus)
		}
	})

	t.Run("should change screen with the vertical arrows", func(t *testing.T) {
		press(tea.KeyDown)

		// moved?
		if ui.selected != 1 {
			t.Fatalf("got screen %d, want the second one", ui.selected)
		}

		press(tea.KeyUp)

		// back?
		if ui.selected != 0 {
			t.Fatalf("got screen %d, want the first one", ui.selected)
		}
	})

	t.Run("should hand the arrows to the list on right", func(t *testing.T) {
		press(tea.KeyRight)
		press(tea.KeyUp)

		// list moved instead of the sidebar?
		if ui.selected != 0 {
			t.Fatalf("got screen %d, want the sidebar left alone", ui.selected)
		}

		if ui.cursor != 0 {
			t.Fatalf("got cursor %d, want it moved up to 0", ui.cursor)
		}
	})

	t.Run("should give the arrows back on left", func(t *testing.T) {
		press(tea.KeyLeft)
		press(tea.KeyDown)

		// sidebar moved again?
		if ui.selected != 1 {
			t.Fatalf("got screen %d, want the sidebar driving again", ui.selected)
		}
	})
}

// TestModelView covers the layout arithmetic, which can only fail at runtime.
func TestModelView(t *testing.T) {
	ui := newTestModel(t)
	ui.add(Request{
		Time:     time.Date(2026, 8, 14, 14, 22, 1, 0, time.UTC),
		Method:   "POST",
		Path:     "/meta/123/messages",
		Headers:  map[string][]string{"Content-Type": {"application/json"}},
		Body:     `{"messaging_product":"whatsapp","contacts":[{"wa_id":"5511"}]}`,
		Status:   200,
		Endpoint: "POST /meta/*/messages",
	})

	t.Run("should draw the panes and the footer", func(t *testing.T) {
		view := ui.View()
		t.Logf("\n%s", view)

		for _, want := range []string{"endpoints", allScreen, "/meta/123/messages",
			"Body:", "↑↓ navega"} {
			// drawn?
			if !strings.Contains(view, want) {
				t.Fatalf("got a view without %q", want)
			}
		}
	})

	t.Run("should survive a tiny terminal", func(t *testing.T) {
		ui.resize(20, 4)

		// no panic?
		if ui.View() == "" {
			t.Fatal("got an empty view, want something drawn")
		}
	})

	t.Run("should draw before any window size arrives", func(t *testing.T) {
		// a terminal that never reports its size must not leave a blank app
		fresh := newModel(NewServer("", "", "", []int{200}))

		// drawn?
		if !strings.Contains(fresh.View(), "endpoints") {
			t.Fatalf("got %q, want the panes already drawn", fresh.View())
		}
	})
}

// TestModelOverride covers cycling the forced status from the TUI.
func TestModelOverride(t *testing.T) {
	server := writeMocks(t,
		`[{"method": "GET", "path": "/prices", "status": 200, "body": {}}]`)
	ui := newModel(server)
	ui.resize(120, 40)

	t.Run("should not override the All screen", func(t *testing.T) {
		ui.cycleOverride()

		// untouched?
		if ui.endpoints[0].override != 0 {
			t.Fatalf("got %d, want the All screen left alone",
				ui.endpoints[0].override)
		}
	})

	t.Run("should reach the server on the next call", func(t *testing.T) {
		ui.nextEndpoint()
		ui.cycleOverride()

		// forced?
		if got := call(server, "GET", "/prices").Code; got != 500 {
			t.Fatalf("got %d, want 500", got)
		}
	})

	t.Run("should wrap back to no override", func(t *testing.T) {
		ui.cycleOverride() // 429
		ui.cycleOverride() // 200
		ui.cycleOverride() // off

		// cleared?
		if got := call(server, "GET", "/prices").Code; got != 200 {
			t.Fatalf("got %d, want the mock status back", got)
		}
	})
}
