# How to apply


## Apply

```bash
cd LedgerFlow
# copy ARCHITECTURE.md and POSTMORTEM.md in from this package
git add ARCHITECTURE.md POSTMORTEM.md README.md
git commit --amend --no-edit    # or a new commit, if you've already pushed
                                  # follow-up commits since the last amend
git push --force                 # only if you amended
```

## Before you submit

POSTMORTEM.md currently has four real issues from the repo-cleanup pass we
did together. There's a placeholder at the bottom for your own engineering
problems — anything from building the agent pipeline, policy engine, or
simulation runner. Those will land better with a technical panel than
repo-hygiene fixes alone, since they show judgment on the actual product
logic rather than tooling. Fill that in before recording your pitch video.
