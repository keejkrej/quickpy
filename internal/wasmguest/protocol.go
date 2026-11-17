package wasmguest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

// Output is one ordered guest output entry. M0 only needs text and console;
// image output is deliberately not part of the transient ABI yet.
type Output struct {
	Type  string `json:"type"`
	Text  string `json:"text,omitempty"`
	Level string `json:"level,omitempty"`
}

// Exception is the structured failure detail a guest may return with a
// "failed" response.
type Exception struct {
	Type      string `json:"type"`
	Message   string `json:"message"`
	Traceback string `json:"traceback"`
	Filename  string `json:"filename"`
	Line      int    `json:"line"`
}

// CallRequest is one guest tool request. IDs are safe JSON integers, unique
// and monotonic per execution.
type CallRequest struct {
	ID   int64          `json:"id"`
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

// Response is the decoded qp_invoke response.
type Response struct {
	Version         int
	Type            string
	Outputs         []Output
	Exception       *Exception
	OutputTruncated bool
	Reason          string
	Calls           []CallRequest
	Awaiting        []int64
	Stalled         bool
	UnknownFields   []string
}

var topLevelKeys = map[string]bool{
	"v":                true,
	"type":             true,
	"output":           true,
	"outputs":          true,
	"output_truncated": true,
	"error":            true,
	"reason":           true,
	"calls":            true,
	"awaiting":         true,
	"stalled":          true,
}

var callKeys = map[string]bool{
	"id":   true,
	"name": true,
	"args": true,
}

var outputKeys = map[string]bool{
	"type":  true,
	"text":  true,
	"level": true,
}

var exceptionKeys = map[string]bool{
	"type":      true,
	"message":   true,
	"traceback": true,
	"filename":  true,
	"line":      true,
}

// decodeResponse decodes a guest response. It tolerates unknown fields (and
// reports them) but rejects malformed JSON, duplicate keys, wrong protocol
// versions, wrong message types, unknown output types, and oversized output.
func decodeResponse(data []byte, maxOutputBytes int) (*Response, *Error) {
	if len(data) == 0 {
		return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "empty guest response", nil)
	}
	if err := checkDuplicateKeys(data); err != nil {
		return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "malformed guest response: "+err.Error(), err)
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "malformed guest response: "+err.Error(), err)
	}
	if top == nil {
		return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "guest response is not a JSON object", nil)
	}

	resp := &Response{}
	resp.UnknownFields = append(resp.UnknownFields, unknownKeys(top, topLevelKeys, "")...)

	vRaw, ok := top["v"]
	if !ok {
		return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "guest response is missing v", nil)
	}
	if err := json.Unmarshal(vRaw, &resp.Version); err != nil {
		return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "guest response v is not an integer", err)
	}
	if resp.Version != 0 {
		return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", fmt.Sprintf("unsupported guest protocol version %d (want 0)", resp.Version), nil)
	}

	typeRaw, ok := top["type"]
	if !ok {
		return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "guest response is missing type", nil)
	}
	if err := json.Unmarshal(typeRaw, &resp.Type); err != nil {
		return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "guest response type is not a string", err)
	}
	switch resp.Type {
	case "completed", "failed", "suspended":
	default:
		return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", fmt.Sprintf("unsupported guest response type %q", resp.Type), nil)
	}

	if raw, ok := top["stalled"]; ok && !isJSONNull(raw) {
		if err := json.Unmarshal(raw, &resp.Stalled); err != nil {
			return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "guest response stalled is not a boolean", err)
		}
	}

	if resp.Type == "suspended" {
		reasonRaw, ok := top["reason"]
		if !ok {
			return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "suspended response is missing reason", nil)
		}
		if err := json.Unmarshal(reasonRaw, &resp.Reason); err != nil {
			return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "suspended response reason is not a string", err)
		}
		switch resp.Reason {
		case "awaiting_calls", "runnable", "stalled":
		default:
			return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", fmt.Sprintf("unsupported suspension reason %q", resp.Reason), nil)
		}

		if raw, ok := top["calls"]; ok && !isJSONNull(raw) {
			var rawCalls []json.RawMessage
			if err := json.Unmarshal(raw, &rawCalls); err != nil {
				return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "suspended response calls is not an array", err)
			}
			seen := map[int64]bool{}
			for i, rawCall := range rawCalls {
				var m map[string]json.RawMessage
				if err := json.Unmarshal(rawCall, &m); err != nil {
					return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", fmt.Sprintf("suspended response calls[%d] is not an object", i), err)
				}
				if m == nil {
					return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", fmt.Sprintf("suspended response calls[%d] is not an object", i), nil)
				}
				resp.UnknownFields = append(resp.UnknownFields, unknownKeys(m, callKeys, fmt.Sprintf("calls[%d].", i))...)

				var id int64
				if err := json.Unmarshal(m["id"], &id); err != nil {
					return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", fmt.Sprintf("suspended response calls[%d].id is not an integer", i), err)
				}
				if id <= 0 {
					return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", fmt.Sprintf("suspended response calls[%d].id must be positive", i), nil)
				}
				if seen[id] {
					return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", fmt.Sprintf("duplicate call id %d in suspended response", id), nil)
				}
				seen[id] = true

				var name string
				if err := json.Unmarshal(m["name"], &name); err != nil {
					return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", fmt.Sprintf("suspended response calls[%d].name is not a string", i), err)
				}
				if name == "" {
					return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", fmt.Sprintf("suspended response calls[%d].name is empty", i), nil)
				}

				args := map[string]any{}
				if rawArgs, ok := m["args"]; ok && !isJSONNull(rawArgs) {
					decoded, err := decodeArgs(rawArgs)
					if err != nil {
						return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", fmt.Sprintf("suspended response calls[%d].args: %v", i, err), err)
					}
					args = decoded
				}
				resp.Calls = append(resp.Calls, CallRequest{ID: id, Name: name, Args: args})
			}
		}

		if raw, ok := top["awaiting"]; ok && !isJSONNull(raw) {
			if err := json.Unmarshal(raw, &resp.Awaiting); err != nil {
				return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "suspended response awaiting is not an integer array", err)
			}
			seen := map[int64]bool{}
			for _, id := range resp.Awaiting {
				if id <= 0 {
					return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "suspended response awaiting contains a non-positive id", nil)
				}
				if seen[id] {
					return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", fmt.Sprintf("duplicate awaiting id %d", id), nil)
				}
				seen[id] = true
			}
		}

		switch resp.Reason {
		case "stalled":
			if !resp.Stalled {
				return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "stalled suspension requires stalled:true", nil)
			}
			if len(resp.Calls) != 0 {
				return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "stalled suspension must not include new calls", nil)
			}
			if len(resp.Awaiting) != 0 {
				return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "stalled suspension must have empty awaiting", nil)
			}
		case "awaiting_calls":
			if resp.Stalled {
				return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "awaiting_calls suspension must not set stalled:true", nil)
			}
			if len(resp.Awaiting) == 0 {
				return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "awaiting_calls suspension must have non-empty awaiting", nil)
			}
		case "runnable":
			if resp.Stalled {
				return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "runnable suspension must not set stalled:true", nil)
			}
			if len(resp.Awaiting) != 0 {
				return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "runnable suspension must have empty awaiting", nil)
			}
		}
		awaitingSet := map[int64]bool{}
		for _, id := range resp.Awaiting {
			awaitingSet[id] = true
		}
		for _, c := range resp.Calls {
			if !awaitingSet[c.ID] {
				return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", fmt.Sprintf("new call %d is not listed in awaiting", c.ID), nil)
			}
		}
		if resp.Exception != nil {
			return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "suspended response must not include error details", nil)
		}
	} else if resp.Reason != "" || len(resp.Calls) > 0 || len(resp.Awaiting) > 0 || resp.Stalled {
		return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "non-suspended response must not include suspension fields", nil)
	}

	outputRaw, hasOutput := top["output"]
	outputsRaw, hasOutputs := top["outputs"]
	if hasOutput && hasOutputs && !isJSONNull(outputRaw) && !isJSONNull(outputsRaw) {
		return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "guest response must not contain both output and outputs", nil)
	}
	if hasOutput && !isJSONNull(outputRaw) {
		var text string
		if err := json.Unmarshal(outputRaw, &text); err != nil {
			return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "guest response output is not a string", err)
		}
		if len(text) > maxOutputBytes {
			return nil, newError(KindLimitExceeded, StatusLimitExceeded, "decode", "", fmt.Sprintf("guest response output exceeds %d bytes", maxOutputBytes), nil)
		}
		resp.Outputs = append(resp.Outputs, Output{Type: "text", Text: text})
	}

	if raw, ok := top["output_truncated"]; ok && !isJSONNull(raw) {
		if err := json.Unmarshal(raw, &resp.OutputTruncated); err != nil {
			return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "guest response output_truncated is not a boolean", err)
		}
	}

	if raw, ok := top["outputs"]; ok && !isJSONNull(raw) {
		var rawOutputs []json.RawMessage
		if err := json.Unmarshal(raw, &rawOutputs); err != nil {
			return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "guest response outputs is not an array", err)
		}
		total := 0
		for i, rawOut := range rawOutputs {
			var m map[string]json.RawMessage
			if err := json.Unmarshal(rawOut, &m); err != nil {
				return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", fmt.Sprintf("guest response outputs[%d] is not an object", i), err)
			}
			if m == nil {
				return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", fmt.Sprintf("guest response outputs[%d] is not an object", i), nil)
			}
			resp.UnknownFields = append(resp.UnknownFields, unknownKeys(m, outputKeys, fmt.Sprintf("outputs[%d].", i))...)

			var outType string
			if err := json.Unmarshal(m["type"], &outType); err != nil {
				return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", fmt.Sprintf("guest response outputs[%d].type is not a string", i), err)
			}
			switch outType {
			case "text":
				var text string
				if err := json.Unmarshal(m["text"], &text); err != nil {
					return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", fmt.Sprintf("guest response outputs[%d].text is not a string", i), err)
				}
				total += len(text)
				resp.Outputs = append(resp.Outputs, Output{Type: "text", Text: text})
			case "console":
				var text string
				if err := json.Unmarshal(m["text"], &text); err != nil {
					return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", fmt.Sprintf("guest response outputs[%d].text is not a string", i), err)
				}
				var level string
				if rawLevel, ok := m["level"]; ok {
					if err := json.Unmarshal(rawLevel, &level); err != nil {
						return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", fmt.Sprintf("guest response outputs[%d].level is not a string", i), err)
					}
				}
				total += len(text)
				resp.Outputs = append(resp.Outputs, Output{Type: "console", Text: text, Level: level})
			default:
				return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", fmt.Sprintf("unsupported guest output type %q", outType), nil)
			}
			if total > maxOutputBytes {
				return nil, newError(KindLimitExceeded, StatusLimitExceeded, "decode", "", fmt.Sprintf("guest response output exceeds %d bytes", maxOutputBytes), nil)
			}
		}
	}

	if raw, ok := top["error"]; ok && !isJSONNull(raw) {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "guest response error is not an object", err)
		}
		if m == nil {
			return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "guest response error is not an object", nil)
		}
		resp.UnknownFields = append(resp.UnknownFields, unknownKeys(m, exceptionKeys, "error.")...)

		ex := &Exception{}
		if rawType, ok := m["type"]; ok {
			if err := json.Unmarshal(rawType, &ex.Type); err != nil {
				return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "guest response error.type is not a string", err)
			}
		}
		if ex.Type == "" {
			return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "guest response error is missing type", nil)
		}
		if rawMessage, ok := m["message"]; ok {
			if err := json.Unmarshal(rawMessage, &ex.Message); err != nil {
				return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "guest response error.message is not a string", err)
			}
		}
		if rawTraceback, ok := m["traceback"]; ok {
			if err := json.Unmarshal(rawTraceback, &ex.Traceback); err != nil {
				return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "guest response error.traceback is not a string", err)
			}
		}
		if rawFilename, ok := m["filename"]; ok {
			if err := json.Unmarshal(rawFilename, &ex.Filename); err != nil {
				return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "guest response error.filename is not a string", err)
			}
		}
		if rawLine, ok := m["line"]; ok {
			if err := json.Unmarshal(rawLine, &ex.Line); err != nil {
				return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "guest response error.line is not an integer", err)
			}
		}
		resp.Exception = ex
	}

	sort.Strings(resp.UnknownFields)
	return resp, nil
}

func unknownKeys(m map[string]json.RawMessage, known map[string]bool, prefix string) []string {
	var out []string
	for k := range m {
		if !known[k] {
			out = append(out, prefix+k)
		}
	}
	return out
}

func isJSONNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

// checkDuplicateKeys walks the JSON token stream and rejects duplicate object
// keys. encoding/json otherwise silently keeps the last value.
func checkDuplicateKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := walkJSON(dec, 0); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return err
	}
	return nil
}

const maxJSONDepth = 256

func walkJSON(dec *json.Decoder, depth int) error {
	if depth > maxJSONDepth {
		return fmt.Errorf("JSON nesting exceeds %d", maxJSONDepth)
	}
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]struct{}{}
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := keyTok.(string)
			if !ok {
				return fmt.Errorf("object key is not a string")
			}
			if _, dup := seen[key]; dup {
				return fmt.Errorf("duplicate object key %q", key)
			}
			seen[key] = struct{}{}
			if err := walkJSON(dec, depth+1); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return fmt.Errorf("expected end of object")
		}
	case '[':
		for dec.More() {
			if err := walkJSON(dec, depth+1); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return fmt.Errorf("expected end of array")
		}
	default:
		return fmt.Errorf("unexpected delimiter %q", delim)
	}
	return nil
}
