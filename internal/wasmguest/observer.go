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

// Observation is one ordered execution observation. Text and console
// observations carry guest output. Tool observations carry the call name, the
// internal call ID, the phase ("started" or "finished"), and an error code
// when the call failed. Image observations are reserved for a future guest
// output channel; the current guest does not emit them.
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
