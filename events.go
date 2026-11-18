package quickpy

// Event is one ordered execution event.
type Event interface{ isEvent() }

// TextEvent is guest stdout.
type TextEvent struct{ Text string }

func (TextEvent) isEvent() {}

// ConsoleEvent is guest diagnostics/log output.
type ConsoleEvent struct {
	Level string
	Text  string
}

func (ConsoleEvent) isEvent() {}

// ImageEvent is guest image output.
type ImageEvent struct {
	MIME string
	Data []byte
}

func (ImageEvent) isEvent() {}

// ToolEvent is a host-observed tool call start or finish.
type ToolEvent struct {
	Phase  string
	Name   string
	CallID string
	Error  string
}

func (ToolEvent) isEvent() {}

type observationKind int

const (
	observationText observationKind = iota
	observationConsole
	observationImage
	observationTool
)

// observation is the SDK-internal form of one guest observation. It mirrors
// the low-level wasmguest.Observation without exposing that package.
type observation struct {
	Kind     observationKind
	Text     string
	Level    string
	MIME     string
	Data     []byte
	ToolName string
	CallID   int64
	Phase    string
	Error    string
}
