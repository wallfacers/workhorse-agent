package provider

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// SSEEvent is one decoded event as it arrives off the wire. Type is empty for
// "default" events that carry no `event:` line. Data is the joined `data:`
// payload with intervening newlines preserved.
type SSEEvent struct {
	Type string
	Data []byte
}

// ParseSSE walks r and invokes fn for each complete event (terminated by a
// blank line). Lines starting with ":" are comments and ignored. `id:` and
// `retry:` lines are ignored too — the agent layer cares about provider events
// only, not transport-level metadata.
//
// fn may return io.EOF to stop processing early without surfacing an error.
// Any other non-nil error is returned to the caller.
//
// The scanner buffer is sized for events up to MaxLineBytes; long
// `data:` lines (e.g. embedded JSON over 1 MiB) cause ErrLineTooLong.
func ParseSSE(r io.Reader, fn func(SSEEvent) error) error {
	sc := bufio.NewScanner(r)
	// SSE lines are usually short, but Anthropic occasionally ships large
	// tool_use JSON in one chunk. Cap at 4 MiB so a single hostile line can't
	// blow up memory but typical payloads always fit.
	sc.Buffer(make([]byte, 0, 8*1024), 4*1024*1024)

	var (
		eventType string
		data      bytes.Buffer
		hasData   bool
	)
	dispatch := func() error {
		if eventType == "" && !hasData {
			return nil
		}
		err := fn(SSEEvent{Type: eventType, Data: data.Bytes()})
		eventType = ""
		data.Reset()
		hasData = false
		return err
	}

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			if err := dispatch(); err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
			continue
		}
		if line[0] == ':' {
			continue // comment / keep-alive
		}
		switch {
		case bytes.HasPrefix(line, []byte("event:")):
			eventType = string(bytes.TrimSpace(line[6:]))
		case bytes.HasPrefix(line, []byte("data:")):
			d := line[5:]
			if len(d) > 0 && d[0] == ' ' {
				d = d[1:]
			}
			if hasData {
				data.WriteByte('\n')
			}
			data.Write(d)
			hasData = true
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	// Flush trailing event with no terminating blank line.
	return dispatch()
}
