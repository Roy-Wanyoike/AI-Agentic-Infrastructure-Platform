package deployments

import "testing"

func TestServiceCreatesEnvironmentAndPromotesRelease(t *testing.T) {
	service := NewService()
	env, err := service.CreateEnvironment("staging")
	if err != nil {
		t.Fatalf("CreateEnvironment returned error: %v", err)
	}
	if env == nil || env.Name != "staging" {
		t.Fatal("created environment should have a valid name")
	}
	if env.Status != "ACTIVE" {
		t.Fatalf("new environment status should be ACTIVE, got %q", env.Status)
	}

	release, err := service.AddRelease("staging", "v1.2.3")
	if err != nil {
		t.Fatalf("AddRelease returned error: %v", err)
	}
	if release == nil || release.Version != "v1.2.3" {
		t.Fatal("release should capture the version")
	}
	if release.Status != "PENDING" {
		t.Fatalf("new release status should be PENDING, got %q", release.Status)
	}

	promoted, err := service.Promote("staging", "v1.2.3")
	if err != nil {
		t.Fatalf("Promote returned error: %v", err)
	}
	if promoted == nil || promoted.Status != "ACTIVE" {
		t.Fatal("promoted release should be ACTIVE")
	}
	if env.Status != "ACTIVE" {
		t.Fatalf("environment should remain ACTIVE after promotion, got %q", env.Status)
	}
	if history := service.History("staging"); len(history) != 1 {
		t.Fatalf("expected exactly one release in history, got %d", len(history))
	}
}

func TestServiceRejectsInvalidEnvironmentAndReleaseNames(t *testing.T) {
	service := NewService()
	if _, err := service.CreateEnvironment(" "); err == nil {
		t.Fatal("blank environment names should be rejected")
	}
	if _, err := service.AddRelease("production", ""); err == nil {
		t.Fatal("blank release versions should be rejected")
	}
	if _, err := service.Promote("missing", "v1"); err == nil {
		t.Fatal("promoting a missing environment should fail")
	}
}
