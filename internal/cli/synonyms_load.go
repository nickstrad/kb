package cli

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/search"
	"github.com/nickstrad/kb/internal/workspace"
)

// loadSynonyms finds the synonyms a search of ws uses (PLAN 5.5). With search.synonyms set, that
// file is the only source (a path under the location root, or ~/...) and a missing file is an
// error: the user named it. Otherwise it is <store dir>/synonyms.txt, then the global location's
// default store's, and no file anywhere is no synonyms (nil, nil). A file that cannot be read
// (too large, not a regular file) fails the command.
func (d *deps) loadSynonyms(cmd *cobra.Command, ws *workspace.Workspace, cfg config.Config) (*search.Synonyms, error) {
	if p := cfg.Search.Synonyms; p != "" {
		path, err := workspace.ExpandPath(p, ws.Root, d.workspaceEnv().Home)
		if err != nil {
			return nil, err
		}
		syn, err := search.LoadSynonyms(path)
		if err != nil {
			return nil, errs.Wrap(err, errs.KindOf(err), "cli.loadSynonyms", "search.synonyms: %v", err)
		}
		return syn, nil
	}
	own := filepath.Join(ws.StoreDir, search.SynonymsFile)
	syn, err := search.LoadSynonyms(own)
	if err == nil || errs.KindOf(err) != errs.NotFound {
		return syn, err
	}
	global, ok := d.globalSynonymsPath(cmd)
	if !ok || global == own {
		return nil, nil
	}
	syn, err = search.LoadSynonyms(global)
	if err != nil && errs.KindOf(err) == errs.NotFound {
		return nil, nil
	}
	return syn, err
}

// globalSynonymsPath is where the global location's default store keeps its synonyms. ok is false
// when there is no global location (or its kb.toml is unusable, which only earns a warning here:
// the search itself does not need it).
func (d *deps) globalSynonymsPath(cmd *cobra.Command) (path string, ok bool) {
	loc, info, err := d.resolver(cmd, config.ReadLocation).Location(workspace.Ref{Global: true})
	if err != nil {
		if errs.KindOf(err) != errs.NotFound {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: global synonyms skipped: %v\n", err)
		}
		return "", false
	}
	gws, err := loc.StoreAt(info.DefaultStore)
	if err != nil {
		return "", false
	}
	return filepath.Join(gws.StoreDir, search.SynonymsFile), true
}
