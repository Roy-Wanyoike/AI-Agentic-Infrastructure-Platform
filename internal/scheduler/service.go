package scheduler

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Schedule struct {
	ID        string
	Name      string
	Cron      string
	Enabled   bool
	NextRunAt time.Time
	CreatedAt time.Time
}

type Service struct {
	mu        sync.Mutex
	schedules map[string]*Schedule
}

func NewService() *Service {
	return &Service{schedules: make(map[string]*Schedule)}
}

func (s *Service) Create(name, cron string) (*Schedule, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("schedule name is required")
	}
	cron = strings.TrimSpace(cron)
	if cron == "" {
		return nil, errors.New("cron expression is required")
	}
	if !isValidCron(cron) {
		return nil, errors.New("invalid cron expression")
	}
	s.mu.Lock(); defer s.mu.Unlock()
	schedule := &Schedule{
		ID:        fmt.Sprintf("schedule-%d", len(s.schedules)+1),
		Name:      name,
		Cron:      cron,
		Enabled:   true,
		NextRunAt: time.Now().UTC().Add(5 * time.Minute),
		CreatedAt: time.Now().UTC(),
	}
	s.schedules[schedule.ID] = schedule
	return schedule, nil
}

func (s *Service) Get(id string) (*Schedule, bool) {
	s.mu.Lock(); defer s.mu.Unlock()
	schedule, ok := s.schedules[id]
	return schedule, ok
}

func (s *Service) Toggle(id string, enabled bool) error {
	schedule, ok := s.Get(id)
	if !ok {
		return errors.New("schedule not found")
	}
	schedule.Enabled = enabled
	return nil
}

func (s *Service) ShouldRun(schedule *Schedule, at time.Time) bool {
	if schedule == nil || !schedule.Enabled {
		return false
	}
	fields := strings.Fields(schedule.Cron)
	if len(fields) != 5 {
		return false
	}
	minute := at.Minute()
	hour := at.Hour()
	dayOfMonth := at.Day()
	month := int(at.Month())
	dayOfWeek := int(at.Weekday())

	return matchesField(fields[0], minute, 0, 59) &&
		matchesField(fields[1], hour, 0, 23) &&
		matchesField(fields[2], dayOfMonth, 1, 31) &&
		matchesField(fields[3], month, 1, 12) &&
		matchesField(fields[4], dayOfWeek, 0, 6)
}

func isValidCron(expr string) bool {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return false
	}
	for _, field := range fields {
		if !matchesFieldSpec(field, 0, 59) {
			return false
		}
	}
	return true
}

func matchesField(field string, value, min, max int) bool {
	if field == "*" {
		return true
	}
	if strings.Contains(field, "/") {
		parts := strings.SplitN(field, "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return false
		}
		step, err := strconv.Atoi(parts[1])
		if err != nil || step <= 0 {
			return false
		}
		base := parts[0]
		if base == "*" {
			return value%step == 0
		}
		start, end, ok := parseRange(base, min, max)
		if !ok {
			return false
		}
		return value >= start && value <= end && (value-start)%step == 0
	}
	if strings.Contains(field, "-") {
		start, end, ok := parseRange(field, min, max)
		if !ok {
			return false
		}
		return value >= start && value <= end
	}
	if strings.Contains(field, ",") {
		for _, option := range strings.Split(field, ",") {
			if option == "" {
				return false
			}
			if matchesField(option, value, min, max) {
				return true
			}
		}
		return false
	}

	parsed, err := strconv.Atoi(field)
	if err != nil {
		return false
	}
	return value == parsed
}

func parseRange(field string, min, max int) (int, int, bool) {
	parts := strings.SplitN(field, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	start, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	end, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, false
	}
	if start < min || end > max || start > end {
		return 0, 0, false
	}
	return start, end, true
}

func matchesFieldSpec(field string, min, max int) bool {
	if field == "*" {
		return true
	}
	if strings.Contains(field, "/") {
		parts := strings.SplitN(field, "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return false
		}
		step, err := strconv.Atoi(parts[1])
		if err != nil || step <= 0 {
			return false
		}
		if parts[0] == "*" {
			return true
		}
		_, _, ok := parseRange(parts[0], min, max)
		return ok || isSingleValue(parts[0], min, max)
	}
	if strings.Contains(field, ",") {
		for _, item := range strings.Split(field, ",") {
			if !matchesFieldSpec(item, min, max) {
				return false
			}
		}
		return true
	}
	if strings.Contains(field, "-") {
		_, _, ok := parseRange(field, min, max)
		return ok
	}
	return isSingleValue(field, min, max)
}

func isSingleValue(field string, min, max int) bool {
	if field == "" {
		return false
	}
	value, err := strconv.Atoi(field)
	if err != nil {
		return false
	}
	return value >= min && value <= max
}
