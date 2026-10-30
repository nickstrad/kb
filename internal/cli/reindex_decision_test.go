package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/embed/fake"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/store"
)

// metaIndex is a store.Index whose embed_meta calls are scripted: ensure[i] is the result of the
// i-th EnsureEmbedMeta call; reset is ResetIndex's result. Other methods are not used.
type metaIndex struct {
	store.Index
	ensure []error
	calls  int
	reset  error
	resets int
}

func (m *metaIndex) EnsureEmbedMeta(context.Context, store.EmbedMeta) error {
	m.calls++
	if m.calls <= len(m.ensure) {
		return m.ensure[m.calls-1]
	}
	return nil
}

func (m *metaIndex) ResetIndex(context.Context) error { m.resets++; return m.reset }

// pingEmbedder is a fake embedder with a scripted Ping.
type pingEmbedder struct {
	*fake.Embedder
	err error
}

func (p pingEmbedder) Ping(context.Context) error { return p.err }

// TestEnsureEmbedMetaForReindexDecision is the TESTING.md §4.2 table for the reindex mismatch
// path: one row per branch outcome.
func TestEnsureEmbedMetaForReindexDecision(t *testing.T) {
	mismatch := errs.Wrap(&store.EmbedMetaMismatchError{StoredModel: "a", StoredDim: 1, WantModel: "b", WantDim: 2},
		errs.Conflict, "sqlite.EnsureEmbedMeta", "mismatch")
	driver := errs.New(errs.Internal, "sqlite.x", "driver failed")
	down := errs.New(errs.Unavailable, "local.Load", "model not downloaded")
	for _, tc := range []struct {
		name      string
		ensure    []error
		reset     error
		ping      error
		all       bool
		wantForce bool
		wantKind  errs.Kind // checked when wantErr
		wantErr   error     // errors.Is target; nil = success
		wantReset int
	}{
		{"fresh or same meta: no force", nil, nil, nil, true, false, 0, nil, 0},
		{"other store error: returned as is", []error{driver}, nil, nil, true, false, errs.Internal, driver, 0},
		{"mismatch, one entry: Conflict, no reset", []error{mismatch}, nil, nil, false, false, errs.Conflict, store.ErrEmbedMetaMismatch, 0},
		{"mismatch, --all, embedder down: Unavailable, no reset", []error{mismatch}, nil, down, true, false, errs.Unavailable, down, 0},
		{"mismatch, --all, ping cancelled: the cancel, no reset", []error{mismatch}, nil, context.Canceled, true, false, errs.Internal, context.Canceled, 0},
		{"mismatch, --all: reset and force", []error{mismatch}, nil, nil, true, true, 0, nil, 1},
		{"mismatch, --all, reset fails", []error{mismatch}, driver, nil, true, false, errs.Internal, driver, 1},
		{"mismatch, --all, recording the new meta fails", []error{mismatch, driver}, nil, nil, true, false, errs.Internal, driver, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &metaIndex{ensure: tc.ensure, reset: tc.reset}
			var e embed.Embedder = pingEmbedder{fake.New("m", 4), tc.ping}
			var stderr bytes.Buffer
			force, err := ensureEmbedMetaForReindex(context.Background(), st, e, tc.all, &stderr)
			if st.resets != tc.wantReset {
				t.Errorf("ResetIndex calls = %d, want %d", st.resets, tc.wantReset)
			}
			if tc.wantErr == nil {
				if err != nil || force != tc.wantForce {
					t.Fatalf("= (%v, %v), want (%v, nil)", force, err, tc.wantForce)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) || errs.KindOf(err) != tc.wantKind {
				t.Fatalf("err = %v (kind %v), want %v of kind %v", err, errs.KindOf(err), tc.wantErr, tc.wantKind)
			}
		})
	}
}

// interruptingStore makes every entry lookup fail the way a statement stopped by
// sqlite3_interrupt does: an Interrupted kind with no context.Canceled in the chain.
type interruptingStore struct{ store.Store }

func (s interruptingStore) Entry(context.Context, string) (*store.EntryRow, error) {
	return nil, errs.New(errs.Interrupted, "sqlite.Entry", "interrupted")
}

// TestReindexInterruptedKind: an Interrupted error without context.Canceled in its chain gets the
// same friendly interrupted message and exit 1 as a cancelled ctx (TestReindexInterrupted).
func TestReindexInterruptedKind(t *testing.T) {
	root := newTempRoot(t)
	useNoneModel(t, root)
	writeFile(t, filepath.Join(root, "data", "a.md"), validFrontMatter("A", "a summary"))
	orig := testDeps.openStore
	testDeps.openStore = func(ctx context.Context, path string, ro bool) (store.Store, error) {
		st, err := orig(ctx, path, ro)
		if err != nil {
			return nil, err
		}
		return interruptingStore{st}, nil
	}
	t.Cleanup(func() { testDeps.openStore = orig })
	_, stderr, code := run(t, "reindex", "--all")
	if code != ExitUsage || !strings.Contains(stderr, "rerun kb reindex") {
		t.Fatalf("exit %d, stderr %q; want 1 with the interrupted/rerun message", code, stderr)
	}
}

// TestEnsureEmbedMetaForReindexNotice: on --all the mismatch notice says the index is being
// rebuilt (not the advice to run `kb reindex --all`, which is what is already running), and only
// once the embedder has passed its ping, so a refused reset does not claim a rebuild.
func TestEnsureEmbedMetaForReindexNotice(t *testing.T) {
	mismatch := errs.Wrap(&store.EmbedMetaMismatchError{StoredModel: "a", StoredDim: 1, WantModel: "b", WantDim: 2},
		errs.Conflict, "sqlite.EnsureEmbedMeta", "mismatch")
	down := errs.New(errs.Unavailable, "local.Load", "model not downloaded")
	for _, tc := range []struct {
		name       string
		ping       error
		wantNotice string // substring; "" = no notice
	}{
		{"ping ok: rebuilding notice", nil, "rebuilding the index"},
		{"ping failed: no rebuild claim", down, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &metaIndex{ensure: []error{mismatch}}
			var e embed.Embedder = pingEmbedder{fake.New("b", 2), tc.ping}
			var stderr bytes.Buffer
			_, _ = ensureEmbedMetaForReindex(context.Background(), st, e, true, &stderr)
			got := stderr.String()
			if tc.wantNotice == "" {
				if strings.Contains(got, "rebuil") {
					t.Fatalf("stderr %q claims a rebuild", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantNotice) || strings.Contains(got, "rebuild it with") {
				t.Fatalf("stderr = %q, want %q and no advice to rebuild", got, tc.wantNotice)
			}
		})
	}
}
