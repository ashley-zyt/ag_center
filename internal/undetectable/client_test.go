package undetectable

import "testing"

func TestFindProfileIDByName(t *testing.T) {
	profiles := Profiles{
		"id1": {Name: "a"},
		"id2": {Name: "b"},
	}
	id, err := FindProfileIDByName(profiles, "b")
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if id != "id2" {
		t.Fatalf("expected id2, got %s", id)
	}
}

func TestFindProfileIDByName_NotFound(t *testing.T) {
	profiles := Profiles{
		"id1": {Name: "a"},
	}
	_, err := FindProfileIDByName(profiles, "x")
	if err == nil {
		t.Fatalf("expected error")
	}
}

func TestFindProfileIDByName_Duplicate(t *testing.T) {
	profiles := Profiles{
		"id1": {Name: "dup"},
		"id2": {Name: "dup"},
	}
	_, err := FindProfileIDByName(profiles, "dup")
	if err == nil {
		t.Fatalf("expected error")
	}
}
