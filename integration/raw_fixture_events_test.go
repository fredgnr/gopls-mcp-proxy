package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/fredgnr/gopls-mcp-proxy/internal/testbackend"
)

// Evidence polling races with the helper creating and appending its JSONL log.
// Reproduce each visible file state without depending on process scheduling.
func TestRawFixtureEventsWaitForCompleteLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	assertEvents := func(stage string, want []testbackend.Event) {
		t.Helper()
		got, err := readFixtureEvents(path)
		if err != nil {
			t.Fatalf("%s: read events: %v", stage, err)
		}
		if len(got) == 0 && len(want) == 0 {
			return
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: events = %#v, want %#v", stage, got, want)
		}
	}
	assertEvents("not yet created", nil)
	log, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	assertEvents("created but empty", nil)

	first := testbackend.Event{Kind: "execution", Boot: "first", PID: 123, Method: "fixture/hold", ID: json.RawMessage(`7`)}
	second := testbackend.Event{Kind: "execution", Boot: "second", PID: 456, Method: "fixture/echo", ID: json.RawMessage(`"7"`)}
	steps := []struct {
		name, append string
		want         []testbackend.Event
	}{
		{"partial first record", `{"kind":"execution","boot":"first","pid":123,"method":"fixture/hold","id":7`, nil},
		{"first JSON complete without newline", `}`, nil},
		{"first record published", "\n", []testbackend.Event{first}},
		{"complete first and partial second record", `{"kind":"execution","boot":"second","pid":456,"method":"fixture/echo","id":`, []testbackend.Event{first}},
		{"second JSON complete without newline", `"7"}`, []testbackend.Event{first}},
		{"second record published", "\n", []testbackend.Event{first, second}},
	}
	for _, step := range steps {
		if _, err := log.WriteString(step.append); err != nil {
			t.Fatalf("%s: append event: %v", step.name, err)
		}
		assertEvents(step.name, step.want)
	}
	assertEvents("unchanged repeated read", []testbackend.Event{first, second})
}

func TestRawFixtureEventsRejectMalformedCompleteLines(t *testing.T) {
	for _, raw := range []string{
		"{\n",
		"{\"kind\":\"boot\"}\n{\"kind\":\n",
	} {
		path := filepath.Join(t.TempDir(), "events.jsonl")
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readFixtureEvents(path); err == nil {
			t.Fatalf("accepted malformed complete record in %q", raw)
		}
	}
}

func TestRawFixtureEventsPreserveReadErrors(t *testing.T) {
	// Reading a directory fails on both supported operating systems and does not
	// depend on chmod permissions, which are ineffective under a root test user.
	if _, err := readFixtureEvents(t.TempDir()); err == nil {
		t.Fatal("directory read failure was treated as an empty event log")
	}
}
