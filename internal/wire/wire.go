// Package wire handles MCP's newline-delimited JSON without reserializing
// application payloads. Only explicit protocol association paths may be edited.
package wire

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"
	"sync"
	"unicode/utf8"
)

const DefaultMaxFrameBytes = 16 << 20

var (
	ErrFrameTooLarge  = errors.New("MCP frame exceeds byte limit")
	ErrInvalidMessage = errors.New("invalid JSON-RPC message")
	ErrMissingPath    = errors.New("JSON path is missing or ambiguous")
)

// ID keeps the original spelling as well as an unambiguous, typed map key.
// Integers never pass through floating point. Zero-value ID is not a valid ID.
type ID struct {
	Raw json.RawMessage
	Key string
}

func ParseID(raw []byte) (ID, error) {
	raw = bytes.TrimSpace(raw)
	if !utf8.Valid(raw) || !json.Valid(raw) || len(raw) == 0 {
		return ID{}, fmt.Errorf("%w: invalid ID", ErrInvalidMessage)
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return ID{}, err
		}
		return ID{Raw: bytes.Clone(raw), Key: "s:" + s}, nil
	}
	key, ok := integerKey(string(raw))
	if !ok {
		return ID{}, fmt.Errorf("%w: ID must be a string or integer", ErrInvalidMessage)
	}
	return ID{Raw: bytes.Clone(raw), Key: "n:" + key}, nil
}

// Normalize mathematically equal JSON integer spellings without expanding an
// exponent or using floating point. Even a huge exponent consumes bounded space.
func integerKey(number string) (string, bool) {
	negative := strings.HasPrefix(number, "-")
	number = strings.TrimPrefix(number, "-")
	if len(number) == 0 || number[0] < '0' || number[0] > '9' {
		return "", false
	}
	exponent := new(big.Int)
	if at := strings.IndexAny(number, "eE"); at >= 0 {
		if _, ok := exponent.SetString(number[at+1:], 10); !ok {
			return "", false
		}
		number = number[:at]
	}
	if at := strings.IndexByte(number, '.'); at >= 0 {
		exponent.Sub(exponent, big.NewInt(int64(len(number)-at-1)))
		number = number[:at] + number[at+1:]
	}
	number = strings.TrimLeft(number, "0")
	if number == "" {
		return "0", true
	}
	trimmed := strings.TrimRight(number, "0")
	exponent.Add(exponent, big.NewInt(int64(len(number)-len(trimmed))))
	if exponent.Sign() < 0 {
		return "", false
	}
	key := trimmed
	if negative {
		key = "-" + key
	}
	if exponent.Sign() != 0 {
		key += "e" + exponent.String()
	}
	return key, true
}

func StringID(value string) ID {
	raw, _ := json.Marshal(value)
	return ID{Raw: raw, Key: "s:" + value}
}

type Kind uint8

const (
	Request Kind = iota + 1
	Notification
	Response
)

type Message struct {
	Raw    []byte
	Method string
	ID     ID
	HasID  bool
	Kind   Kind
}

type Frame struct {
	Raw      []byte
	Messages []Message
	Batch    bool
}

// ParseFrame recognizes only the JSON-RPC envelope. No business fields are
// normalized or interpreted. Profile-specific batch permission is a caller duty.
func ParseFrame(raw []byte) (Frame, error) {
	if !utf8.Valid(raw) || !json.Valid(raw) {
		return Frame{}, fmt.Errorf("%w: malformed JSON or UTF-8", ErrInvalidMessage)
	}
	f := Frame{Raw: bytes.Clone(raw)}
	trimmed := bytes.TrimSpace(f.Raw)
	var members []json.RawMessage
	if trimmed[0] == '[' {
		f.Batch = true
		if err := json.Unmarshal(trimmed, &members); err != nil {
			return Frame{}, err
		}
		if len(members) == 0 {
			return Frame{}, fmt.Errorf("%w: empty batch", ErrInvalidMessage)
		}
	} else {
		members = []json.RawMessage{f.Raw}
	}
	for _, member := range members {
		m, err := ParseMessage(member)
		if err != nil {
			return Frame{}, err
		}
		f.Messages = append(f.Messages, m)
	}
	return f, nil
}

func ParseMessage(raw []byte) (Message, error) {
	if !utf8.Valid(raw) || !json.Valid(raw) {
		return Message{}, ErrInvalidMessage
	}
	raw = bytes.Clone(raw)
	if !uniqueEnvelopeKeys(raw) {
		return Message{}, fmt.Errorf("%w: duplicate envelope key", ErrInvalidMessage)
	}
	v, ok := Get(raw, "jsonrpc")
	var revision string
	if !ok || json.Unmarshal(v, &revision) != nil || revision != "2.0" {
		return Message{}, fmt.Errorf("%w: jsonrpc must be 2.0", ErrInvalidMessage)
	}
	m := Message{Raw: raw}
	id, hasID := Get(raw, "id")
	method, hasMethod := Get(raw, "method")
	_, hasResult := Get(raw, "result")
	_, hasError := Get(raw, "error")
	if hasID && string(id) != "null" {
		var err error
		m.ID, err = ParseID(id)
		if err != nil {
			return Message{}, err
		}
		m.HasID = true
	}
	if hasMethod {
		if hasResult || hasError || json.Unmarshal(method, &m.Method) != nil {
			return Message{}, ErrInvalidMessage
		}
		if hasID {
			if !m.HasID {
				return Message{}, fmt.Errorf("%w: request ID is null", ErrInvalidMessage)
			}
			m.Kind = Request
		} else {
			m.Kind = Notification
		}
	} else {
		if hasResult == hasError || (hasResult && !m.HasID) {
			return Message{}, ErrInvalidMessage
		}
		m.Kind = Response
	}
	return m, nil
}

func uniqueEnvelopeKeys(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return false
	}
	seen := make(map[string]bool)
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return false
		}
		name, ok := key.(string)
		if !ok || seen[name] {
			return false
		}
		seen[name] = true
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return false
		}
	}
	return true
}

// Join preserves individual message bytes. An empty batch is never emitted.
func Join(messages [][]byte, batch bool) ([]byte, error) {
	if len(messages) == 0 {
		return nil, fmt.Errorf("%w: empty frame", ErrInvalidMessage)
	}
	if !batch {
		if len(messages) != 1 {
			return nil, ErrInvalidMessage
		}
		return bytes.Clone(messages[0]), nil
	}
	return append(append([]byte{'['}, bytes.Join(messages, []byte{','})...), ']'), nil
}

type Reader struct {
	reader *bufio.Reader
	limit  int
}

func NewReader(reader io.Reader, maxBytes int) *Reader {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxFrameBytes
	}
	return &Reader{reader: bufio.NewReaderSize(reader, min(maxBytes+1, 32<<10)), limit: maxBytes}
}
func (r *Reader) ReadFrame() ([]byte, error) {
	var frame []byte
	for {
		part, err := r.reader.ReadSlice('\n')
		complete := len(part) > 0 && part[len(part)-1] == '\n'
		if complete {
			part = part[:len(part)-1]
		}
		// Allow the optional CR in a CRLF delimiter without counting it as
		// payload. At most one delimiter byte is retained beyond the budget.
		if len(frame)+len(part) > r.limit && (len(frame)+len(part) != r.limit+1 || (len(part) > 0 && part[len(part)-1] != '\r')) {
			return nil, ErrFrameTooLarge
		}
		frame = append(frame, part...)
		if complete {
			if len(frame) > 0 && frame[len(frame)-1] == '\r' {
				frame = frame[:len(frame)-1]
			}
			if len(frame) > r.limit {
				return nil, ErrFrameTooLarge
			}
			return frame, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && len(frame) > 0 {
			return nil, io.ErrUnexpectedEOF
		}
		if err != nil {
			return nil, err
		}
	}
}

// Writer serializes whole frames. Its underlying writer must be closeable by
// the owner to interrupt a blocked write; cancellation alone cannot close it.
type Writer struct {
	writer io.Writer
	limit  int
	mu     sync.Mutex
}

func NewWriter(writer io.Writer, maxBytes int) *Writer {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxFrameBytes
	}
	return &Writer{writer: writer, limit: maxBytes}
}
func (w *Writer) WriteFrame(frame []byte) error {
	if len(frame) > w.limit {
		return ErrFrameTooLarge
	}
	if bytes.ContainsAny(frame, "\r\n") {
		return fmt.Errorf("%w: frame contains a literal line break", ErrInvalidMessage)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := WriteFull(w.writer, frame); err != nil {
		return err
	}
	return WriteFull(w.writer, []byte{'\n'})
}
func WriteFull(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if n < 0 || n > len(data) {
			return io.ErrShortWrite
		}
		data = data[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

// Get returns only the exact object-key path. It never recursively searches a
// payload. Duplicate keys on that path are ambiguous and reported as absent.
func Get(raw []byte, path ...string) (json.RawMessage, bool) {
	if !utf8.Valid(raw) || !json.Valid(raw) {
		return nil, false
	}
	start, end, ok := locate(raw, path)
	if !ok {
		return nil, false
	}
	return raw[start:end], true
}

// Replace preserves every byte outside the selected value (including unknown
// fields, member order and whitespace). It cannot add a missing association.
func Replace(raw, replacement []byte, path ...string) ([]byte, error) {
	if !utf8.Valid(raw) || !json.Valid(raw) || !json.Valid(replacement) || !utf8.Valid(replacement) {
		return nil, ErrInvalidMessage
	}
	start, end, ok := locate(raw, path)
	if !ok {
		return nil, ErrMissingPath
	}
	out := make([]byte, 0, len(raw)-(end-start)+len(replacement))
	out = append(out, raw[:start]...)
	out = append(out, replacement...)
	out = append(out, raw[end:]...)
	return out, nil
}

func locate(raw []byte, path []string) (int, int, bool) {
	start, end := skipSpace(raw, 0), len(raw)
	for end > start && isSpace(raw[end-1]) {
		end--
	}
	for _, key := range path {
		if start >= end || raw[start] != '{' {
			return 0, 0, false
		}
		found, nextStart, nextEnd := false, 0, 0
		for i := skipSpace(raw, start+1); i < end && raw[i] != '}'; {
			keyEnd := stringEnd(raw, i)
			var current string
			if json.Unmarshal(raw[i:keyEnd], &current) != nil {
				return 0, 0, false
			}
			valueStart := skipSpace(raw, skipSpace(raw, keyEnd)+1)
			valueEnd := endValue(raw, valueStart)
			if current == key {
				if found {
					return 0, 0, false
				}
				found, nextStart, nextEnd = true, valueStart, valueEnd
			}
			i = skipSpace(raw, valueEnd)
			if i < end && raw[i] == ',' {
				i = skipSpace(raw, i+1)
			}
		}
		if !found {
			return 0, 0, false
		}
		start, end = nextStart, nextEnd
	}
	return start, end, true
}
func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }
func skipSpace(raw []byte, i int) int {
	for i < len(raw) && isSpace(raw[i]) {
		i++
	}
	return i
}
func stringEnd(raw []byte, i int) int {
	for i++; i < len(raw); i++ {
		if raw[i] == '\\' {
			i++
			continue
		}
		if raw[i] == '"' {
			return i + 1
		}
	}
	return len(raw)
}
func endValue(raw []byte, i int) int {
	if raw[i] == '"' {
		return stringEnd(raw, i)
	}
	if raw[i] == '{' || raw[i] == '[' {
		depth := 0
		for ; i < len(raw); i++ {
			switch raw[i] {
			case '"':
				i = stringEnd(raw, i) - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
		}
		return i
	}
	for i < len(raw) && !isSpace(raw[i]) && raw[i] != ',' && raw[i] != '}' && raw[i] != ']' {
		i++
	}
	return i
}
