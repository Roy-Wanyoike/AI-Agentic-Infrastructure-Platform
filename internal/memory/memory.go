package memory

import (
	"errors"
	"strings"
	"sync"
	"time"
)

type Entry struct {
	ID        string
	Kind      string
	Content   string
	CreatedAt time.Time
}

type Store struct {
	mu    sync.Mutex
	items []Entry
}

func NewStore() *Store {
	return &Store{items: make([]Entry, 0)}
}

func (s *Store) Add(kind, content string) (*Entry, error) {
	if s == nil {
		return nil, errors.New("memory store is required")
	}
	if strings.TrimSpace(kind) == "" {
		return nil, errors.New("kind is required")
	}
	if strings.TrimSpace(content) == "" {
		return nil, errors.New("content is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := Entry{ID: kind + "-" + time.Now().UTC().Format(time.RFC3339Nano), Kind: kind, Content: content, CreatedAt: time.Now().UTC()}
	s.items = append(s.items, entry)
	return &entry, nil
}

func (s *Store) Search(kind, query string) []Entry {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(query) == "" {
		return append([]Entry(nil), s.items...)
	}
	matches := make([]Entry, 0)
	for _, item := range s.items {
		if kind != "" && item.Kind != kind {
			continue
		}
		if strings.Contains(strings.ToLower(item.Content), strings.ToLower(query)) {
			matches = append(matches, item)
		}
	}
	return matches
}
