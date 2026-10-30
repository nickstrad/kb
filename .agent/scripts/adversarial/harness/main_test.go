package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestSetUserVersion(t *testing.T) {
	p := filepath.Join(t.TempDir(), "db")
	hdr := make([]byte, 100)
	copy(hdr, "SQLite format 3\x00")
	if err := os.WriteFile(p, hdr, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setUserVersion(p, 99); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !bytes.Equal(b[60:64], []byte{0, 0, 0, 99}) {
		t.Fatalf("user_version bytes = %v", b[60:64])
	}
	if err := os.WriteFile(p, []byte("not sqlite"), 0o644); err != nil {
		t.Fatal(err)
	}
	if setUserVersion(p, 1) == nil {
		t.Fatal("non-SQLite file accepted")
	}
}

func TestInvariants(t *testing.T) {
	tests := []struct {
		name   string
		r      res
		failed bool
	}{
		{"exit 0", res{Code: 0}, false},
		{"exit 2", res{Code: 2}, false},
		{"exit 3 breaks the contract", res{Code: 3}, true},
		{"panic on stderr", res{Code: 2, Stderr: "panic: boom\n\ngoroutine 1 [running]:"}, true},
		{"word panic in a message is fine", res{Code: 1, Stderr: "kb: query mentions panic: none"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &H{}
			h.invariants(tt.r)
			if got := len(h.failures) > 0; got != tt.failed {
				t.Fatalf("failed = %v, want %v (%v)", got, tt.failed, h.failures)
			}
		})
	}
}

func TestScenarioNamesUnique(t *testing.T) {
	seen := map[string]bool{}
	name := regexp.MustCompile(`^[a-z0-9-]+$`)
	for _, s := range scenarios {
		if seen[s.Name] || !name.MatchString(s.Name) || s.Desc == "" || s.Run == nil {
			t.Errorf("bad or duplicate scenario %q", s.Name)
		}
		seen[s.Name] = true
	}
}

func TestEnvHasNoNetworkAndTempHome(t *testing.T) {
	h := &H{dir: "/x", root: "/x/root", home: "/x/home"}
	env := strings.Join(h.env(), "\n")
	for _, want := range []string{"HOME=/x/home", "KB_ROOT=/x/root", "HTTPS_PROXY=" + deadProxy, "REMBED_CACHE=/x/home/"} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %q", want)
		}
	}
}
