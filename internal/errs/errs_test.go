package errs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
)

func TestWrapKeepsChainAndKind(t *testing.T) {
	inner := New(Conflict, "store.EnsureEmbedMeta", "model mismatch").With("db", "/x/kb.sqlite")
	mid := fmt.Errorf("reindex a.md: %w", inner)
	outer := Wrap(mid, KindOf(mid), "reindex.One", "could not reindex a.md")

	if got := KindOf(outer); got != Conflict {
		t.Fatalf("KindOf = %v, want conflict", got)
	}
	if outer.Error() != "could not reindex a.md" {
		t.Fatalf("Error() = %q, want the friendly message only", outer.Error())
	}
	if !errors.Is(outer, inner) {
		t.Fatal("errors.Is lost the inner *Error")
	}
	if string(outer.Stack) != string(inner.Stack) || !outer.Time.Equal(inner.Time) {
		t.Fatal("Wrap captured a second stack instead of keeping the inner one")
	}
}

func TestWrapCapturesStackForForeignError(t *testing.T) {
	e := Wrap(fs.ErrNotExist, NotFound, "entry.Load", "no such entry")
	if len(e.Stack) == 0 || e.Time.IsZero() {
		t.Fatal("no stack or time recorded at the first wrap")
	}
	if !errors.Is(e, fs.ErrNotExist) {
		t.Fatal("errors.Is lost the foreign cause")
	}
}

func TestWrapNilIsNeverNil(t *testing.T) {
	var err error = Wrap(nil, Usage, "op", "bad")
	if err == nil || err.Error() != "bad" || errors.Unwrap(err) != nil {
		t.Fatalf("Wrap(nil) = %v", err)
	}
}

func TestKindOf(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want Kind
	}{
		{"nil", nil, Internal},
		{"plain", errors.New("x"), Internal},
		{"canceled", context.Canceled, Internal},
		{"outermost wins", Wrap(New(Usage, "a", "a"), Unavailable, "b", "b"), Unavailable},
		{"through fmt", fmt.Errorf("ctx: %w", New(NotFound, "a", "a")), NotFound},
	}
	for _, tc := range cases {
		if got := KindOf(tc.err); got != tc.want {
			t.Errorf("%s: KindOf = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestErrorFallsBackToInnerText(t *testing.T) {
	e := &Error{Err: errors.New("inner")}
	if e.Error() != "inner" {
		t.Fatalf("Error() = %q", e.Error())
	}
}

func TestFormatPlusV(t *testing.T) {
	e := Wrap(fmt.Errorf("driver: %w", errors.New("disk I/O error")), Internal, "sqlite.ReplaceEntry", "could not write").
		With("path", "data/a.md", "db")
	full := fmt.Sprintf("%+v", e)
	for _, want := range []string{"sqlite.ReplaceEntry [internal] could not write {db=<nil> path=data/a.md}",
		"caused by: driver: disk I/O error", "caused by: disk I/O error", "goroutine "} {
		if !strings.Contains(full, want) {
			t.Errorf("%%+v lacks %q:\n%s", want, full)
		}
	}
	if got := fmt.Sprintf("%v|%s", e, e); got != "could not write|could not write" {
		t.Errorf("%%v|%%s = %q", got)
	}
}

func TestKindString(t *testing.T) {
	want := map[Kind]string{Internal: "internal", Usage: "usage", NotFound: "not_found", Conflict: "conflict",
		Unavailable: "unavailable", Interrupted: "interrupted", Kind(42): "kind(42)", Kind(-1): "kind(-1)"}
	for k, s := range want {
		if k.String() != s {
			t.Errorf("Kind(%d).String() = %q, want %q", int(k), k.String(), s)
		}
	}
}
