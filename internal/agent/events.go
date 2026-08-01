package agent

// EventKind classifies one Progress notification emitted during a turn.
type EventKind int

const (
	// EventIteration fires once per Think/Act cycle, right before the
	// provider call, so a channel can show "thinking..." feedback on slow
	// turns.
	EventIteration EventKind = iota
	// EventToolStarted fires immediately before ToolRunner.Run is called
	// for one tool call.
	EventToolStarted
	// EventToolFinished fires after ToolRunner.Run returns for one tool
	// call, whether it produced a result string or a Go error. Err is set
	// only in the latter case: a tool-level failure is a result string,
	// not an error, and is not reported through Err here.
	EventToolFinished
)

// String renders the kind for logs.
func (k EventKind) String() string {
	switch k {
	case EventIteration:
		return "iteration"
	case EventToolStarted:
		return "tool_started"
	case EventToolFinished:
		return "tool_finished"
	default:
		return "unknown"
	}
}

// Event is one progress notification a channel can use to show "running
// exec..." style feedback without blocking the loop.
type Event struct {
	Kind EventKind
	// Iteration is set on EventIteration; 1-based.
	Iteration int
	// ToolName and ToolCallID are set on EventToolStarted and
	// EventToolFinished.
	ToolName   string
	ToolCallID string
	// Err is set on EventToolFinished only when ToolRunner.Run returned a
	// Go error (an infrastructure fault that is about to abort the turn).
	Err error
}

// Progress receives Event notifications during Loop.Run. It may be nil; the
// loop checks before calling it.
type Progress func(Event)
