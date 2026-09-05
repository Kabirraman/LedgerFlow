# What broke, and how it was fixed

Buildathon submissions are judged partly on this, so here's an honest account
rather than a cleaned-up story. These are the problems actually hit while
preparing this repo for submission — not hypotheticals.

## 1. `go.mod` pinned an exact, very new Go toolchain

**What broke:** `go.mod` declared `go 1.26.3`. On any machine without that
exact toolchain and without unrestricted internet access to fetch it via
`GOPROXY`, `go build` and `go test` failed outright before compiling a single
file — including in a sandboxed review environment that blocks
`proxy.golang.org`.

**How it was found:** Running a plain `go build ./...` in an environment with
an older, stable Go release installed (1.22) failed immediately with a clear
`go: go.mod requires go >= 1.26.3` error, then failed a second time trying to
auto-download the toolchain because that network egress was blocked.

**The fix:** Audited the codebase for anything that actually required a
recent Go version — no use of `slices`, `maps`, `cmp`, or `iter` from the
standard library, and no other 1.24+/1.25+ language features. Lowered the
`go` directive to `1.23`, ran `go mod tidy` to let the dependency graph
re-resolve for that version, then re-ran the full test suite:

```
go build ./...
go test ./...
```

All packages compiled and every existing test passed. The lesson: pin the
lowest version the code actually needs, not the version that happens to be on
the machine that wrote it — a judge's environment is not guaranteed to match
the original dev machine.

## 2. Next.js build cache committed to git, ~80MB and 620 files

**What broke:** `.gitignore` only excluded `node_modules/` and `.env`. It
never excluded `.next/`, `.next-dev/`, or `.next-verify/` — Next.js's build
output and webpack cache directories. All three had been committed, along
with `tsconfig.tsbuildinfo`. This inflated the repository to ~688MB and the
`.git` directory alone to ~97MB, none of which reflects actual project code.

**How it was found:** `du -sh` on the repo showed an implausible total size
for a project of this scope; `git ls-files | grep .next` confirmed close to
500 tracked files that were regenerated build artifacts, not source.

**The fix:** Added the missing patterns to `.gitignore`, ran
`git rm -r --cached` on the tracked build directories, and — since the
repository's history was a single commit — amended that commit rather than
adding a second one on top of the mess, then force-pushed the cleaned-up
version.

## 3. A local AI-assistant config file (`.claude/`) had been committed

**What broke:** A `.claude/settings.local.json` file — local tool-permission
config from the AI coding assistant used during development — was tracked in
git. Not a secret, but not something that belongs in a submitted repo either.

**The fix:** Removed it from tracking and added `.claude/` to `.gitignore`
alongside the other local-only paths.

## 4. Docs promised infrastructure that didn't exist in the repo

**What broke:** The README described a Docker Compose deployment as the
recommended path, and a code comment in `main.go` referred to "the Makefile"
— but neither a `Dockerfile`, a `docker-compose.yml`, nor a `Makefile`
actually existed in the repository. Anyone following the docs literally would
have hit a dead end at the first step.

**The fix:** Added `backend/Dockerfile` (multi-stage, distroless runtime
image), `frontend/Dockerfile` (using the `output: 'standalone'` mode already
configured in `next.config.mjs`), a root `docker-compose.yml` wiring
Postgres + backend + frontend together, and a `Makefile` with the handful of
commands referenced but never defined. Then verified the frontend build and
typecheck pass cleanly end to end.

---

<!--
Add your own real engineering problems here — the ones from actual feature
work (agent pipeline, policy engine, simulation runner, etc.) will carry more
weight with a technical panel than repo-hygiene issues alone. Keep the same
shape: what broke, how you found it, what you changed, and what you'd do
differently next time.
-->
