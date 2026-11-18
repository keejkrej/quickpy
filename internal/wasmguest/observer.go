package wasmguest

// ObservationKind identifies one ordered observation emitted by the execution
// owner.
type ObservationKind string

const (
	ObservationText    ObservationKind = "text"
	ObservationConsole ObservationKind = "console"
	ObservationImage   ObservationKind = "image"
	ObservationTool    ObservationKind = "tool"
)

// Observation is one ordered execution observation. Text, console, and image
// observations carry guest output. Tool observations carry the call name, the
// internal call ID, the phase ("started" or "finished"), and an error code
// when the call failed. Image observations carry a validated MIME type and raw
// decoded bytes.
type Observation struct {
	Kind     ObservationKind
	Text     string
	Level    string
	MIME     string
	Data     []byte
	ToolName string
	CallID   int64
	Phase    string
	Error    string
}

// Observer receives observations serially from the execution owner. It must
// not call back into the guest. A panic is recovered and classified as
// HostFault.
type Observer func(Observation)
