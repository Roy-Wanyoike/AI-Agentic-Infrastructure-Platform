package sandbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// protocol.go defines the wire format between the parent Runner and the
// cmd/sandbox-exec helper child: line-delimited JSON. The parent writes one
// Request line to the child's stdin; the child writes one Response line
// ("result|error") to stdout and exits. Both sides import this file so the
// types cannot drift.

// MaxRequestBytes caps how much of a request line the child is willing to
// read. Requests carry tool inputs only, which are small in practice; the cap
// bounds child memory against a misbehaving parent (defense in depth behind
// the child's own rlimits).
const MaxRequestBytes = 4 << 20 // 4 MiB

// Request is one tool job sent parent -> child.
type Request struct {
	// Tool is the registry name of the tool to execute inside the child.
	Tool string `json:"tool"`
	// Input is the tool argument map exactly as the runtime would pass it to
	// the tool in-process.
	Input map[string]any `json:"input,omitempty"`
}

// Response is one outcome sent child -> parent: either a result (OK=true) or
// a tool/protocol error message (OK=false, Error set).
type Response struct {
	OK     bool           `json:"ok"`
	Result map[string]any `json:"result,omitempty"`
	Error  string         `json:"error,omitempty"`
}

// EncodeRequest renders req as a single newline-terminated JSON line.
func EncodeRequest(req Request) ([]byte, error) {
	line, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("sandbox: cannot encode request: %w", err)
	}
	return append(line, '\n'), nil
}

// WriteResponse writes resp as a single newline-terminated JSON line. The
// whole line is written with one Write call so a partial write can never
// interleave with anything else (the child is a single-request process).
func WriteResponse(w io.Writer, resp Response) error {
	line, err := json.Marshal(resp)
	if err != nil {
		// Fall back to a minimal error line: a response that cannot be
		// marshaled (e.g. a result with an unencodable value) is still an
		// answer the parent can parse and surface.
		line, _ = json.Marshal(Response{OK: false, Error: "sandbox: cannot encode response: " + err.Error()})
	}
	buf := &bytes.Buffer{}
	buf.Write(line)
	buf.WriteByte('\n')
	_, err = w.Write(buf.Bytes())
	return err
}

// DecodeRequest parses one request line (newline already stripped by the
// caller, or trailing whitespace tolerated).
func DecodeRequest(line []byte) (Request, error) {
	var req Request
	if err := json.Unmarshal(bytes.TrimSpace(line), &req); err != nil {
		return Request{}, fmt.Errorf("sandbox: cannot decode request: %w", err)
	}
	if req.Tool == "" {
		return Request{}, fmt.Errorf("sandbox: request is missing the tool name")
	}
	return req, nil
}

// DecodeResponse parses one response line (newline already stripped by the
// caller, or trailing whitespace tolerated).
func DecodeResponse(line []byte) (Response, error) {
	var resp Response
	if err := json.Unmarshal(bytes.TrimSpace(line), &resp); err != nil {
		return Response{}, fmt.Errorf("sandbox: cannot decode response: %w", err)
	}
	return resp, nil
}
