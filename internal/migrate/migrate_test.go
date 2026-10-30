package migrate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// steps builds n well-formed steps for artefact "art" whose Up appends From to the handle.
func steps(n int) Steps[*[]int] {
	var s Steps[*[]int]
	for i := range n {
		s = append(s, Step[*[]int]{
			Info: Info{From: i, ID: fmt.Sprintf("art/%04d-step-%d", i+1, i), Desc: "step", Effect: Effect(i % int(effectEnd))},
			Up: func(_ context.Context, h *[]int) error {
				*h = append(*h, i)
				return nil
			},
		})
	}
	return s
}

func TestEffectString(t *testing.T) {
	for _, tc := range []struct {
		e    Effect
		want string
	}{
		{EffectNone, "none"},
		{EffectReindex, "reindex"},
		{EffectReembed, "reembed"},
		{effectEnd, "effect(3)"},
		{255, "effect(255)"},
	} {
		if got := tc.e.String(); got != tc.want {
			t.Errorf("Effect(%d).String() = %q, want %q", tc.e, got, tc.want)
		}
	}
}

// TestCheck is the Steps.Check decision table: every way a list can be malformed, and the
// boundary cases that are well-formed.
func TestCheck(t *testing.T) {
	mod := func(f func(s Steps[*[]int]) Steps[*[]int]) Steps[*[]int] { return f(slices.Clone(steps(3))) }
	for _, tc := range []struct {
		name  string
		steps Steps[*[]int]
		want  string // substring of the error; "" = valid
	}{
		{"empty list", nil, ""},
		{"one step", steps(1), ""},
		{"three steps", steps(3), ""},
		{"gap: From skips", mod(func(s Steps[*[]int]) Steps[*[]int] { return append(s[:1], s[2:]...) }), "From is 2, want 1"},
		{"From != index at 0", mod(func(s Steps[*[]int]) Steps[*[]int] { s[0].From = 1; return s }), "From is 1, want 0"},
		{"reordered", mod(func(s Steps[*[]int]) Steps[*[]int] { s[1], s[2] = s[2], s[1]; return s }), "From is 2, want 1"},
		{"duplicate ID", mod(func(s Steps[*[]int]) Steps[*[]int] {
			s = append(s, s[2])
			s[3].From = 3
			return s
		}), "numbered 0003, want 0004"},
		{"duplicate ID same number", mod(func(s Steps[*[]int]) Steps[*[]int] {
			s[2].ID = s[1].ID
			s[2].From, s[1].From = 2, 1
			return s
		}), "numbered 0002, want 0003"},
		{"ID without artefact", mod(func(s Steps[*[]int]) Steps[*[]int] { s[1].ID = "0002-x"; return s }), "is not <artefact>/<NNNN>-<slug>"},
		{"ID with three digits", mod(func(s Steps[*[]int]) Steps[*[]int] { s[1].ID = "art/002-x"; return s }), "is not"},
		{"ID with upper-case slug", mod(func(s Steps[*[]int]) Steps[*[]int] { s[1].ID = "art/0002-X"; return s }), "is not"},
		{"ID with trailing dash", mod(func(s Steps[*[]int]) Steps[*[]int] { s[1].ID = "art/0002-x-"; return s }), "is not"},
		{"ID with empty slug", mod(func(s Steps[*[]int]) Steps[*[]int] { s[1].ID = "art/0002-"; return s }), "is not"},
		{"ID of another artefact", mod(func(s Steps[*[]int]) Steps[*[]int] { s[1].ID = "other/0002-x"; return s }), `belongs to artefact "other"`},
		{"ID number off by one", mod(func(s Steps[*[]int]) Steps[*[]int] { s[1].ID = "art/0001-x"; return s }), "numbered 0001, want 0002"},
		{"empty Desc", mod(func(s Steps[*[]int]) Steps[*[]int] { s[1].Desc = ""; return s }), "Desc must be one non-empty line"},
		{"blank Desc", mod(func(s Steps[*[]int]) Steps[*[]int] { s[1].Desc = " \t"; return s }), "Desc must be one non-empty line"},
		{"two-line Desc", mod(func(s Steps[*[]int]) Steps[*[]int] { s[1].Desc = "a\nb"; return s }), "Desc must be one non-empty line"},
		{"CR in Desc", mod(func(s Steps[*[]int]) Steps[*[]int] { s[1].Desc = "a\rb"; return s }), "Desc must be one non-empty line"},
		{"last valid effect", mod(func(s Steps[*[]int]) Steps[*[]int] { s[1].Effect = effectEnd - 1; return s }), ""},
		{"unknown effect", mod(func(s Steps[*[]int]) Steps[*[]int] { s[1].Effect = effectEnd; return s }), "unknown effect 3"},
		{"nil Up", mod(func(s Steps[*[]int]) Steps[*[]int] { s[1].Up = nil; return s }), "Up is nil"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.steps.Check("art")
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("Check = %v, want nil", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("Check = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

// TestPending covers the boundaries of current: 0, inside, Target-1, Target, Target+1.
func TestPending(t *testing.T) {
	s := steps(3)
	for _, tc := range []struct {
		current int
		want    []int // From of each pending step
	}{
		{0, []int{0, 1, 2}},
		{1, []int{1, 2}},
		{2, []int{2}},
		{3, nil},
		{4, nil},
	} {
		got := s.Pending(tc.current)
		var froms []int
		for _, in := range got {
			froms = append(froms, in.From)
		}
		if !slices.Equal(froms, tc.want) {
			t.Errorf("Pending(%d) = %v, want %v", tc.current, froms, tc.want)
		}
		if tc.want == nil && got != nil {
			t.Errorf("Pending(%d) = %#v, want nil", tc.current, got)
		}
	}
	if got := Steps[*[]int](nil).Pending(0); got != nil {
		t.Errorf("empty Pending(0) = %v", got)
	}
	if got := s.Target(); got != 3 {
		t.Errorf("Target = %d, want 3", got)
	}
}

// TestUp: steps run in order from current, each followed by its stamp; a failing step or stamp
// stops the run with the step ID in the message and the cause wrapped.
func TestUp(t *testing.T) {
	ctx := context.Background()
	errBoom := errors.New("boom")
	for _, tc := range []struct {
		name      string
		current   int
		failStep  int // From of the step that fails, -1 none
		failStamp int // version whose stamp fails, -1 none
		wantRan   []int
		wantDone  int
		wantErr   string
	}{
		{"from 0", 0, -1, -1, []int{0, 1, 2}, 3, ""},
		{"from 2", 2, -1, -1, []int{2}, 1, ""},
		{"at target", 3, -1, -1, nil, 0, ""},
		{"step fails", 0, 1, -1, []int{0}, 1, "step art/0002-step-1 (v1 to v2)"},
		{"stamp fails", 0, -1, 2, []int{0, 1}, 1, "stamp version 2 after art/0002-step-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := steps(3)
			if tc.failStep >= 0 {
				s[tc.failStep].Up = func(context.Context, *[]int) error { return errBoom }
			}
			var ran []int
			var stamps []int
			done, err := s.Up(ctx, &ran, tc.current, func(_ context.Context, _ *[]int, v int) error {
				if v == tc.failStamp {
					return errBoom
				}
				stamps = append(stamps, v)
				return nil
			})
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr) || !errors.Is(err, errBoom)) {
				t.Fatalf("Up err = %v, want %q wrapping boom", err, tc.wantErr)
			}
			if !slices.Equal(ran, tc.wantRan) {
				t.Errorf("ran %v, want %v", ran, tc.wantRan)
			}
			if len(done) != tc.wantDone {
				t.Errorf("done = %v, want %d steps", done, tc.wantDone)
			}
			for i, v := range stamps {
				if v != tc.current+i+1 {
					t.Errorf("stamps = %v, want consecutive from %d", stamps, tc.current+1)
				}
			}
		})
	}
}

// TestUpCancelled: a cancelled ctx stops before the next step.
func TestUpCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := steps(3)
	s[0].Up = func(_ context.Context, h *[]int) error { *h = append(*h, 0); cancel(); return nil }
	var ran []int
	done, err := s.Up(ctx, &ran, 0, func(context.Context, *[]int, int) error { return nil })
	if !errors.Is(err, context.Canceled) || len(done) != 1 || !slices.Equal(ran, []int{0}) {
		t.Fatalf("Up after cancel: done %v, ran %v, err %v", done, ran, err)
	}
}

func TestCombined(t *testing.T) {
	for _, tc := range []struct {
		effects []Effect
		want    Effect
	}{
		{nil, EffectNone},
		{[]Effect{EffectNone}, EffectNone},
		{[]Effect{EffectReindex, EffectNone}, EffectReindex},
		{[]Effect{EffectReembed, EffectReindex}, EffectReembed},
		{[]Effect{EffectNone, EffectReembed}, EffectReembed},
	} {
		var infos []Info
		for _, e := range tc.effects {
			infos = append(infos, Info{Effect: e})
		}
		if got := Combined(infos); got != tc.want {
			t.Errorf("Combined(%v) = %v, want %v", tc.effects, got, tc.want)
		}
	}
}

func TestStatusNewer(t *testing.T) {
	for _, tc := range []struct {
		current, target int
		want            bool
	}{{0, 3, false}, {2, 3, false}, {3, 3, false}, {4, 3, true}} {
		if got := (Status{Current: tc.current, Target: tc.target}).Newer(); got != tc.want {
			t.Errorf("Newer(%d, %d) = %v", tc.current, tc.target, got)
		}
	}
}
