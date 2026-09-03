package sdk

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// RunEvent mirrors one SSE payload of GET /v1/runs/{id}/events. The handler
// writes each event as `data: {json}\n\n` with the keys below
// (cmd/api/handlers.go runEventsHandler).
type RunEvent struct {
	RunID     string         `json:"run_id"`
	Type      string         `json:"type"`
	Name      string         `json:"name"`
	Payload   map[string]any `json:"payload"`
	CreatedAt string         `json:"created_at"`
}

// EventStream is a decoder over a text/event-stream response. It follows the
// SSE framing rules relevant here: `data:` payload lines accumulated until a
// blank line, with `:` comment lines and `event:`/`id:` fields ignored.
type EventStream struct {
	resp    *http.Response
	scanner *bufio.Scanner
	carried []string // data lines seen since the last blank line
}

// newEventStream wraps a 2xx event-stream response.
func newEventStream(resp *http.Response) *EventStream {
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // events stay well under 1MB
	return &EventStream{resp: resp, scanner: sc}
}

// Next returns the next event. It returns io.EOF when the stream ends.
func (es *EventStream) Next() (*RunEvent, error) {
	for es.scanner.Scan() {
		line := es.scanner.Text()
		switch {
		case line == "": // blank line terminates one SSE event
			if len(es.carried) == 0 {
				continue
			}
			payload := strings.Join(es.carried, "\n")
			es.carried = nil
			var ev RunEvent
			if err := json.Unmarshal([]byte(payload), &ev); err != nil {
				return nil, fmt.Errorf("decode sse event: %w", err)
			}
			return &ev, nil
		case strings.HasPrefix(line, ":"): // SSE comment / keepalive
			continue
		case strings.HasPrefix(line, "data:"):
			es.carried = append(es.carried, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		default: // event:/id:/retry: fields carry nothing this API needs
			continue
		}
	}
	if err := es.scanner.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

// Close releases the underlying response body.
func (es *EventStream) Close() error {
	if es.resp == nil || es.resp.Body == nil {
		return nil
	}
	return es.resp.Body.Close()
}

// readAllLimited reads a body for error reporting (bounded to keep hostile
// responses from ballooning memory).
func readAllLimited(r io.Reader) []byte {
	raw, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return nil
	}
	return raw
}
