package scheduler

import "testing"

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
