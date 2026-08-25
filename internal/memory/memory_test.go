package memory

import "testing"

func TestMemoryStoreAddsAndSearchesEntries(t *testing.T) {
	store := NewStore()
	if _, err := store.Add("note", "Acme is the customer"); err != nil {
		t.Fatalf("Add returned error: %v", err)
	}
	matches := store.Search("note", "Acme")
	if len(matches) == 0 {
		t.Fatal("search should return matching notes")
	}
}
