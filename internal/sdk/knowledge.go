package sdk

import (
	"context"
	"net/url"
	"time"
)

// urlPathEscape wraps url.PathEscape for the resource files.
func urlPathEscape(s string) string { return url.PathEscape(s) }

// KnowledgeDocument mirrors the knowledgeDocumentView wire shape of
// GET/POST /v1/knowledge/documents (snake_case).
type KnowledgeDocument struct {
	ID         string         `json:"id"`
	Title      string         `json:"title"`
	Source     string         `json:"source,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	ChunkCount int            `json:"chunk_count"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
}

// AddDocumentRequest is the POST /v1/knowledge/documents body.
type AddDocumentRequest struct {
	Title    string         `json:"title"`
	Content  string         `json:"content"`
	Source   string         `json:"source,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// AddDocumentResponse is the 201 body of POST /v1/knowledge/documents. The
// Warning field is non-empty when the embeddings backend failed but the
// chunks were still stored (lexical fallback remains searchable).
type AddDocumentResponse struct {
	Document KnowledgeDocument `json:"document"`
	Warning  string            `json:"warning,omitempty"`
}

// KnowledgeDocumentList is the wrapped shape of GET /v1/knowledge/documents.
type KnowledgeDocumentList struct {
	Documents []KnowledgeDocument `json:"documents"`
}

// SearchResult is one scored chunk of POST /v1/knowledge/search.
type SearchResult struct {
	DocumentID    string  `json:"document_id"`
	DocumentTitle string  `json:"document_title"`
	ChunkID       string  `json:"chunk_id"`
	ChunkOrdinal  int     `json:"chunk_ordinal"`
	Content       string  `json:"content"`
	Score         float64 `json:"score"`
	Citation      string  `json:"citation"`
}

// SearchResults is the wrapped shape of POST /v1/knowledge/search.
type SearchResults struct {
	Results []SearchResult `json:"results"`
}

// ListDocuments lists the caller's knowledge documents
// (GET /v1/knowledge/documents).
func (c *Client) ListDocuments(ctx context.Context) (*KnowledgeDocumentList, error) {
	var out KnowledgeDocumentList
	if err := c.do(ctx, httpMethodGet, "/knowledge/documents", nil, nil, &out); err != nil {
		return nil, err
	}
	if out.Documents == nil {
		out.Documents = []KnowledgeDocument{}
	}
	return &out, nil
}

// AddDocument ingests a document (chunk -> embed -> store;
// POST /v1/knowledge/documents).
func (c *Client) AddDocument(ctx context.Context, req AddDocumentRequest) (*AddDocumentResponse, error) {
	var out AddDocumentResponse
	if err := c.do(ctx, httpMethodPost, "/knowledge/documents", nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Search retrieves the top-k chunks for a query (POST /v1/knowledge/search).
// k <= 0 lets the server apply its default.
func (c *Client) Search(ctx context.Context, query string, k int) (*SearchResults, error) {
	body := map[string]any{"query": query}
	if k > 0 {
		body["k"] = k
	}
	var out SearchResults
	if err := c.do(ctx, httpMethodPost, "/knowledge/search", nil, body, &out); err != nil {
		return nil, err
	}
	if out.Results == nil {
		out.Results = []SearchResult{}
	}
	return &out, nil
}
