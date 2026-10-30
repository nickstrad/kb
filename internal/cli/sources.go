// Helpers for a store's [[sources]]: the entries directory kb add writes to, and the warnings for
// what discovery skipped.
package cli

import (
	"fmt"
	"io"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/entry"
)

// entriesSource returns the store's one entries source, the directory kb add copies into. A store
// with none (only in-place globs) or several has no single place to add to: a Usage error that
// names the config file.
func entriesSource(cfg config.Config, cfgPath string) (config.Source, error) {
	var found []config.Source
	for _, s := range cfg.Sources {
		if s.Layout == config.LayoutEntries {
			found = append(found, s)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return config.Source{}, usageErr("this store has no entries source to add to (%s lists no [[sources]] with layout = %q); "+
			"in-place files are indexed where they are: edit them and run kb reindex", cfgPath, config.LayoutEntries)
	default:
		return config.Source{}, usageErr("this store has %d entries sources in %s (%s, %s, ...); kb add needs exactly one",
			len(found), cfgPath, found[0], found[1])
	}
}

// warnSkipped prints one warning line per path discovery left out on purpose.
func warnSkipped(stderr io.Writer, skipped []entry.Skipped) {
	for _, s := range skipped {
		fmt.Fprintf(stderr, "warning: skipped %s\n", s)
	}
}
