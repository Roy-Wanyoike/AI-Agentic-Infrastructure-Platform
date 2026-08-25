package audit

import (
    "fmt"
    "sync"
    "time"
)

type Entry struct {
    ID            string
    Actor         string
    Action        string
    OrganizationID string
    Resource      string
    CreatedAt     time.Time
}

type Service struct {
    mu     sync.Mutex
    items  []*Entry
}

func NewService() *Service { return &Service{items: make([]*Entry, 0)} }

func (s *Service) Log(actor, action, organizationID, resource string) *Entry {
    s.mu.Lock(); defer s.mu.Unlock()
    entry := &Entry{
        ID:            fmt.Sprintf("audit-%d", len(s.items)+1),
        Actor:         actor,
        Action:        action,
        OrganizationID: organizationID,
        Resource:      resource,
        CreatedAt:     time.Now().UTC(),
    }
    s.items = append(s.items, entry)
    return entry
}

func (s *Service) List() []*Entry {
    s.mu.Lock(); defer s.mu.Unlock()
    out := make([]*Entry, len(s.items))
    copy(out, s.items)
    return out
}
