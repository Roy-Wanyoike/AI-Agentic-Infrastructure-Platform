package notifications

import (
    "fmt"
    "sync"
    "time"
)

type Message struct {
    ID        string
    UserID    string
    Channel   string
    Subject   string
    Body      string
    CreatedAt time.Time
}

type Service struct {
    mu       sync.Mutex
    messages []*Message
}

func NewService() *Service { return &Service{messages: make([]*Message, 0)} }

func (s *Service) Send(userID, channel, subject, body string) *Message {
    s.mu.Lock(); defer s.mu.Unlock()
    msg := &Message{ID: fmt.Sprintf("msg-%d", len(s.messages)+1), UserID: userID, Channel: channel, Subject: subject, Body: body, CreatedAt: time.Now().UTC()}
    s.messages = append(s.messages, msg)
    return msg
}

func (s *Service) List() []*Message {
    s.mu.Lock(); defer s.mu.Unlock()
    out := make([]*Message, len(s.messages))
    copy(out, s.messages)
    return out
}
