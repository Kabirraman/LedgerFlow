# How to apply these fixes

This package contains new/changed files only. Copy them into your local clone
of LedgerFlow, overwriting the existing ones, then follow the steps below.

## 1. Copy the files in

From this package's root, copy everything into your repo root, preserving
paths:

```
.gitignore              -> LedgerFlow/.gitignore
LICENSE                 -> LedgerFlow/LICENSE           (new)
Makefile                -> LedgerFlow/Makefile          (new)
docker-compose.yml      -> LedgerFlow/docker-compose.yml (new)
README.md               -> LedgerFlow/README.md
backend/Dockerfile      -> LedgerFlow/backend/Dockerfile      (new)
backend/.dockerignore   -> LedgerFlow/backend/.dockerignore   (new)
backend/go.mod          -> LedgerFlow/backend/go.mod
frontend/Dockerfile     -> LedgerFlow/frontend/Dockerfile     (new)
frontend/.dockerignore  -> LedgerFlow/frontend/.dockerignore  (new)
```

## 2. Untrack build artifacts and local tool config

Run this from your repo root:

```bash
git rm -r --cached frontend/.next frontend/.next-dev frontend/.next-verify frontend/tsconfig.tsbuildinfo
git rm -r --cached .claude 2>/dev/null || true   # only if it exists in your repo
rm -rf .claude
```

## 3. Verify locally before touching git history

```bash
cd backend
go build ./...
go test ./...
```

I changed `go 1.26.3` to `go 1.23` in go.mod since nothing in the code actually
requires a version that new (no use of `slices`, `maps`, `cmp`, or `iter`
packages) — but I could not compile-test this myself: my sandbox's network
policy blocks `proxy.golang.org`, so `go build` couldn't fetch dependencies.
**Run the two commands above yourself before pushing.** If `go build` fails
for an unrelated reason, that's pre-existing and not something this change
introduced — but confirm it either way.

```bash
cd ../frontend
npm install
npm run build
npm run lint
npm run typecheck
```

I ran all four of these myself in a clean sandbox and they pass.

## 4. Re-commit

Your repo currently has exactly one commit, so the cleanest fix is to amend
it rather than add a second commit on top of a messy one:

```bash
git add -A
git commit --amend -m "Initial commit: LedgerFlow — Autonomous Revenue Recovery Operating System

Backend: Go + Gin API with agentic detection/diagnosis/planning pipeline,
policy engine, Razorpay test-mode integration, and a full audit trail.
Frontend: Next.js operator console (dashboard, case review, approvals,
simulation lab). Includes Docker Compose setup, Makefile, and a
Getting Started guide in the README."
```

## 5. Force-push

Amending rewrites the commit hash, so a normal `git push` will be rejected.
Since you're the only contributor and there's only one commit, force-pushing
is safe here:

```bash
git push --force
```

GitHub will eventually garbage-collect the old ~80MB of blobs from the
previous commit; new clones won't see them.

## 6. About git history (the one thing I didn't "fix")

I didn't fabricate backdated commits to make it look like there was an
incremental build process — doing that would misrepresent your timeline to
judges, which isn't something I'll do even if it would look better. What I
did instead: cleaned up the single commit so it's not carrying junk, and gave
it a real, descriptive message.

If you still have time before the deadline, the honest way to get a more
natural-looking history is to make a few more **real** commits now — e.g. one
for a small doc tweak, one for a config change, one for anything else you
genuinely touch before submitting. If a judge asks about it, "I scaffolded
most of this locally and pushed it as one commit" is a perfectly normal
answer — it's not something to worry about.
