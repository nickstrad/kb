package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanCountsSides(t *testing.T) {
	dir := t.TempDir()
	write := func(name, src string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("p.go", `package p

import "time"

const (
	MaxChars = 1500
	Ratio    = 0.5
	Name     = "not numeric"
	Neg      = -3
)

var Window = 30 * time.Second

var notLimit = make([]int, 3)
`)
	write("p_test.go", `package p
var _ = MaxChars
var _ = MaxChars - 1
var _ = MaxChars+1
var _ = Ratio
`)
	ls, err := scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]limit{}
	for _, l := range ls {
		got[l.Name] = l
	}
	if len(got) != 4 {
		t.Fatalf("limits = %v, want MaxChars, Ratio, Neg, Window", ls)
	}
	tests := []struct {
		name   string
		status string
	}{
		{"MaxChars", "ok"}, {"Ratio", "part"}, {"Neg", "none"}, {"Window", "none"},
	}
	for _, tt := range tests {
		if s := status(got[tt.name]); s != tt.status {
			t.Errorf("%s: status %s, want %s (%+v)", tt.name, s, tt.status, got[tt.name])
		}
	}
	if got["Window"].Value != "30*time.Second" || got["Neg"].Value != "-3" {
		t.Errorf("values: Window %q, Neg %q", got["Window"].Value, got["Neg"].Value)
	}
}
