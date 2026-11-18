package wasmguest

import (
	"bytes"
	"strings"
	"testing"
)

func TestDecodeCompleted(t *testing.T) {
	data := []byte(`{"v":0,"type":"completed","outputs":[{"type":"text","text":"hello"},{"type":"console","level":"warn","text":"careful"},{"type":"image","mime":"image/png","data_base64":"AAECAwT/"}]}`)
	resp, err := decodeResponse(data)
	if err != nil {
		t.Fatalf("decodeResponse: %v", err)
	}
	if resp.Type != "completed" {
		t.Fatalf("type = %q", resp.Type)
	}
	if len(resp.Outputs) != 3 {
		t.Fatalf("outputs = %d", len(resp.Outputs))
	}
	if resp.Outputs[0].Text != "hello" || resp.Outputs[1].Level != "warn" {
		t.Fatalf("unexpected outputs: %+v", resp.Outputs)
	}
	if resp.Outputs[2].MIME != "image/png" || !bytes.Equal(resp.Outputs[2].Data, []byte{0, 1, 2, 3, 4, 0xff}) {
		t.Fatalf("image output = %+v", resp.Outputs[2])
	}
	if len(resp.UnknownFields) != 0 {
		t.Fatalf("unknown fields = %v", resp.UnknownFields)
	}
}

func TestDecodeFailed(t *testing.T) {
	data := []byte(`{"v":0,"type":"failed","error":{"type":"ValueError","message":"boom","traceback":"tb","filename":"main.py","line":3}}`)
	resp, err := decodeResponse(data)
	if err != nil {
		t.Fatalf("decodeResponse: %v", err)
	}
	if resp.Type != "failed" {
		t.Fatalf("type = %q", resp.Type)
	}
	if resp.Exception == nil || resp.Exception.Type != "ValueError" || resp.Exception.Line != 3 {
		t.Fatalf("exception = %+v", resp.Exception)
	}
	if resp.Exception.ToolCause != nil {
		t.Fatalf("tool cause = %+v, want nil", resp.Exception.ToolCause)
	}
}

func TestDecodeEngineMetadata(t *testing.T) {
	data := []byte(`{"v":0,"type":"completed","engine_name":"micropython","engine_version":"1.29.0"}`)
	resp, err := decodeResponse(data)
	if err != nil {
		t.Fatalf("decodeResponse: %v", err)
	}
	if resp.EngineName != "micropython" || resp.EngineVersion != "1.29.0" {
		t.Fatalf("engine metadata = %q %q", resp.EngineName, resp.EngineVersion)
	}
	if len(resp.UnknownFields) != 0 {
		t.Fatalf("unknown fields = %v", resp.UnknownFields)
	}
}

func TestDecodeToolCause(t *testing.T) {
	data := []byte(`{"v":0,"type":"failed","error":{"type":"ToolError","message":"tool failed","tool_cause":{"code":"permission_denied","message":"no","tool":"search","call_id":7}}}`)
	resp, err := decodeResponse(data)
	if err != nil {
		t.Fatalf("decodeResponse: %v", err)
	}
	tc := resp.Exception.ToolCause
	if tc == nil {
		t.Fatal("ToolCause = nil")
	}
	if tc.Code != "permission_denied" || tc.Message != "no" || tc.Tool != "search" {
		t.Fatalf("ToolCause = %+v", tc)
	}
	if tc.CallID == nil || *tc.CallID != 7 {
		t.Fatalf("CallID = %v", tc.CallID)
	}
}

func TestDecodeToolCauseOptionalFields(t *testing.T) {
	data := []byte(`{"v":0,"type":"failed","error":{"type":"ToolError","tool_cause":{"code":"tool_error","message":"boom"}}}`)
	resp, err := decodeResponse(data)
	if err != nil {
		t.Fatalf("decodeResponse: %v", err)
	}
	tc := resp.Exception.ToolCause
	if tc == nil || tc.Code != "tool_error" || tc.Message != "boom" {
		t.Fatalf("ToolCause = %+v", tc)
	}
	if tc.Tool != "" || tc.CallID != nil {
		t.Fatalf("optional fields = %+v", tc)
	}
}

func TestDecodeToolCauseRejectsMissingCode(t *testing.T) {
	_, err := decodeResponse([]byte(`{"v":0,"type":"failed","error":{"type":"ToolError","tool_cause":{"message":"boom"}}}`))
	if err == nil || err.Kind != KindProtocolError {
		t.Fatalf("err = %v", err)
	}
}

func TestDecodeToolCauseRejectsMissingMessage(t *testing.T) {
	_, err := decodeResponse([]byte(`{"v":0,"type":"failed","error":{"type":"ToolError","tool_cause":{"code":"x"}}}`))
	if err == nil || err.Kind != KindProtocolError {
		t.Fatalf("err = %v", err)
	}
}

func TestDecodeToolCauseRejectsBadCallID(t *testing.T) {
	_, err := decodeResponse([]byte(`{"v":0,"type":"failed","error":{"type":"ToolError","tool_cause":{"code":"x","call_id":0}}}`))
	if err == nil || err.Kind != KindProtocolError {
		t.Fatalf("err = %v", err)
	}
}

func TestDecodeUnknownFields(t *testing.T) {
	data := []byte(`{"v":0,"type":"completed","future":true,"outputs":[{"type":"text","text":"x","extra":1}],"error":null}`)
	resp, err := decodeResponse(data)
	if err != nil {
		t.Fatalf("decodeResponse: %v", err)
	}
	got := strings.Join(resp.UnknownFields, ",")
	if got != "future,outputs[0].extra" {
		t.Fatalf("unknown fields = %q", got)
	}
}

func TestDecodeOutputString(t *testing.T) {
	data := []byte(`{"v":0,"type":"completed","output":"hello"}`)
	resp, err := decodeResponse(data)
	if err != nil {
		t.Fatalf("decodeResponse: %v", err)
	}
	if len(resp.Outputs) != 1 || resp.Outputs[0].Type != "text" || resp.Outputs[0].Text != "hello" {
		t.Fatalf("outputs = %+v", resp.Outputs)
	}
}

func TestDecodeOutputTruncated(t *testing.T) {
	data := []byte(`{"v":0,"type":"completed","output":"hello","output_truncated":true}`)
	resp, err := decodeResponse(data)
	if err != nil {
		t.Fatalf("decodeResponse: %v", err)
	}
	if !resp.OutputTruncated {
		t.Fatal("expected output_truncated")
	}
}

func TestDecodePrefersOutputsOverLegacy(t *testing.T) {
	resp, err := decodeResponse([]byte(`{"v":0,"type":"completed","output":"legacy","outputs":[{"type":"text","text":"structured"}]}`))
	if err != nil {
		t.Fatalf("decodeResponse: %v", err)
	}
	if len(resp.Outputs) != 1 || resp.Outputs[0].Text != "structured" {
		t.Fatalf("outputs = %+v", resp.Outputs)
	}
}

func TestDecodeEmptyOutputsArrayIgnoresLegacy(t *testing.T) {
	resp, err := decodeResponse([]byte(`{"v":0,"type":"completed","output":"legacy","outputs":[]}`))
	if err != nil {
		t.Fatalf("decodeResponse: %v", err)
	}
	if len(resp.Outputs) != 0 {
		t.Fatalf("outputs = %+v", resp.Outputs)
	}
}

func TestDecodeNullOutputsFallsBackToLegacy(t *testing.T) {
	resp, err := decodeResponse([]byte(`{"v":0,"type":"completed","output":"legacy","outputs":null}`))
	if err != nil {
		t.Fatalf("decodeResponse: %v", err)
	}
	if len(resp.Outputs) != 1 || resp.Outputs[0].Text != "legacy" {
		t.Fatalf("outputs = %+v", resp.Outputs)
	}
}

func TestDecodeMissingOutputsFallsBackToLegacy(t *testing.T) {
	resp, err := decodeResponse([]byte(`{"v":0,"type":"completed","output":"legacy"}`))
	if err != nil {
		t.Fatalf("decodeResponse: %v", err)
	}
	if len(resp.Outputs) != 1 || resp.Outputs[0].Text != "legacy" {
		t.Fatalf("outputs = %+v", resp.Outputs)
	}
}

func TestDecodeMissingOutputsAndLegacyIsEmpty(t *testing.T) {
	resp, err := decodeResponse([]byte(`{"v":0,"type":"completed"}`))
	if err != nil {
		t.Fatalf("decodeResponse: %v", err)
	}
	if len(resp.Outputs) != 0 {
		t.Fatalf("outputs = %+v", resp.Outputs)
	}
}

func TestDecodeConsoleLevels(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "error"} {
		data := []byte(`{"v":0,"type":"completed","outputs":[{"type":"console","level":"` + level + `","text":"x"}]}`)
		resp, err := decodeResponse(data)
		if err != nil {
			t.Fatalf("level %q: %v", level, err)
		}
		if len(resp.Outputs) != 1 || resp.Outputs[0].Level != level {
			t.Fatalf("level %q: outputs = %+v", level, resp.Outputs)
		}
	}
}

func TestDecodeRejectsInvalidConsoleLevel(t *testing.T) {
	cases := []string{
		`{"v":0,"type":"completed","outputs":[{"type":"console","text":"x"}]}`,
		`{"v":0,"type":"completed","outputs":[{"type":"console","level":"trace","text":"x"}]}`,
		`{"v":0,"type":"completed","outputs":[{"type":"console","level":null,"text":"x"}]}`,
		`{"v":0,"type":"completed","outputs":[{"type":"console","level":1,"text":"x"}]}`,
	}
	for _, data := range cases {
		if _, err := decodeResponse([]byte(data)); err == nil || err.Kind != KindProtocolError {
			t.Fatalf("expected protocol error for %s, got %v", data, err)
		}
	}
}

func TestDecodeImageBinaryFidelity(t *testing.T) {
	resp, err := decodeResponse([]byte(`{"v":0,"type":"completed","outputs":[{"type":"image","mime":"application/octet-stream","data_base64":"AAECf4D/"}]}`))
	if err != nil {
		t.Fatalf("decodeResponse: %v", err)
	}
	want := []byte{0x00, 0x01, 0x02, 0x7f, 0x80, 0xff}
	if len(resp.Outputs) != 1 || !bytes.Equal(resp.Outputs[0].Data, want) {
		t.Fatalf("image data = %v, want %v", resp.Outputs[0].Data, want)
	}
}

func TestDecodeRejectsInvalidBase64(t *testing.T) {
	cases := []string{
		`{"v":0,"type":"completed","outputs":[{"type":"image","mime":"image/png","data_base64":"!!!!"}]}`,
		`{"v":0,"type":"completed","outputs":[{"type":"image","mime":"image/png","data_base64":"A"}]}`,
		`{"v":0,"type":"completed","outputs":[{"type":"image","mime":"image/png","data_base64":null}]}`,
		`{"v":0,"type":"completed","outputs":[{"type":"image","mime":"image/png"}]}`,
	}
	for _, data := range cases {
		if _, err := decodeResponse([]byte(data)); err == nil || err.Kind != KindProtocolError {
			t.Fatalf("expected protocol error for %s, got %v", data, err)
		}
	}
}

func TestDecodeRejectsInvalidMIME(t *testing.T) {
	cases := []string{
		`{"v":0,"type":"completed","outputs":[{"type":"image","mime":"","data_base64":"AQID"}]}`,
		`{"v":0,"type":"completed","outputs":[{"type":"image","mime":"png","data_base64":"AQID"}]}`,
		`{"v":0,"type":"completed","outputs":[{"type":"image","mime":"image/","data_base64":"AQID"}]}`,
		`{"v":0,"type":"completed","outputs":[{"type":"image","mime":"/png","data_base64":"AQID"}]}`,
		`{"v":0,"type":"completed","outputs":[{"type":"image","mime":"image/png; charset=x","data_base64":"AQID"}]}`,
		`{"v":0,"type":"completed","outputs":[{"type":"image","mime":"image/p ng","data_base64":"AQID"}]}`,
		`{"v":0,"type":"completed","outputs":[{"type":"image","mime":null,"data_base64":"AQID"}]}`,
	}
	for _, data := range cases {
		if _, err := decodeResponse([]byte(data)); err == nil || err.Kind != KindProtocolError {
			t.Fatalf("expected protocol error for %s, got %v", data, err)
		}
	}
}

func TestDecodeRejectsInvalidUTF8(t *testing.T) {
	data := []byte("{\"v\":0,\"type\":\"completed\",\"outputs\":[{\"type\":\"text\",\"text\":\"bad\xff\"}]}")
	if _, err := decodeResponse(data); err == nil || err.Kind != KindProtocolError {
		t.Fatalf("err = %v", err)
	}
}

func TestDecodeRejectsUnpairedSurrogate(t *testing.T) {
	_, err := decodeResponse([]byte(`{"v":0,"type":"completed","outputs":[{"type":"text","text":"\ud800"}]}`))
	if err == nil || err.Kind != KindProtocolError {
		t.Fatalf("err = %v", err)
	}
}

func TestDecodeRejectsImageOverMessageBound(t *testing.T) {
	// 6 decoded bytes, message-derived allocation bound 5.
	_, err := decodeResponseWithMessageLimit([]byte(`{"v":0,"type":"completed","outputs":[{"type":"image","mime":"image/png","data_base64":"AAECAwT/"}]}`), 5)
	if err == nil || err.Kind != KindProtocolError {
		t.Fatalf("err = %v", err)
	}
}

func TestDecodeAllowsImageAtMessageBound(t *testing.T) {
	resp, err := decodeResponseWithMessageLimit([]byte(`{"v":0,"type":"completed","outputs":[{"type":"image","mime":"image/png","data_base64":"AAECAwT/"}]}`), 6)
	if err != nil {
		t.Fatalf("decodeResponseWithMessageLimit: %v", err)
	}
	if len(resp.Outputs) != 1 || len(resp.Outputs[0].Data) != 6 {
		t.Fatalf("outputs = %+v", resp.Outputs)
	}
}

func TestDecodeAcceptsImageLargerThanOutputCap(t *testing.T) {
	// The decoder has no public output cap. A valid image is decoded here and
	// clipped later by the execution owner's cumulative accounting.
	resp, err := decodeResponse([]byte(`{"v":0,"type":"completed","outputs":[{"type":"image","mime":"image/png","data_base64":"AAECAwQFBgcICQoLDA0ODxAREhM="}]}`))
	if err != nil {
		t.Fatalf("decodeResponse: %v", err)
	}
	if len(resp.Outputs) != 1 || len(resp.Outputs[0].Data) != 20 {
		t.Fatalf("outputs = %+v", resp.Outputs)
	}
}

func TestDecodeRejectsNonStringOutputFields(t *testing.T) {
	cases := []string{
		`{"v":0,"type":"completed","outputs":[{"type":"text","text":1}]}`,
		`{"v":0,"type":"completed","outputs":[{"type":"console","level":"info","text":1}]}`,
		`{"v":0,"type":"completed","outputs":[{"type":"image","mime":1,"data_base64":"AQID"}]}`,
		`{"v":0,"type":"completed","outputs":[{"type":"image","mime":"image/png","data_base64":1}]}`,
		`{"v":0,"type":"completed","outputs":[{"type":1}]}`,
	}
	for _, data := range cases {
		if _, err := decodeResponse([]byte(data)); err == nil || err.Kind != KindProtocolError {
			t.Fatalf("expected protocol error for %s, got %v", data, err)
		}
	}
}

func TestDecodeRejectsOutputsNotArray(t *testing.T) {
	_, err := decodeResponse([]byte(`{"v":0,"type":"completed","outputs":"nope"}`))
	if err == nil || err.Kind != KindProtocolError {
		t.Fatalf("err = %v", err)
	}
}

func TestDecodeRejectsLegacyOutputNotString(t *testing.T) {
	_, err := decodeResponse([]byte(`{"v":0,"type":"completed","output":1}`))
	if err == nil || err.Kind != KindProtocolError {
		t.Fatalf("err = %v", err)
	}
}

func TestDecodeRejectsDeepNesting(t *testing.T) {
	data := []byte(`{"v":0,"type":"completed","outputs":` + strings.Repeat("[", 300) + strings.Repeat("]", 300) + `}`)
	_, err := decodeResponse(data)
	if err == nil || err.Kind != KindProtocolError {
		t.Fatalf("err = %v", err)
	}
}

func TestDecodeRejectsStalledOnCompleted(t *testing.T) {
	_, err := decodeResponse([]byte(`{"v":0,"type":"completed","stalled":true}`))
	if err == nil || err.Kind != KindProtocolError {
		t.Fatalf("err = %v", err)
	}
}

func TestDecodeRejectsMalformed(t *testing.T) {
	cases := [][]byte{
		nil,
		[]byte(`{`),
		[]byte(`[]`),
		[]byte(`null`),
		[]byte(`{"v":0,"type":"completed"} trailing`),
	}
	for _, data := range cases {
		if _, err := decodeResponse(data); err == nil {
			t.Fatalf("expected error for %q", data)
		}
	}
}

func TestDecodeRejectsWrongVersion(t *testing.T) {
	_, err := decodeResponse([]byte(`{"v":1,"type":"completed"}`))
	if err == nil || err.Kind != KindProtocolError {
		t.Fatalf("err = %v", err)
	}
}

func TestDecodeRejectsWrongType(t *testing.T) {
	_, err := decodeResponse([]byte(`{"v":0,"type":"started"}`))
	if err == nil || err.Kind != KindProtocolError {
		t.Fatalf("err = %v", err)
	}
}

func TestDecodeRejectsUnknownOutputType(t *testing.T) {
	_, err := decodeResponse([]byte(`{"v":0,"type":"completed","outputs":[{"type":"video","text":"x"}]}`))
	if err == nil || err.Kind != KindProtocolError {
		t.Fatalf("err = %v", err)
	}
}

func TestDecodeRejectsDuplicateKeys(t *testing.T) {
	_, err := decodeResponse([]byte(`{"v":0,"v":0,"type":"completed"}`))
	if err == nil || err.Kind != KindProtocolError {
		t.Fatalf("err = %v", err)
	}
}

func TestDecodeAllowsOversizedOutputForOwnerClipping(t *testing.T) {
	// Output size is enforced cumulatively by the execution owner so the
	// retained prefix and observer stream are clipped consistently. The
	// decoder only validates the message shape.
	resp, err := decodeResponse([]byte(`{"v":0,"type":"completed","outputs":[{"type":"text","text":"0123456789"}]}`))
	if err != nil {
		t.Fatalf("decodeResponse: %v", err)
	}
	if len(resp.Outputs) != 1 || resp.Outputs[0].Text != "0123456789" {
		t.Fatalf("outputs = %+v", resp.Outputs)
	}
}

func TestDecodeStateFields(t *testing.T) {
	resp, err := decodeResponse([]byte(`{"v":0,"type":"completed","state_written":true,"state":{"a":1}}`))
	if err != nil {
		t.Fatalf("decodeResponse: %v", err)
	}
	if !resp.StateWritten || !resp.StatePresent {
		t.Fatalf("state flags = written:%v present:%v", resp.StateWritten, resp.StatePresent)
	}
	if string(resp.StateRaw) != `{"a":1}` {
		t.Fatalf("state raw = %s", resp.StateRaw)
	}
}

func TestDecodeStateNull(t *testing.T) {
	resp, err := decodeResponse([]byte(`{"v":0,"type":"completed","state_written":true,"state":null}`))
	if err != nil {
		t.Fatalf("decodeResponse: %v", err)
	}
	if !resp.StateWritten || !resp.StatePresent {
		t.Fatalf("state flags = written:%v present:%v", resp.StateWritten, resp.StatePresent)
	}
	if string(resp.StateRaw) != "null" {
		t.Fatalf("state raw = %s", resp.StateRaw)
	}
}

func TestDecodeStateOmittedNoWrite(t *testing.T) {
	resp, err := decodeResponse([]byte(`{"v":0,"type":"completed"}`))
	if err != nil {
		t.Fatalf("decodeResponse: %v", err)
	}
	if resp.StateWritten || resp.StatePresent {
		t.Fatalf("state flags = written:%v present:%v", resp.StateWritten, resp.StatePresent)
	}
}

func TestDecodeStateWrittenMissingStateRejected(t *testing.T) {
	_, err := decodeResponse([]byte(`{"v":0,"type":"completed","state_written":true}`))
	if err == nil || err.Kind != KindProtocolError {
		t.Fatalf("err = %v", err)
	}
}

func TestDecodeStateOnNonCompletedIgnored(t *testing.T) {
	resp, err := decodeResponse([]byte(`{"v":0,"type":"failed","state_written":true,"state":{"a":1},"error":{"type":"ValueError","message":"x"}}`))
	if err != nil {
		t.Fatalf("decodeResponse: %v", err)
	}
	if resp.StateWritten || resp.StatePresent {
		t.Fatalf("state flags = written:%v present:%v", resp.StateWritten, resp.StatePresent)
	}
}

func TestDecodeRejectsCompletedWithError(t *testing.T) {
	// A completed response is a success and must never carry error details.
	_, err := decodeResponse([]byte(`{"v":0,"type":"completed","error":{"type":"Error","message":"x"}}`))
	if err == nil || err.Kind != KindProtocolError {
		t.Fatalf("err = %v", err)
	}
}

func TestDecodeRejectsRealShapedCompletedWithError(t *testing.T) {
	// A real-shaped completed response that also carries output, state, and an
	// error object must be rejected rather than surfaced as a successful
	// completion with committed state.
	data := []byte(`{"v":0,"type":"completed","output":"done","state_written":true,"state":null,"error":{"type":"ValueError","message":"boom","traceback":"tb","filename":"main.py","line":3}}`)
	_, err := decodeResponse(data)
	if err == nil || err.Kind != KindProtocolError {
		t.Fatalf("err = %v", err)
	}
}

func TestDecodeAcceptsCompletedStateNullNoError(t *testing.T) {
	// The legitimate success shape: output plus an explicit null state and no
	// error object is accepted and remains committable.
	resp, err := decodeResponse([]byte(`{"v":0,"type":"completed","output":"done","state_written":true,"state":null}`))
	if err != nil {
		t.Fatalf("decodeResponse: %v", err)
	}
	if resp.Type != "completed" || !resp.StateWritten || !resp.StatePresent {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.Exception != nil {
		t.Fatalf("exception = %+v", resp.Exception)
	}
	if string(resp.StateRaw) != "null" {
		t.Fatalf("state raw = %s", resp.StateRaw)
	}
}

func TestBoundedBuffer(t *testing.T) {
	b := newBoundedBuffer(5)
	if n, err := b.Write([]byte("hello world")); err != nil || n != 11 {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if b.String() != "hello" {
		t.Fatalf("String = %q", b.String())
	}
	if !b.Truncated() {
		t.Fatal("expected truncated")
	}
}
