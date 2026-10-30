// The open/embed/search pipeline kb search and kb context share, and the Searcher setup kb explain
// shares with them.
package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/search"
	"github.com/nickstrad/kb/internal/store"
	"github.com/nickstrad/kb/internal/workspace"
)

// newSearcher builds the Searcher over st: settings from cfg, the synonym set, and (when logged)
// the search log in st and the clock. memo is the shared query memo of a cross-store search (nil
// for none). It returns the settings too, which the extra stores reuse. An explain Searcher has
// no Log, so it cannot write the search log.
func (d *deps) newSearcher(cmd *cobra.Command, ws *workspace.Workspace, cfg config.Config, st store.Store, embedder embed.Embedder, memo *search.QueryMemo, logged bool) (*search.Searcher, search.Settings, error) {
	settings, err := search.NewSettings(cfg)
	if err != nil {
		return nil, search.Settings{}, err
	}
	syn, err := d.loadSynonyms(cmd, ws, cfg)
	if err != nil {
		return nil, search.Settings{}, err
	}
	settings = settings.WithSynonyms(syn.Hash())
	s := &search.Searcher{Store: st, Embedder: embedder, KBVersion: Version, Settings: settings, Queries: memo, Synonyms: syn}
	if logged {
		s.Log = st
		s.Now = d.clock
	}
	return s, settings, nil
}

// storesSearch is an open logged search over this store and its extra stores.
type storesSearch struct {
	ws       *workspace.Workspace
	cfg      config.Config
	st       store.Store
	searcher *search.Searcher
	extras   *extraStores
	mode     string // the mode actually run (hybrid becomes fts with the none embedder)
}

// close releases the extra stores and the primary store.
func (s *storesSearch) close() {
	s.extras.close()
	s.st.Close()
}

// openStoresSearch is the setup of kb search and kb context: workspace and config, the index (never
// created), the embedder for mode, the Searcher, and the extra stores (fanOut: --stores was given,
// else [search] stores applies). op names the command in errors. On success the caller closes it.
func (d *deps) openStoresSearch(cmd *cobra.Command, stderr io.Writer, op, mode string, fanOut bool, stores string) (*storesSearch, error) {
	ws, cfg, err := d.workspaceConfig(cmd)
	if err != nil {
		return nil, err
	}
	embs := embedderSet{}
	st, embedder, mode, err := d.openSearchWith(cmd, ws, cfg, mode, op, false, embs)
	if err != nil {
		return nil, err
	}
	memo := &search.QueryMemo{}
	searcher, settings, err := d.newSearcher(cmd, ws, cfg, st, embedder, memo, true)
	if err != nil {
		st.Close()
		return nil, err
	}
	refs := fanOutRefs(fanOut, stores, cfg.Search.Stores)
	extras, err := d.openExtraStores(cmd, ws, refs, mode, settings, memo, embs)
	s := &storesSearch{ws: ws, cfg: cfg, st: st, searcher: searcher, extras: extras, mode: mode}
	if err != nil {
		s.close()
		return nil, err
	}
	if n := embs.models(); len(n) > 2 {
		fmt.Fprintf(stderr, "warning: this search loads %d embedding models (%s); each one takes memory and time\n", len(n), strings.Join(n, ", "))
	}
	return s, nil
}

// storesResult is what a stores search produced: the outcome, the entry lookups by Store tag for
// rendering ("" = this store), and the error logging the search (nil when logged).
type storesResult struct {
	out     *search.Outcome
	lookups map[string]entryLookup
	logErr  error
}

// run searches this store and the extras for req, warns about skipped and degraded stores, and logs
// the search in this store only.
func (s *storesSearch) run(cmd *cobra.Command, d *deps, stderr io.Writer, req search.Request) (storesResult, error) {
	ctx := cmd.Context()
	parts := append([]search.Part{{Root: s.ws.Root, Searcher: s.searcher}}, s.extras.parts...)
	mo, err := search.SearchStores(ctx, parts, req)
	if err != nil {
		return storesResult{}, err
	}
	d.warnSkipped(cmd, mo.Skipped)
	for _, sk := range mo.Degraded {
		fmt.Fprintf(stderr, "warning: store %s searched with FTS only: %s\n", sk.Part.Label, sk.Err)
	}
	_, logErr := s.searcher.Record(ctx, req, mo.Outcome)
	lookups := s.extras.lookups()
	lookups[""] = s.st
	return storesResult{out: mo.Outcome, lookups: lookups, logErr: logErr}, nil
}
