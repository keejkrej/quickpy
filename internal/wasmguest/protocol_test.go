package wasmguest

import (
	"strings"
	"testing"
)

func TestDecodeCompleted(t *testing.T) {
	data := []byte(`{"v":0,"type":"completed","outputs":[{"type":"text","text":"hello"},{"type":"console","level":"warn","text":"careful"}]}`)
	resp, err := decodeResponse(data)
	if err != nil {
		t.Fatalf("decodeResponse: %v", err)
	}
	if resp.Type != "completed" {
		t.Fatalf("type = %q", resp.Type)
	}
	if len(resp.Outputs) != 2 {
		t.Fatalf("outputs = %d", len(resp.Outputs))
	}
	if resp.Outputs[0].Text != "hello" || resp.Outputs[1].Level != "warn" {
		t.Fatalf("unexpected outputs: %+v", resp.Outputs)
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

func TestDecodeRejectsBothOutputForms(t *testing.T) {
	_, err := decodeResponse([]byte(`{"v":0,"type":"completed","output":"a","outputs":[{"type":"text","text":"b"}]}`))
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
	_, err := decodeResponse([]byte(`{"v":0,"type":"completed","outputs":[{"type":"image","text":"x"}]}`))
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
