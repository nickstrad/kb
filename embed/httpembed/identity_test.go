package httpembed

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Regression (review of 17192aa): with truncation on, send_dimensions decides whether the server
// shortens the vector or the client does, so the two give different vectors and must not share an
// identity (the embedding cache and embed_meta are keyed by Model() and Dim()).
func TestSendDimensionsChangesIdentity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if req.Dimensions == 2 {
			fmt.Fprint(w, `{"data":[{"index":0,"embedding":[1,0]}]}`)
			return
		}
		fmt.Fprint(w, `{"data":[{"index":0,"embedding":[0,1,1]}]}`)
	}))
	t.Cleanup(srv.Close)

	cfg := Config{BaseURL: srv.URL, Model: "m", Dim: 2, Truncate: true, SendDimensions: true}
	sent, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.SendDimensions = false
	unsent, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sv, err := sent.Embed(context.Background(), []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	uv, err := unsent.Embed(context.Background(), []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(sv) == fmt.Sprint(uv) {
		t.Fatalf("fixture broken: both settings returned %v", sv)
	}
	if sent.Model() == unsent.Model() {
		t.Errorf("send_dimensions changes the vectors (%v vs %v) but not Model() %q", sv, uv, sent.Model())
	}
}

// Regression (review of 17192aa): free-form fields must not be able to imitate a flag or another
// field. Every pair of distinct vector-changing settings drawn from hostile values must give a
// distinct Model().
func TestModelIdentityIsCollisionFree(t *testing.T) {
	type key struct {
		url, path, model, doc, query string
		send, trunc                  bool
	}
	urls := []string{"http://example", `http://example/embeddings" model="m`}
	paths := []string{"", "/x"}
	models := []string{"m", "m truncate", "m send_dimensions", `m doc_prefix="x"`, `m" truncate`, `m query_prefix="x"`}
	prefixes := []string{"", "x", `x" truncate`, ` query_prefix="x"`}
	seen := map[string]key{}
	for _, u := range urls {
		for _, p := range paths {
			for _, m := range models {
				for _, d := range prefixes {
					for _, q := range prefixes {
						for _, send := range []bool{false, true} {
							for _, trunc := range []bool{false, true} {
								k := key{u, p, m, d, q, send, trunc}
								c, err := New(Config{BaseURL: u, Path: p, Model: m, Dim: 2, DocPrefix: d, QueryPrefix: q, SendDimensions: send, Truncate: trunc})
								if err != nil {
									t.Fatal(err)
								}
								id := c.Model()
								if prev, ok := seen[id]; ok && prev != k {
									t.Fatalf("distinct settings collide on %q:\n  %+v\n  %+v", id, prev, k)
								}
								seen[id] = k
							}
						}
					}
				}
			}
		}
	}
	// The probe's exact pair, named so a failure reads plainly.
	a, _ := New(Config{BaseURL: "http://example", Model: "m truncate", Dim: 2})
	b, _ := New(Config{BaseURL: "http://example", Model: "m", Dim: 2, Truncate: true})
	if a.Model() == b.Model() {
		t.Errorf(`model "m truncate" collides with model "m" + truncate: %q`, a.Model())
	}
}

// Regression (review of 17192aa): a reply whose indexes are not exactly 0..n-1, each once, does
// not correspond to the request and must be an error, never silently reassigned vectors.
func TestEmbedRejectsInvalidResponseIndexes(t *testing.T) {
	cases := []struct {
		name    string
		indexes [2]int
		want    string // "" means the reply is valid
	}{
		{"in order", [2]int{0, 1}, ""},
		{"reversed", [2]int{1, 0}, ""},
		{"duplicate", [2]int{0, 0}, "index 0 more than once"},
		{"duplicate high", [2]int{1, 1}, "index 1 more than once"},
		{"negative", [2]int{-1, 1}, "index -1, want 0..1"},
		{"past the end", [2]int{0, 9}, "index 9, want 0..1"},
		{"equal to n", [2]int{2, 0}, "index 2, want 0..1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"data":[{"index":%d,"embedding":[1,0]},{"index":%d,"embedding":[0,1]}]}`, tc.indexes[0], tc.indexes[1])
			}))
			t.Cleanup(srv.Close)
			c, err := New(Config{BaseURL: srv.URL, Model: "m", Dim: 2})
			if err != nil {
				t.Fatal(err)
			}
			vecs, err := c.Embed(context.Background(), []string{"a", "b"})
			if tc.want == "" {
				if err != nil {
					t.Fatalf("valid reply rejected: %v", err)
				}
				// Vector for input i is the one the server labelled i.
				want := map[int][]float32{tc.indexes[0]: {1, 0}, tc.indexes[1]: {0, 1}}
				for i := range vecs {
					if fmt.Sprint(vecs[i]) != fmt.Sprint(want[i]) {
						t.Errorf("vector %d = %v, want %v", i, vecs[i], want[i])
					}
				}
				return
			}
			if err == nil {
				t.Fatalf("invalid indexes %v accepted: vectors=%v", tc.indexes, vecs)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}
