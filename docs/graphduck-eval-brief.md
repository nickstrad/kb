# Brief (Fable): GraphDuck evaluation for kb - DuckDB pivot, dual approach, and 2-5 book-based changes

You are in the graphduck-eval worktree of the kb repo on branch eval/graphduck. kb is Nick's knowledge-store CLI (Go): it indexes knowledge entries from $KB_ROOT/data into .kb/kb.sqlite and searches them with SQLite FTS5 + sqlite-vec hybrid search. Nick shared the book "GraphDuck: Embedded Graph for AI Agents with DuckDB" by Volodymyr Pavlyshyn and asked for this evaluation.

Read first, in this order:
1. This repo's README.md and AGENTS.md/CLAUDE.md if present. The repo has no STATE.md yet - create one at the worktree root per the standing protocol (goal pointer, one-line status, done, in-progress, next steps, open decisions, resume instructions) and keep it updated.
2. The current architecture in code: internal/store (SQLite schema, sqlite-vec usage), internal/search (FTS + vector hybrid, RRF), internal/embed and internal/embed/provider (how embeddings are made and chosen), and internal/stats/duck.go - note carefully that kb ALREADY uses DuckDB in exactly one place: stats copies the search-log tables out of SQLite into an in-memory DuckDB for analytic SQL (see the D2 note in that file: "this package copies, it does not attach"; plan.md lives in /root/Raw/knowledge/plan.md if you need the decision record).
3. The book: PDF at /root/Software/kb/research/graphduck.pdf - extract text with `pdftotext /root/Software/kb/research/graphduck.pdf /root/Software/research/graphduck.txt` and read the chapters relevant to an agent knowledge store (graph modeling, vector indexes, memory design / agentic memory, hybrid RAG, triple store).
4. The book's companion repo, cloned temporarily at /root/Software/research/graphduck (chapter example code - ch09 vector indexes, ch10/ch11 memory design, ch13 hybrid RAG are the most relevant).

Write the evaluation as a new doc in this repo (use docs/graphduck-eval.md; create docs/ if needed). It must cover, with claims grounded in kb's actual code (cite files) and in what GraphDuck/DuckDB actually provides (cite chapters/examples - do not invent DuckDB features):
(a) Pivot: would moving kb's storage from SQLite to DuckDB be worth it? What specifically would kb gain (query power, vector search via VSS, graph relations between entries, analytics) and what would it lose or risk (embedded single-file simplicity, sqlite-vec behavior, write patterns, maturity of the Go driver)?
(b) Dual approach: SQLite for some things, DuckDB for others - what split would make sense given kb already does a small version of this in stats, and what does the split cost in complexity and consistency?
(c) A clear recommendation in one paragraph.
(d) 2-5 concrete changes or features for kb based on the book, each with a one-paragraph rationale grounded in a book idea and kb's code, plus a rough size (S/M/L). These are Nick's explicit ask: "suggest 2-5 changes or features based on the book."
Commit the doc and STATE.md on eval/graphduck and push the branch. Do NOT merge to main. Print: the doc path, your recommendation in 2-3 sentences, the titles of the 2-5 suggested changes, and the branch tip.
