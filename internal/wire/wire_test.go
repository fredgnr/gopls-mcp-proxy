package wire

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

func TestRawEnvelopeAndExactPathRewrite(t *testing.T) {
	raw := []byte(` { "jsonrpc" : "2.0", "id":9007199254740993,"method":"vendor/unknown","params":{"id":9007199254740993,"arguments":{"id":9007199254740993,"_meta":{"progressToken":"business"}},"_meta":{"progressToken":"association","future":[null,9007199254740993]}},"unknown":{"keep":"<>&","n":1e90}} `)
	m, err := ParseMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(m.Raw, raw) || m.ID.Key != "n:9007199254740993" {
		t.Fatalf("message changed: %+v", m)
	}
	rewritten, err := Replace(raw, []byte(`"control:1"`), "id")
	if err != nil {
		t.Fatal(err)
	}
	expected := bytes.Replace(raw, []byte(`"id":9007199254740993`), []byte(`"id":"control:1"`), 1)
	if !bytes.Equal(rewritten, expected) {
		t.Fatalf("rewrite affected unrelated bytes:\n%s\n%s", rewritten, expected)
	}
	rewritten, err = Replace(raw, []byte(`"replacement"`), "params", "_meta", "progressToken")
	if err != nil {
		t.Fatal(err)
	}
	expected = bytes.Replace(raw, []byte(`"progressToken":"association"`), []byte(`"progressToken":"replacement"`), 1)
	if !bytes.Equal(rewritten, expected) {
		t.Fatal("progress rewrite traversed application payload")
	}
	if _, err := Replace(raw, []byte(`1`), "params", "absent"); !errors.Is(err, ErrMissingPath) {
		t.Fatal(err)
	}
}

func TestTypedIDsAndNumericPrecision(t *testing.T) {
	for _, raw := range []string{`1`, `"1"`, `9007199254740992`, `9007199254740993`, `"\u0031"`, `-0`, `1.0`, `100`, `1e2`, `1e999999999999999999999`} {
		id, err := ParseID([]byte(raw))
		if err != nil || string(id.Raw) != raw {
			t.Fatalf("%s: %+v %v", raw, id, err)
		}
	}
	ids := func(s string) ID {
		id, err := ParseID([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	if ids(`1`).Key == ids(`"1"`).Key || ids(`9007199254740992`).Key == ids(`9007199254740993`).Key {
		t.Fatal("ID collision")
	}
	for _, pair := range [][2]string{{`"1"`, `"\u0031"`}, {`0`, `-0`}, {`1`, `1.0`}, {`100`, `1e2`}, {`1`, `10e-1`}} {
		if ids(pair[0]).Key != ids(pair[1]).Key {
			t.Fatalf("equivalent ID mismatch %v", pair)
		}
	}
	for _, raw := range []string{`null`, `true`, `[]`, `{}`, `1.1`, `1e-1`, `01`, ``} {
		if _, err := ParseID([]byte(raw)); err == nil {
			t.Errorf("accepted ID %q", raw)
		}
	}
}

func TestMessageKindsAndOpaqueFields(t *testing.T) {
	tests := []struct {
		raw   string
		kind  Kind
		hasID bool
	}{
		{`{"jsonrpc":"2.0","id":"a","method":"x","params":null,"x":null}`, Request, true},
		{`{"jsonrpc":"2.0","method":"x"}`, Notification, false},
		{`{"jsonrpc":"2.0","id":1,"result":null,"_meta":{"future":true}}`, Response, true},
		{`{"jsonrpc":"2.0","id":1,"error":{"code":123,"message":"x","data":null}}`, Response, true},
		{`{"jsonrpc":"2.0","error":{"code":-32700,"message":"parse"}}`, Response, false},
		{`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"parse"}}`, Response, false},
	}
	for _, tt := range tests {
		m, err := ParseMessage([]byte(tt.raw))
		if err != nil || m.Kind != tt.kind || m.HasID != tt.hasID || string(m.Raw) != tt.raw {
			t.Fatalf("%s: %+v %v", tt.raw, m, err)
		}
	}
	for _, raw := range []string{`null`, `1`, `[]`, `{}`, `{"jsonrpc":"1.0","method":"x"}`, `{"jsonrpc":"2.0","id":null,"method":"x"}`, `{"jsonrpc":"2.0","id":1,"id":2,"method":"x"}`, `{"jsonrpc":"2.0","id":1,"result":{},"error":{}}`} {
		if _, err := ParseMessage([]byte(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}

func TestBatchMembersRemainRaw(t *testing.T) {
	raw := []byte(`[{ "jsonrpc":"2.0", "id":9007199254740993,"method":"x", "params":null },{"jsonrpc":"2.0","method":"notice","unknown":1e99},{"jsonrpc":"2.0","id":"1","error":{"code":42,"message":"opaque","data":[null]}}]`)
	f, err := ParseFrame(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !f.Batch || len(f.Messages) != 3 || !bytes.Equal(raw, f.Raw) {
		t.Fatalf("%+v", f)
	}
	var rebuilt [][]byte
	for _, m := range f.Messages {
		rebuilt = append(rebuilt, m.Raw)
	}
	joined, err := Join(rebuilt, true)
	if err != nil || !bytes.Equal(raw, joined) {
		t.Fatalf("%s %v", joined, err)
	}
	if _, err := Join(nil, true); err == nil {
		t.Fatal("emitted empty batch")
	}
	if _, err := ParseFrame([]byte(`[]`)); err == nil {
		t.Fatal("accepted empty batch")
	}
}

type shortReader struct{ io.Reader }

func (r shortReader) Read(p []byte) (int, error) { return r.Reader.Read(p[:min(len(p), 3)]) }

func TestFramingShortReadsLargeFramesAndEOF(t *testing.T) {
	large := `{"jsonrpc":"2.0","method":"opaque","params":{"s":"` + strings.Repeat("x", 256<<10) + `"}}`
	input := large + "\n" + `{"jsonrpc":"2.0","method":"two"}` + "\n"
	reader := NewReader(shortReader{strings.NewReader(input)}, 512<<10)
	first, err := reader.ReadFrame()
	if err != nil || string(first) != large {
		t.Fatalf("large frame: len=%d err=%v", len(first), err)
	}
	second, err := reader.ReadFrame()
	if err != nil || string(second) != `{"jsonrpc":"2.0","method":"two"}` {
		t.Fatalf("second: %s %v", second, err)
	}
	if _, err := reader.ReadFrame(); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if _, err := NewReader(strings.NewReader(`{"jsonrpc":"2.0"}`), 1024).ReadFrame(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
	if _, err := NewReader(strings.NewReader("12345\n"), 4).ReadFrame(); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatal(err)
	}
	if frame, err := NewReader(strings.NewReader("1234\n"), 4).ReadFrame(); err != nil || string(frame) != "1234" {
		t.Fatalf("exact limit %q %v", frame, err)
	}
	if frame, err := NewReader(shortReader{strings.NewReader("1234\r\n")}, 4).ReadFrame(); err != nil || string(frame) != "1234" {
		t.Fatalf("CRLF delimiter counted as payload %q %v", frame, err)
	}
}

type shortWriter struct{ bytes.Buffer }

func (w *shortWriter) Write(p []byte) (int, error) { return w.Buffer.Write(p[:min(len(p), 3)]) }

type stoppedWriter struct{}

func (stoppedWriter) Write([]byte) (int, error) { return 0, nil }

type partialFailure struct{ bytes.Buffer }

func (w *partialFailure) Write(p []byte) (int, error) {
	n, _ := w.Buffer.Write(p[:min(2, len(p))])
	return n, io.ErrClosedPipe
}

func TestSerializedFullWrites(t *testing.T) {
	out := &shortWriter{}
	writer := NewWriter(out, 1024)
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := writer.WriteFrame([]byte(`{"jsonrpc":"2.0","method":"x"}`)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	reader := NewReader(bytes.NewReader(out.Bytes()), 1024)
	for i := 0; i < 30; i++ {
		frame, err := reader.ReadFrame()
		if err != nil || string(frame) != `{"jsonrpc":"2.0","method":"x"}` {
			t.Fatalf("interleaved frame %s %v", frame, err)
		}
	}
	if err := NewWriter(stoppedWriter{}, 1024).WriteFrame([]byte(`{}`)); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
	partial := &partialFailure{}
	if err := NewWriter(partial, 1024).WriteFrame([]byte(`{"a":1}`)); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	if partial.String() != "{\"" {
		t.Fatalf("retried uncertain write: %q", partial.String())
	}
}

func TestMissingNullAndDuplicatePath(t *testing.T) {
	raw := []byte(`{"x":null,"nested":{"id":1,"id":2}}`)
	if v, ok := Get(raw, "x"); !ok || string(v) != "null" {
		t.Fatal("null lost")
	}
	if _, ok := Get(raw, "absent"); ok {
		t.Fatal("absence lost")
	}
	if _, ok := Get(raw, "nested", "id"); ok {
		t.Fatal("ambiguous path accepted")
	}
}

func FuzzExactPath(f *testing.F) {
	f.Add([]byte(`{"jsonrpc":"2.0","id":9007199254740993,"method":"x","params":{"nested":[{},null,"escaped\\\""]}}`))
	f.Add([]byte(`{"a":{"b":[1,2,"}"]}}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		if !json.Valid(raw) {
			return
		}
		_, _ = ParseFrame(raw)
		if value, ok := Get(raw, "id"); ok {
			replaced, err := Replace(raw, value, "id")
			if err != nil || !bytes.Equal(raw, replaced) {
				t.Fatalf("identity replacement: %v", err)
			}
		}
	})
}
