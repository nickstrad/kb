// Command loadfixture is the harness's SQLite helper (built next to the kb binary): it makes the
// older and oversized index databases the migrate-* scenarios attack, with the same driver and
// sqlite-vec build kb uses. It never runs against a real store.
//
//	loadfixture load   DB FIXTURE.sql   replace DB with the SQL text fixture (an older kb's file)
//	loadfixture fill   DB N             add N filler rows to entries, so a backup or a step takes a while
//	loadfixture setver DB V             set PRAGMA user_version (fake a newer kb's file)
//	loadfixture check  DB               print "<user_version> ok" when integrity_check passes; exit 1 otherwise
package main

import (
	"database/sql"
	"fmt"
	"os"
	"strconv"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	_ "github.com/mattn/go-sqlite3"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "loadfixture:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: loadfixture load|fill|setver|check DB [ARG]")
	}
	sqlite_vec.Auto()
	cmd, path := args[0], args[1]
	switch cmd {
	case "load":
		if len(args) != 3 {
			return fmt.Errorf("load DB FIXTURE.sql")
		}
		text, err := os.ReadFile(args[2])
		if err != nil {
			return err
		}
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		return exec(path, string(text))
	case "fill":
		if len(args) != 3 {
			return fmt.Errorf("fill DB N")
		}
		n, err := strconv.Atoi(args[2])
		if err != nil || n < 0 {
			return fmt.Errorf("fill: N must be a number")
		}
		return exec(path, fmt.Sprintf(`
			WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM seq WHERE i < %d)
			INSERT INTO entries (id, path, kind, title, summary, tags, body_hash, indexed_at)
			SELECT 100000 + i, 'filler/' || i || '.md', 'file', 'Filler ' || i, 'filler summary number ' || i,
			       '[]', 'hash-' || i, '2026-09-01T00:00:00Z' FROM seq`, n))
	case "setver":
		if len(args) != 3 {
			return fmt.Errorf("setver DB V")
		}
		v, err := strconv.Atoi(args[2])
		if err != nil {
			return fmt.Errorf("setver: V must be a number")
		}
		return exec(path, fmt.Sprintf("PRAGMA user_version = %d", v))
	case "check":
		db, err := sql.Open("sqlite3", "file:"+path+"?mode=ro")
		if err != nil {
			return err
		}
		defer db.Close()
		var v int
		var check string
		if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
			return err
		}
		if err := db.QueryRow("PRAGMA integrity_check").Scan(&check); err != nil {
			return err
		}
		if check != "ok" {
			return fmt.Errorf("integrity_check: %s", check)
		}
		fmt.Printf("%d ok\n", v)
		return nil
	}
	return fmt.Errorf("unknown command %q", cmd)
}

func exec(path, stmt string) error {
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(stmt)
	return err
}
