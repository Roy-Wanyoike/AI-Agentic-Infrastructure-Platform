package scheduler

import (
	"testing"
	"time"
)

func TestCreateAndToggleSchedule(t *testing.T) {
	service := NewService()
	schedule, err := service.Create("nightly cleanup", "0 0 * * *")
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if schedule.ID == "" {
		t.Fatal("schedule ID should be populated")
	}
	if err := service.Toggle(schedule.ID, false); err != nil {
		t.Fatalf("Toggle returned error: %v", err)
	}
	if schedule.Enabled {
		t.Fatal("schedule should be disabled after toggle")
	}
}

func TestScheduleMatchesCronWindow(t *testing.T) {
	service := NewService()
	schedule, err := service.Create("hourly cleanup", "0 * * * *")
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	at := time.Date(2024, time.January, 1, 12, 0, 0, 0, time.UTC)
	if !service.ShouldRun(schedule, at) {
		t.Fatal("schedule should match the current cron minute")
	}
	if service.ShouldRun(schedule, at.Add(time.Minute)) {
		t.Fatal("schedule should not fire outside the matching cron minute")
	}
}

func TestScheduleRejectsInvalidCron(t *testing.T) {
	service := NewService()
	if _, err := service.Create("bad schedule", "invalid"); err == nil {
		t.Fatal("invalid cron should be rejected")
	}
}
