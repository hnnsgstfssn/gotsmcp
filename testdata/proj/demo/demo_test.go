package demo

import "testing"

func TestLoad(t *testing.T) {
	if Load().Name != "x" {
		t.Fatal("bad")
	}
}
