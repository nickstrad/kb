# index-check

**Purpose:** report entries missing from, or extra in, `.agent/scripts/index.md`,
`.agent/knowledge/index.md`, `.agent/leverage/index.md` and `.agent/testability/index.md`,
compared with the files and folders beside each index.

**Usage** (from the repo root):

```bash
go run ./.agent/scripts/index-check            # checks .agent/
go run ./.agent/scripts/index-check -root DIR  # checks DIR/scripts, DIR/knowledge, DIR/leverage and DIR/testability
go test ./.agent/scripts/index-check           # its own tests
```

**Inputs:** the four `index.md` files and their folder listings. An entry is any file or folder
except `index.md`, `README.md` and dotfiles. A relative Markdown link names an entry by its first
path element, so `[x](tool/README.md)`, `[x](tool/)` and `[x](tool)` all name `tool`. URLs,
absolute paths and `../` links are ignored.

**Outputs:** one line per problem on stdout. Exit 0 in sync, 1 out of sync, 2 unreadable.

**Dependencies:** Go standard library only. It is a package in the main module (no `go.mod`);
`./...` patterns skip `.agent`, so run and test it by explicit path. No `-tags fts5` needed.
