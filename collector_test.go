package main

import "testing"

func TestNewCwCollectorWithoutSettings(t *testing.T) {
	c, err := NewCwCollector("a", "b", "c")
	if err == nil {
		t.Fatalf("err shouldn't be nil, got: %s", err)
	}

	if c != nil {
		t.Fatal("collector should be nil")
	}
}
