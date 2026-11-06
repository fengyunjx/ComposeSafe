// Package runlog writes the append-only JSONL event log of a run
// (DESIGN 4.2). Recovery commands read only this log and the snapshot
// manifest, never in-memory state.
package runlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Status values of an event.
const (
	Start = "start"
	OK    = "ok"
	Fail  = "fail"
	Skip  = "skip"
)

// Event is one line of run.jsonl.
type Event struct {
	TS     time.Time      `json:"ts"`
	Run    string         `json:"run"`
	Step   string         `json:"step"` // "<phase>.<action>", e.g. "snapshot.dump"
	Status string         `json:"status"`
	Error  string         `json:"error,omitempty"`
	Cmd    []string       `json:"cmd,omitempty"`
	Detail map[string]any `json:"detail,omitempty"`
}

// Logger appends events to a file, redacting and fsyncing each one so the
// log survives a crash of the process.
type Logger struct {
	f   *os.File
	run string
	red *Redactor
	now func() time.Time
}

// Open creates (or appends to) the log at path with mode 0600.
func Open(path, runID string, red *Redactor) (*Logger, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open run log: %w", err)
	}
	return &Logger{f: f, run: runID, red: red, now: time.Now}, nil
}

func (l *Logger) Close() error { return l.f.Close() }

// Log writes one event. Free-form text (error, command, string details) is
// redacted before it reaches the disk.
func (l *Logger) Log(e Event) error {
	e.Run = l.run
	if e.TS.IsZero() {
		e.TS = l.now()
	}
	e.Error = l.red.Redact(e.Error)
	if len(e.Cmd) > 0 {
		cmd := make([]string, len(e.Cmd))
		for i, a := range e.Cmd {
			cmd[i] = l.red.Redact(a)
		}
		e.Cmd = cmd
	}
	if len(e.Detail) > 0 {
		d := make(map[string]any, len(e.Detail))
		for k, v := range e.Detail {
			if s, ok := v.(string); ok {
				v = l.red.Redact(s)
			}
			d[k] = v
		}
		e.Detail = d
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := l.f.Write(append(b, '\n')); err != nil {
		return err
	}
	return l.f.Sync()
}

// Begin logs a start event for step.
func (l *Logger) Begin(step string, cmd ...string) error {
	return l.Log(Event{Step: step, Status: Start, Cmd: cmd})
}

// Done logs the successful end of step.
func (l *Logger) Done(step string, detail map[string]any) error {
	return l.Log(Event{Step: step, Status: OK, Detail: detail})
}

// Failed logs the failure of step.
func (l *Logger) Failed(step string, err error) error {
	return l.Log(Event{Step: step, Status: Fail, Error: err.Error()})
}

// Read parses a run log. A truncated final line (crash mid-write) is ignored.
func Read(path string) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// Interrupted returns the step that was started but never finished, or "".
// A run killed mid-step is recognised by this and rolled back from the
// manifest.
func Interrupted(events []Event) string {
	open := map[string]bool{}
	last := ""
	for _, e := range events {
		switch e.Status {
		case Start:
			open[e.Step] = true
			last = e.Step
		case OK, Fail, Skip:
			delete(open, e.Step)
		}
	}
	if last != "" && open[last] {
		return last
	}
	for _, e := range events { // earlier unfinished step, if any
		if open[e.Step] {
			return e.Step
		}
	}
	return ""
}
