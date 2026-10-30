# Sandbox: throwaway copies of the working tree

Everything in this folder except this README and `.gitignore` is git-ignored and disposable. Any
tool that **writes source code** (mutation testing) or needs **build outputs and scratch state**
(the CLI harness's binary and temp stores) works in a sandbox here, never in the live tree. Other
agents edit the live tree at the same time.

## Protocol

1. `bash .agent/scripts/sandbox.sh new <name>` prints the new directory,
   `.agent/sandbox/<YYYYMMDD-HHMM>-<name>[-N]/`. It holds a snapshot of the current working tree:
   tracked and untracked-but-not-ignored files, uncommitted edits included. Deleted files, ignored
   files (build outputs), `.git` and `.agent/sandbox` itself are left out. Because the name has a
   timestamp and `mkdir` is atomic, concurrent agents never share a sandbox.
2. Work only inside that directory. Run `go` commands with the sandbox as the working directory.
   It has its own `go.mod`, so it is a separate module, and the main module's `./...` skips
   dot-dirs anyway. Put binaries, profiles and temp stores under `<sandbox>/_adv/`.
3. When you are done, run `bash .agent/scripts/sandbox.sh rm <name|path>`. Keep a sandbox only
   for inspection (the tools' `-keep`/`-k`), and remove it before you finish.
4. Pre-finish: `bash .agent/scripts/sandbox.sh list`. If you created any sandboxes, prune them:
   `bash .agent/scripts/sandbox.sh prune --older-than 0m` removes all of them, so do that only
   when you run alone; otherwise `rm` your own.

Rules: never point a mutating tool at the live tree; never write outside the sandbox; no network
(set `GOPROXY=off`; module downloads are already cached). `sandbox.sh rm` and `prune` refuse any
path that is not directly inside this folder.

Tools that follow this protocol: `.agent/scripts/adversarial/` (mutate, harness, coverage-gaps.sh,
flaky.sh). See `docs/ADVERSARIAL_TESTING.md`.
