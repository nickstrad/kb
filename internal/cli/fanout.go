// The extra stores of a cross-store search (kb search --stores, [search] stores): which refs,
// resolving and opening them read-only, and the embedder each one searches with.
package cli

import (
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/search"
	"github.com/nickstrad/kb/internal/store"
	"github.com/nickstrad/kb/internal/workspace"
)

// allStores is the --stores word for every store of the current location (reserved: no store or
// alias can be named so).
const allStores = workspace.AllName

// fanOutRefs is the list of refs a search fans out to: --stores when given (comma-separated,
// blanks dropped, so --stores ” is none), else [search] stores.
func fanOutRefs(flagSet bool, flag string, configured []string) []string {
	if !flagSet {
		return configured
	}
	var out []string
	for _, r := range strings.Split(flag, ",") {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	return out
}

// embedderSet shares one embedder per distinct [embed] config within one command, so stores with
// the same model load it once. A nil set builds every time.
type embedderSet map[config.Embed]embed.Embedder

func (s embedderSet) get(key config.Embed, build func() (embed.Embedder, error)) (embed.Embedder, error) {
	if s == nil {
		return build()
	}
	if e, ok := s[key]; ok {
		return e, nil
	}
	e, err := build()
	if err != nil {
		return nil, err
	}
	s[key] = e
	return e, nil
}

// models are the distinct models the set loaded (none excluded), sorted.
func (s embedderSet) models() []string {
	var out []string
	for _, e := range s {
		if !embed.IsNone(e) && !slices.Contains(out, e.Model()) {
			out = append(out, e.Model())
		}
	}
	slices.Sort(out)
	return out
}

// extraStores are the opened extra stores of one search; close releases them.
type extraStores struct {
	parts  []search.Part
	stores []store.Store
}

func (x *extraStores) close() {
	for _, st := range x.stores {
		st.Close()
	}
}

// lookups maps each part's label (the hit's Store tag) to its store, for rendering.
func (x *extraStores) lookups() map[string]entryLookup {
	m := make(map[string]entryLookup, len(x.parts)+1)
	for i, p := range x.parts {
		m[p.Label] = x.stores[i]
	}
	return m
}

// openExtraStores resolves refs (all = every store of ws's location), drops the primary and
// duplicates (by canonical ref, the first spelling wins), refuses more than search.MaxExtraStores
// (Usage), and opens each read-only with its own embedder: a store that cannot take part (named
// nothing, no database, another schema, an unreadable config, an embed_meta mismatch, or no
// embedder for --mode vec) is skipped with one warning. A malformed ref is a Usage error. The
// returned set is never nil; the caller closes it, also on error.
func (d *deps) openExtraStores(cmd *cobra.Command, ws *workspace.Workspace, refs []string, mode string, settings search.Settings, memo *search.QueryMemo, embs embedderSet) (*extraStores, error) {
	x := &extraStores{}
	if len(refs) == 0 {
		return x, nil
	}
	expanded, err := expandAll(ws.Location, refs)
	if err != nil {
		return x, err
	}
	type target struct {
		label string
		ws    *workspace.Workspace
	}
	seen := map[string]bool{workspace.Canonical(ws.Store, ws.Root): true}
	var targets []target
	for _, ref := range expanded {
		tws, err := d.resolveFanOut(cmd, ref)
		if err != nil {
			return x, err
		}
		if tws == nil {
			continue
		}
		c := workspace.Canonical(tws.Store, tws.Root)
		if seen[c] {
			continue
		}
		seen[c] = true
		targets = append(targets, target{ref, tws})
	}
	if len(targets) > search.MaxExtraStores {
		return x, usageErr("--stores names %d other stores; a search fans out to at most %d", len(targets), search.MaxExtraStores)
	}
	for _, t := range targets {
		o, err := d.openResolvedReadOnly(cmd, t.label, t.ws)
		if err != nil {
			return x, err
		}
		if o == nil {
			continue
		}
		part, err := d.extraPart(cmd, t.label, o, mode, settings, memo, embs)
		if err != nil {
			o.Store.Close()
			d.skipExtra(cmd, t.label, err)
			continue
		}
		x.parts, x.stores = append(x.parts, part), append(x.stores, o.Store)
	}
	return x, nil
}

// skipExtra leaves one extra store out with its warning (DEFAULTS 175/206: a broken extra store
// never fails the search). An Internal error is a kb bug, not a store problem, so it is also
// recorded as an incident and the warning names its id (CODING_STYLE 1.5, 1.6: a worker does not
// decide that a store "doesn't matter"; the coordinator surfaces what it cannot classify). Under
// --debug the full chain follows.
func (d *deps) skipExtra(cmd *cobra.Command, label string, err error) {
	if errs.KindOf(err) != errs.Internal {
		warnSkip(cmd, label, "%v", err)
		return
	}
	id := newIncidentID()
	where := writeIncident(d, cmd, id, []string{cmd.CommandPath()}, incident{Kind: errs.Internal.String(), Chain: errs.Chain(err), Stack: stackOf(err)})
	warnSkip(cmd, label, "%v (internal error, a kb bug: id %s, details %s; please report it)", err, id, where)
	if debugFlag(cmd) {
		fmt.Fprintf(cmd.ErrOrStderr(), "%s\n", detail(err))
	}
}

// extraPart sets up the search of one opened extra store: its own config and embedder (checked
// against its embed_meta), the primary's settings. A none embedder searches it with fts.
func (d *deps) extraPart(cmd *cobra.Command, label string, o *openedStore, mode string, settings search.Settings, memo *search.QueryMemo, embs embedderSet) (search.Part, error) {
	cfg, err := d.loadConfig(cmd, o.Workspace)
	if err != nil {
		return search.Part{}, err
	}
	emb, pmode, err := d.searchEmbedderWith(cmd, o.Store, cfg, mode, "cli.search", embs)
	if err != nil {
		return search.Part{}, err
	}
	syn, err := d.loadSynonyms(cmd, o.Workspace, cfg)
	if err != nil {
		return search.Part{}, err
	}
	s := &search.Searcher{Store: o.Store, Embedder: emb, KBVersion: Version, Settings: settings, Queries: memo, Synonyms: syn}
	return search.Part{Ref: o.Canonical, Label: label, Root: o.Workspace.Root, Mode: pmode, Searcher: s}, nil
}

// expandAll replaces the word all with the names of every store of loc.
func expandAll(loc *workspace.Location, refs []string) ([]string, error) {
	if !slices.Contains(refs, allStores) {
		return refs, nil
	}
	names, _, err := loc.Stores()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range refs {
		if r == allStores {
			out = append(out, names...)
			continue
		}
		out = append(out, r)
	}
	return out, nil
}
