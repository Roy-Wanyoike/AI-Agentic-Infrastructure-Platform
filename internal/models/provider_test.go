package models

import (
	"context"
	"testing"
)

func TestStaticProviderRespondsWithConfiguredText(t *testing.T) {
	provider := NewStaticProvider("demo", "hello from model")
	resp, err := provider.Complete(context.Background(), CompletionRequest{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
	if resp.Text == "" {
		t.Fatal("response text should not be empty")
	}
	if provider.Name() == "" {
		t.Fatal("provider name should not be empty")
	}
}
