// Package testbackend implements an intentionally non-refreshing MCP stdio peer.
// It is a test fixture, independent of the production wire/protocol packages.
package testbackend

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Config names inputs to snapshot once and an evidence log outside those inputs.
type Config struct {
	Inputs []string
	Log    string
	Args   []string
}
type Event struct {
	Kind   string          `json:"kind"`
	Boot   string          `json:"boot"`
	PID    int             `json:"pid"`
	Method string          `json:"method,omitempty"`
	ID     json.RawMessage `json:"id,omitempty"`
	Raw    string          `json:"raw,omitempty"`
}
type Snapshot struct {
	Boot        string             `json:"boot"`
	PID         int                `json:"pid"`
	Reads       int                `json:"reads"`
	Inputs      map[string]*string `json:"inputs"`
	Args        []string           `json:"args"`
	CWD         string             `json:"cwd"`
	Environment map[string]string  `json:"environment"`
	Initialize  json.RawMessage    `json:"initialize,omitempty"`
}

// Run never rereads Config.Inputs. Only fixture/write explicitly mutates one.
func Run(in io.Reader, out io.Writer, cfg Config) error {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	cwd, _ := os.Getwd()
	snap := Snapshot{Boot: hex.EncodeToString(random[:]), PID: os.Getpid(), Inputs: map[string]*string{}, Args: cfg.Args, CWD: cwd, Environment: map[string]string{}}
	for _, key := range []string{"PATH", "GOWORK", "GOENV", "FIXTURE_CUSTOM_ENV"} {
		snap.Environment[key] = os.Getenv(key)
	}
	for _, path := range cfg.Inputs {
		data, err := os.ReadFile(path)
		snap.Reads++
		if os.IsNotExist(err) {
			snap.Inputs[filepath.Base(path)] = nil
			continue
		}
		if err != nil {
			return err
		}
		value := string(data)
		snap.Inputs[filepath.Base(path)] = &value
	}
	log, err := os.OpenFile(cfg.Log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	record := func(kind, method string, id json.RawMessage, raw []byte) error {
		return json.NewEncoder(log).Encode(Event{Kind: kind, Boot: snap.Boot, PID: snap.PID, Method: method, ID: id, Raw: string(raw)})
	}
	if err := record("boot", "", nil, nil); err != nil {
		return err
	}
	write := func(raw []byte) error {
		if err := record("sent", "", nil, raw); err != nil {
			return err
		}
		_, err := fmt.Fprintf(out, "%s\n", raw)
		return err
	}
	result := func(id json.RawMessage, value any) []byte {
		b, _ := json.Marshal(value)
		return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, id, b))
	}
	reverse := map[string]json.RawMessage{}
	subscriptions := map[string]json.RawMessage{}
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 4096), 32<<20)
	for scanner.Scan() {
		frame := append([]byte(nil), scanner.Bytes()...)
		if err := record("receipt", "", nil, frame); err != nil {
			return err
		}
		messages := []json.RawMessage{frame}
		batch := len(strings.TrimSpace(string(frame))) > 0 && strings.TrimSpace(string(frame))[0] == '['
		if batch {
			if err := json.Unmarshal(frame, &messages); err != nil {
				return err
			}
		}
		var responses []json.RawMessage
		for _, raw := range messages {
			var msg struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
				Result json.RawMessage `json:"result"`
			}
			if err := json.Unmarshal(raw, &msg); err != nil {
				return err
			}
			if msg.Method == "" {
				if original, ok := reverse[string(msg.ID)]; ok {
					responses = append(responses, result(original, json.RawMessage(msg.Result)))
					delete(reverse, string(msg.ID))
				}
				continue
			}
			if len(msg.ID) == 0 {
				if msg.Method == "notifications/cancelled" {
					var params struct {
						RequestID json.RawMessage `json:"requestId"`
					}
					_ = json.Unmarshal(msg.Params, &params)
					delete(subscriptions, string(params.RequestID))
				}
				continue
			}
			if err := record("execution", msg.Method, msg.ID, raw); err != nil {
				return err
			}
			var response []byte
			switch msg.Method {
			case "initialize":
				snap.Initialize = append([]byte(nil), raw...)
				var params struct {
					ProtocolVersion string `json:"protocolVersion"`
				}
				_ = json.Unmarshal(msg.Params, &params)
				response = result(msg.ID, map[string]any{"protocolVersion": params.ProtocolVersion, "serverInfo": map[string]string{"name": "raw-snapshot-fixture", "version": "1"}, "capabilities": map[string]any{"tools": map[string]bool{"listChanged": true}, "resources": map[string]bool{"subscribe": true, "listChanged": true}, "logging": map[string]any{}}, "instructions": "Fixture reads inputs only at boot."})
			case "server/discover":
				var params struct {
					ErrorCode int `json:"fixtureErrorCode"`
				}
				_ = json.Unmarshal(msg.Params, &params)
				if params.ErrorCode != 0 {
					response = []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":%d,"message":"fixture discovery rejected","data":{"opaque":900719925474099312345}}}`, msg.ID, params.ErrorCode))
					break
				}
				response = result(msg.ID, map[string]any{"protocolVersions": []string{"2026-07-28"}, "serverInfo": map[string]string{"name": "raw-snapshot-fixture", "version": "1"}, "capabilities": map[string]any{}})
			case "tools/call":
				var params struct {
					Name string `json:"name"`
				}
				if err := json.Unmarshal(msg.Params, &params); err != nil || params.Name != "future_tool" {
					response = []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32602,"message":"unknown fixture tool"}}`, msg.ID))
					break
				}
				text, _ := json.Marshal(snap)
				response = result(msg.ID, map[string]any{
					"content":           []map[string]string{{"type": "text", "text": string(text)}},
					"structuredContent": snap,
				})
			case "fixture/snapshot":
				response = result(msg.ID, snap)
			case "fixture/echo":
				params := msg.Params
				if len(params) == 0 {
					params = []byte(`null`)
				}
				response = []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s,"x-fixture":{"untouched":900719925474099312345}}`, msg.ID, params))
			case "fixture/error":
				response = []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32042,"message":"fixture failure","data":{"opaque":null,"large":900719925474099312345}},"_meta":{"future":[1,null]}}`, msg.ID))
			case "fixture/tool-error":
				response = []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"isError":true,"content":[{"type":"future","raw":null}],"structuredContent":{"n":900719925474099312345},"_meta":{"opaque":true}}}`, msg.ID))
			case "subscriptions/listen":
				subscriptions[string(msg.ID)] = append([]byte(nil), msg.ID...)
				ack := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/subscriptions/acknowledged","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":%s},"notifications":{"toolsListChanged":true}}}`, msg.ID))
				if err := write(ack); err != nil {
					return err
				}
				continue
			case "fixture/subscriptionComplete":
				var params struct {
					SubscriptionID json.RawMessage `json:"subscriptionId"`
				}
				_ = json.Unmarshal(msg.Params, &params)
				if id, ok := subscriptions[string(params.SubscriptionID)]; ok {
					completion := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete","_meta":{"io.modelcontextprotocol/subscriptionId":%s}}}`, id, id))
					if err := write(completion); err != nil {
						return err
					}
					delete(subscriptions, string(id))
				}
				response = result(msg.ID, map[string]any{})
			case "fixture/subscriptionEvent":
				for _, id := range subscriptions {
					notification := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":%s},"fixture":"event"}}`, id))
					if err := write(notification); err != nil {
						return err
					}
				}
				response = result(msg.ID, map[string]any{})
			case "fixture/mrtr":
				var params struct {
					RequestState json.RawMessage `json:"requestState"`
				}
				_ = json.Unmarshal(msg.Params, &params)
				if len(params.RequestState) == 0 {
					response = result(msg.ID, map[string]any{"resultType": "input_required", "requestState": map[string]any{"boot": snap.Boot, "opaque": []any{"leave untouched", nil}}, "inputRequests": []any{map[string]any{"type": "roots/list"}}})
				} else {
					var state struct {
						Boot string `json:"boot"`
					}
					_ = json.Unmarshal(params.RequestState, &state)
					if state.Boot != snap.Boot {
						response = []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32044,"message":"fixture token belongs to old process","data":{"requestState":%s}}}`, msg.ID, params.RequestState))
					} else {
						response = result(msg.ID, map[string]any{"resultType": "complete", "requestState": params.RequestState})
					}
				}
			case "fixture/hold":
				continue
			case "fixture/half":
				_, _ = io.WriteString(out, `{"jsonrpc":"2.0","id":`)
				return fmt.Errorf("requested fixture exit during response frame")
			case "fixture/crash":
				return fmt.Errorf("requested fixture crash after receipt")
			case "fixture/write":
				var params struct {
					Path    string `json:"path"`
					Content string `json:"content"`
				}
				if err := json.Unmarshal(msg.Params, &params); err != nil {
					return err
				}
				if err := os.WriteFile(params.Path, []byte(params.Content), 0600); err != nil {
					return err
				}
				response = result(msg.ID, map[string]bool{"written": true})
			case "fixture/reverse":
				reverse["1"] = append([]byte(nil), msg.ID...)
				if err := write([]byte(`{"jsonrpc":"2.0","id":1,"method":"roots/list","params":{"_meta":{"fixture":true}}}`)); err != nil {
					return err
				}
				continue
			case "tools/list":
				response = []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"future_tool","inputSchema":{"oneOf":[{"type":"null"},{"type":"array"}]},"annotations":{"opaque":true},"x-future":{"n":900719925474099312345}}],"_meta":{"opaque":null}}}`, msg.ID))
			case "resources/list":
				response = result(msg.ID, map[string]any{"resources": []any{}})
			case "prompts/list":
				response = result(msg.ID, map[string]any{"prompts": []any{}})
			case "resources/subscribe", "resources/unsubscribe", "logging/setLevel", "ping":
				response = result(msg.ID, map[string]any{})
			default:
				response = result(msg.ID, map[string]any{"unknownMethod": msg.Method, "params": json.RawMessage(msg.Params)})
			}
			responses = append(responses, response)
		}
		if len(responses) > 0 {
			if batch {
				raw, _ := json.Marshal(responses)
				if err := write(raw); err != nil {
					return err
				}
			} else {
				for _, raw := range responses {
					if err := write(raw); err != nil {
						return err
					}
				}
			}
		}
	}
	return scanner.Err()
}
