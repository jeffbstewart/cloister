# Do not publish this image

This build context produces an image that **may not be redistributed**, and
`cmd/publish-lint` refuses any workflow step that pushes it.  The presence of
this file is what the linter keys on; deleting it turns the guard off, so
delete it only if the licensing position below has actually changed.

## Why

`docker/workbench-claude/Dockerfile` installs `@anthropic-ai/claude-code`.
Its `LICENSE.md` reads, in full: "© Anthropic PBC.  All rights reserved.  Use
is subject to Anthropic's Commercial Terms of Service."  All rights reserved
means no redistribution right is granted, and pushing a built image
containing the CLI to a registry others can pull **is** redistribution.

The **recipe** is a different artifact and is public, deliberately.  A
Dockerfile is not a copy: a layer that runs `npm install -g <package>`
contains no Anthropic code, it names a publicly installable package that
whoever runs the build fetches themselves under their own licence.  Anthropic
publishes its own containerization recipe — `.devcontainer/init-firewall.sh`
and all — in that same public repository.

So: this file, the Dockerfile beside it, the compose overlays, and the design
doc are all public.  The built image is not published anywhere.  It is built
on the host that runs it (`docs/JAILED_CLAUDE.md`, "Deploying M1").

## Why this is a file and not a comment

It was a comment, and the comment was wrong.  `.github/workflows/images.yml`
asserted that "GHCR packages are created private; do not promote this one" —
a default that was never verified.  A package published from Actions via
`GITHUB_TOKEN` and linked to a public repository inherits that repository's
visibility, so the first successful run published the image world-readable.
It was deleted within minutes of being noticed, and it should never have
depended on being noticed.

The lesson is the one this repository keeps relearning and had already
written down two files away: a control that has never been *observed failing
closed* should be assumed not to be a control.  A comment saying "do not
publish this" enforces nothing.  A linter that fails the build does.
