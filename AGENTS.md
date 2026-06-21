# Repository Instructions

Start with `PROJECT.md` and `.doctrine/project.json` before changing this
repository. They define the project goal, lifecycle, boundaries, public
surfaces, delivery model, and adoption gaps.

Use `SylphxAI/doctrine` for enterprise standards. Keep this repository focused
on composing and publishing custom Talos images: individual system extensions,
cluster upgrade controllers, and node-level rollout decisions stay in their
own repositories or documented consumer configuration.

For control-plane-only changes, validate with:

```bash
python3 /Users/kyle/.doctrine/scripts/project-control-plane-audit.py --local . --fail-on-drift --json
git diff --check
```

For image changes, also prove the imager build, GitHub Release artifact, GHCR
installer image, and the intended Talos install or upgrade readback.
