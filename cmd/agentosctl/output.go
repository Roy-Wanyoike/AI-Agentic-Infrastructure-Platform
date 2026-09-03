package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"agentos/internal/sdk"
)

// maxCellWidth bounds table columns so wide payloads (agent instructions,
// run outputs) stay readable; longer values are truncated with an ellipsis.
const maxCellWidth = 60

// printTable renders an aligned plain-text table. Empty rows still print the
// header so commands confirm "no data" rather than silence.
func printTable(w io.Writer, headers []string, rows [][]string) {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = utf8.RuneCountInString(h)
	}
	cells := make([][]string, len(rows))
	for r, row := range rows {
		cells[r] = make([]string, len(headers))
		for i := range headers {
			v := ""
			if i < len(row) {
				v = row[i]
			}
			cells[r][i] = truncate(v, maxCellWidth)
			if n := utf8.RuneCountInString(cells[r][i]); n > widths[i] {
				widths[i] = n
			}
		}
	}
	var b strings.Builder
	for i, h := range headers {
		b.WriteString(pad(h, widths[i]))
		if i < len(headers)-1 {
			b.WriteString("  ")
		}
	}
	b.WriteString("\n")
	for i := range widths {
		b.WriteString(strings.Repeat("-", widths[i]))
		if i < len(widths)-1 {
			b.WriteString("  ")
		}
	}
	b.WriteString("\n")
	for _, row := range cells {
		for i := range headers {
			b.WriteString(pad(row[i], widths[i]))
			if i < len(headers)-1 {
				b.WriteString("  ")
			}
		}
		b.WriteString("\n")
	}
	_, _ = io.WriteString(w, b.String())
}

// pad right-pads s to width runes.
func pad(s string, width int) string {
	n := utf8.RuneCountInString(s)
	if n >= width {
		return s
	}
	return s + strings.Repeat(" ", width-n)
}

// truncate cuts s at max runes, appending an ellipsis.
func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max-1]) + "…"
}

// printJSON marshals v as indented JSON (the --json output mode) and returns
// the process exit code.
func printJSON(w io.Writer, v any) int {
	buf, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return exitError
	}
	_, _ = w.Write(append(buf, '\n'))
	return exitOK
}

// printDetail renders a single record as "Field: value" lines in sorted key
// order (deterministic; the human-readable counterpart of --json).
func printDetail(w io.Writer, fields map[string]string) {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%-18s %s\n", k+":", truncate(fields[k], maxCellWidth*2))
	}
	_, _ = io.WriteString(w, b.String())
}

// describeAPIError renders an *sdk.APIError (or any error) into a
// user-facing message with the documented 401/403/404/422 treatments.
func describeAPIError(err error) string {
	apiErr, ok := err.(*sdk.APIError)
	if !ok {
		return "error: " + err.Error()
	}
	switch apiErr.StatusCode {
	case 401:
		return fmt.Sprintf("error: unauthorized (401): %s — run `agentosctl login` or set %s/%s",
			apiErr.Message, EnvToken, EnvAPIKey)
	case 403:
		return fmt.Sprintf("error: forbidden (403): %s — your role lacks the required permission", apiErr.Message)
	case 404:
		return fmt.Sprintf("error: not found (404): %s", apiErr.Message)
	case 422:
		var b strings.Builder
		fmt.Fprintf(&b, "error: validation failed (422)")
		if apiErr.Code != "" {
			fmt.Fprintf(&b, " [%s]", apiErr.Code)
		}
		b.WriteString(":")
		if len(apiErr.ValidationErrors) == 0 {
			fmt.Fprintf(&b, " %s", apiErr.Message)
			return b.String()
		}
		for _, ve := range apiErr.ValidationErrors {
			b.WriteString("\n  - ")
			if ve.Code != "" {
				fmt.Fprintf(&b, "[%s] ", ve.Code)
			}
			b.WriteString(ve.Message)
			if ve.NodeID != "" {
				fmt.Fprintf(&b, " (node: %s)", ve.NodeID)
			}
		}
		return b.String()
	default:
		head := apiErr.Status
		if apiErr.Code != "" {
			head = fmt.Sprintf("%s [%s]", head, apiErr.Code)
		}
		return fmt.Sprintf("error: %s: %s", head, apiErr.Message)
	}
}
