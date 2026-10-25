# Skills

Procedures an assistant follows when working on this package.

They live in `.agents/skills/<name>/SKILL.md`, which is the path the coding
assistants read from — Cursor, Codex, Cline, Copilot, Gemini CLI, Amp, OpenCode,
Warp, Zed and the rest all look there. It is one directory rather than a file
per vendor, so a skill written once is read by whatever this package is being
written with.

Each file opens with frontmatter carrying a `name` and a `description`. The
`name` has to equal the directory name exactly, or the skill is not loaded. The
description is what a tool reads to decide whether the skill is relevant, so it
names the situation you are in rather than the topic it covers.

| skill | when it fires |
| --- | --- |
| `live-module` | changing the hub: the pass, the container reader, the viewers, the wire format, their tests |
| `rtc-release` | the gates, the manifest, a dependency, a version, a tag |
| `live-vault-notes` | writing the note, the gap or the journal entry, when this checkout sits inside the Arandu Obsidian vault |
| `rtc-package` | installing and wiring this package **into an application** |

The last one has a different audience from the other three, and that is on
purpose: it travels with the package so that an assistant working in somebody
else's project — the one running `go get` — has the wiring, the pass and the limits
in front of it instead of guessing.

Its frontmatter says so, under `metadata`, with `audience: app`. That line is
what `aru skills:sync` reads to copy the skill into an application whose
`go.mod` requires this package, and no other skill here carries it: an
application that received the release procedure would follow it.

`live-vault-notes` fires on a condition rather than on a task: it applies
only when `MOC-arandu.md`, `plans/cmd/audit-vault/` and `45-modules/` are actually
beside this checkout. Outside the vault it is inert, and it says so first, so a
package cloned somewhere else never grows a folder tree imitating one.

## Why these exist

The audience of the first three is somebody changing the package. The common
failure modes are letting the hub wait for a viewer (which stops the process that
reads the camera), decoding or encoding on the node, giving the package a route, a
table or a policy that belong to the installer, and widening what a pass opens.
The suite plays real ffmpeg output through the hub and a real WebSocket;
`go test -race ./...` exercises it.

## Adding your own

A skill in this directory is yours and travels with the repository. Keep it a
procedure rather than a description: a file that says "read the documentation"
never changes what anybody does.
