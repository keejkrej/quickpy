package wasmguest

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"
)

// Output is one ordered guest output entry. ABI 0.4 adds structured console
// and image entries. Image data is stored as raw decoded bytes, never as the
// base64 text that appears on the wire.
type Output struct {
	Type  string `json:"type"`
	Text  string `json:"text,omitempty"`
	Level string `json:"level,omitempty"`
	MIME  string `json:"mime,omitempty"`
	Data  []byte `json:"data,omitempty"`
}

// ToolCause is the structured detail of an uncaught guest ToolError. It is
// only present on a failed response when a tool failure ended the script.
// CallID is optional and opaque; it is a diagnostic correlation value, not a
// resumable handle.
type ToolCause struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Tool    string `json:"tool,omitempty"`
	CallID  *int64 `json:"call_id,omitempty"`
}

// Exception is the structured failure detail a guest may return with a
// "failed" response.
type Exception struct {
	Type      string     `json:"type"`
	Message   string     `json:"message"`
	Traceback string     `json:"traceback"`
	Filename  string     `json:"filename"`
	Line      int        `json:"line"`
	ToolCause *ToolCause `json:"tool_cause,omitempty"`
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

	// EngineName and EngineVersion are optional ABI 0.4 guest identity
	// metadata. They are reported for diagnostics and never used for
	// control flow.
	EngineName    string
	EngineVersion string

	// StateWritten and StateRaw are only meaningful on a completed response.
	// StateRaw is the raw JSON value of the state field; it is decoded by the
	// execution owner with the resolved state bound so a malformed or
	// oversized final value is rejected without committing anything.
	StateWritten bool
	StateRaw     json.RawMessage
	StatePresent bool
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
	"state":            true,
	"state_written":    true,
	"engine_name":      true,
	"engine_version":   true,
}

var callKeys = map[string]bool{
	"id":   true,
	"name": true,
	"args": true,
}

var outputKeys = map[string]bool{
	"type":        true,
	"text":        true,
	"level":       true,
	"mime":        true,
	"data_base64": true,
}

var exceptionKeys = map[string]bool{
	"type":       true,
	"message":    true,
	"traceback":  true,
	"filename":   true,
	"line":       true,
	"tool_cause": true,
}

var toolCauseKeys = map[string]bool{
	"code":    true,
	"message": true,
	"tool":    true,
	"call_id": true,
}

// decodeResponse decodes a guest response without a per-image allocation
// bound. It is used by tests and by callers that only need shape validation.
func decodeResponse(data []byte) (*Response, *Error) {
	return decodeResponseWithMessageLimit(data, 0)
}

// decodeResponseWithMessageLimit decodes a guest response. It tolerates
// unknown fields (and reports them) but rejects malformed JSON, duplicate
// keys, wrong protocol versions, wrong message types, invalid UTF-8, invalid
// console levels, invalid MIME types, invalid base64, and unknown output
// types.
//
// maxImageBytes, when positive, is a message-derived allocation safety bound
// for one decoded image. It is not the public output cap: a valid image larger
// than MaxOutputBytes is decoded and then clipped by the execution owner as
// part of cumulative output accounting, yielding LimitExceeded. The bound
// exists only to avoid allocating an image larger than the protocol frame that
// carried it.
func decodeResponseWithMessageLimit(data []byte, maxImageBytes int) (*Response, *Error) {
	if len(data) == 0 {
		return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "empty guest response", nil)
	}
	if !utf8.Valid(data) {
		return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "guest response is not valid UTF-8", nil)
	}
	if err := validateJSONStringEscapes(data); err != nil {
		return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "malformed guest response: "+err.Error(), err)
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

	if raw, ok := top["engine_name"]; ok && !isJSONNull(raw) {
		if err := json.Unmarshal(raw, &resp.EngineName); err != nil {
			return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "guest response engine_name is not a string", err)
		}
	}
	if raw, ok := top["engine_version"]; ok && !isJSONNull(raw) {
		if err := json.Unmarshal(raw, &resp.EngineVersion); err != nil {
			return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "guest response engine_version is not a string", err)
		}
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
	// An explicit outputs array always wins. The legacy output string is a
	// text-only aggregate that the guest still emits for older consumers; it
	// must never be counted a second time when outputs is present. An empty
	// outputs array therefore means "no output", not "fall back to output".
	if hasOutputs && !isJSONNull(outputsRaw) {
		var rawOutputs []json.RawMessage
		if err := json.Unmarshal(outputsRaw, &rawOutputs); err != nil {
			return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "guest response outputs is not an array", err)
		}
		for i, rawOut := range rawOutputs {
			var m map[string]json.RawMessage
			if err := json.Unmarshal(rawOut, &m); err != nil {
				return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", fmt.Sprintf("guest response outputs[%d] is not an object", i), err)
			}
			if m == nil {
				return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", fmt.Sprintf("guest response outputs[%d] is not an object", i), nil)
			}
			resp.UnknownFields = append(resp.UnknownFields, unknownKeys(m, outputKeys, fmt.Sprintf("outputs[%d].", i))...)

			outType, oerr := decodeStringField(m, "type", fmt.Sprintf("guest response outputs[%d].type", i))
			if oerr != nil {
				return nil, oerr
			}
			switch outType {
			case "text":
				text, oerr := decodeStringField(m, "text", fmt.Sprintf("guest response outputs[%d].text", i))
				if oerr != nil {
					return nil, oerr
				}
				resp.Outputs = append(resp.Outputs, Output{Type: "text", Text: text})
			case "console":
				text, oerr := decodeStringField(m, "text", fmt.Sprintf("guest response outputs[%d].text", i))
				if oerr != nil {
					return nil, oerr
				}
				level, oerr := decodeStringField(m, "level", fmt.Sprintf("guest response outputs[%d].level", i))
				if oerr != nil {
					return nil, oerr
				}
				if !validConsoleLevel(level) {
					return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", fmt.Sprintf("guest response outputs[%d].level %q is not one of debug, info, warn, error", i, level), nil)
				}
				resp.Outputs = append(resp.Outputs, Output{Type: "console", Text: text, Level: level})
			case "image":
				mime, oerr := decodeStringField(m, "mime", fmt.Sprintf("guest response outputs[%d].mime", i))
				if oerr != nil {
					return nil, oerr
				}
				if !validMIME(mime) {
					return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", fmt.Sprintf("guest response outputs[%d].mime %q is not a valid MIME type", i, mime), nil)
				}
				encoded, oerr := decodeStringField(m, "data_base64", fmt.Sprintf("guest response outputs[%d].data_base64", i))
				if oerr != nil {
					return nil, oerr
				}
				// Bound the decoded allocation by the message-derived safety
				// bound, never by the public output cap. The encoded frame is
				// already bounded by MaxMessageBytes; this check makes the
				// allocation bound explicit before decoding.
				if maxImageBytes > 0 && base64DecodedLen(encoded) > maxImageBytes {
					return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", fmt.Sprintf("guest response outputs[%d].data_base64 exceeds %d decoded bytes", i, maxImageBytes), nil)
				}
				data, derr := base64.StdEncoding.Strict().DecodeString(encoded)
				if derr != nil {
					return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", fmt.Sprintf("guest response outputs[%d].data_base64 is not valid base64", i), derr)
				}
				resp.Outputs = append(resp.Outputs, Output{Type: "image", MIME: mime, Data: data})
			default:
				return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", fmt.Sprintf("unsupported guest output type %q", outType), nil)
			}
		}
	} else if hasOutput && !isJSONNull(outputRaw) {
		text, oerr := decodeStringField(top, "output", "guest response output")
		if oerr != nil {
			return nil, oerr
		}
		resp.Outputs = append(resp.Outputs, Output{Type: "text", Text: text})
	}

	if raw, ok := top["output_truncated"]; ok && !isJSONNull(raw) {
		if err := json.Unmarshal(raw, &resp.OutputTruncated); err != nil {
			return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "guest response output_truncated is not a boolean", err)
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
		if rawCause, ok := m["tool_cause"]; ok && !isJSONNull(rawCause) {
			var cm map[string]json.RawMessage
			if err := json.Unmarshal(rawCause, &cm); err != nil {
				return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "guest response error.tool_cause is not an object", err)
			}
			if cm == nil {
				return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "guest response error.tool_cause is not an object", nil)
			}
			resp.UnknownFields = append(resp.UnknownFields, unknownKeys(cm, toolCauseKeys, "error.tool_cause.")...)

			tc := &ToolCause{}
			if rawCode, ok := cm["code"]; ok {
				if err := json.Unmarshal(rawCode, &tc.Code); err != nil {
					return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "guest response error.tool_cause.code is not a string", err)
				}
			}
			if tc.Code == "" {
				return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "guest response error.tool_cause is missing code", nil)
			}
			rawMessage, ok := cm["message"]
			if !ok {
				return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "guest response error.tool_cause is missing message", nil)
			}
			if err := json.Unmarshal(rawMessage, &tc.Message); err != nil {
				return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "guest response error.tool_cause.message is not a string", err)
			}
			if rawTool, ok := cm["tool"]; ok && !isJSONNull(rawTool) {
				if err := json.Unmarshal(rawTool, &tc.Tool); err != nil {
					return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "guest response error.tool_cause.tool is not a string", err)
				}
			}
			if rawCallID, ok := cm["call_id"]; ok && !isJSONNull(rawCallID) {
				var id int64
				if err := json.Unmarshal(rawCallID, &id); err != nil {
					return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "guest response error.tool_cause.call_id is not an integer", err)
				}
				if id <= 0 {
					return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "guest response error.tool_cause.call_id must be positive", nil)
				}
				tc.CallID = &id
			}
			ex.ToolCause = tc
		}
		resp.Exception = ex
	}

	// A completed response is a success and must never carry error details.
	// Only a failed response may carry a structured exception; a completed
	// response that also carries an error object is a protocol violation and
	// must never be surfaced as a successful completion.
	if resp.Type == "completed" && resp.Exception != nil {
		return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "completed response must not include error details", nil)
	}

	// State is only committable on a genuinely completed response. Fields on
	// failed/suspended responses are tolerated but ignored by the owner.
	if resp.Type == "completed" {
		if raw, ok := top["state_written"]; ok && !isJSONNull(raw) {
			if err := json.Unmarshal(raw, &resp.StateWritten); err != nil {
				return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "guest response state_written is not a boolean", err)
			}
		}
		if raw, ok := top["state"]; ok {
			resp.StatePresent = true
			resp.StateRaw = append(json.RawMessage(nil), raw...)
		}
		if resp.StateWritten && !resp.StatePresent {
			return nil, newError(KindProtocolError, StatusProtocolError, "decode", "", "completed response with state_written:true is missing state", nil)
		}
	}

	sort.Strings(resp.UnknownFields)
	return resp, nil
}

// decodeStringField decodes a required, non-null string field. Empty strings
// are allowed; the caller decides whether a particular field must be non-empty.
func decodeStringField(m map[string]json.RawMessage, key, path string) (string, *Error) {
	raw, ok := m[key]
	if !ok || isJSONNull(raw) {
		return "", newError(KindProtocolError, StatusProtocolError, "decode", "", path+" is missing", nil)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", newError(KindProtocolError, StatusProtocolError, "decode", "", path+" is not a string", err)
	}
	if !utf8.ValidString(s) {
		return "", newError(KindProtocolError, StatusProtocolError, "decode", "", path+" is not valid UTF-8", nil)
	}
	return s, nil
}

// base64DecodedLen returns the exact decoded length for a standard padded
// base64 string without allocating the decoded bytes. Invalid input may
// produce an approximate value; the subsequent strict decode reports the
// protocol error.
func base64DecodedLen(encoded string) int {
	n := base64.StdEncoding.DecodedLen(len(encoded))
	if strings.HasSuffix(encoded, "==") {
		n -= 2
	} else if strings.HasSuffix(encoded, "=") {
		n--
	}
	if n < 0 {
		return 0
	}
	return n
}

func validConsoleLevel(level string) bool {
	switch level {
	case "debug", "info", "warn", "error":
		return true
	default:
		return false
	}
}

// validMIME accepts a bare type/subtype token pair. Parameters, whitespace,
// control characters, and non-ASCII bytes are rejected so a malformed MIME
// value cannot be surfaced as a real ImageEvent.
func validMIME(mime string) bool {
	if mime == "" || len(mime) > 255 {
		return false
	}
	slash := strings.IndexByte(mime, '/')
	if slash <= 0 || slash == len(mime)-1 {
		return false
	}
	if strings.IndexByte(mime[slash+1:], '/') >= 0 {
		return false
	}
	for i := 0; i < len(mime); i++ {
		c := mime[i]
		if c <= 0x20 || c >= 0x7f {
			return false
		}
		switch c {
		case '(', ')', '<', '>', '@', ',', ';', ':', '\\', '"', '[', ']', '?', '=':
			return false
		}
	}
	return true
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
