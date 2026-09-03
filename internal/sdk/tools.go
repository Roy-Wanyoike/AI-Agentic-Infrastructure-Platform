package sdk

import (
	"context"
)

// Tool is one entry of the public tool registry catalog
// (GET /v1/tools, {"tools":[{"name","description","input_schema"}]}).
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
}

// ToolList is the wrapped shape of GET /v1/tools.
type ToolList struct {
	Tools []Tool `json:"tools"`
}

// ListTools returns the read-only registry catalog (GET /v1/tools).
func (c *Client) ListTools(ctx context.Context) (*ToolList, error) {
	var out ToolList
	if err := c.do(ctx, httpMethodGet, "/tools", nil, nil, &out); err != nil {
		return nil, err
	}
	if out.Tools == nil {
		out.Tools = []Tool{}
	}
	return &out, nil
}
