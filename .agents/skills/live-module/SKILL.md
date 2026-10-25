---
name: live-module
description: Change the live video hub itself. Use when the request is to "support another codec", "change the wire format", "change what a viewer is sent", "the late viewer waits too long", "a slow viewer stalls", "change the limits", "change the pass", "the container is not read", "add a test for the hub", or when a change touches pass.go, fmp4.go, hub.go or serve.go. Covers what the package is and refuses to become, where each concern lives, the one rule about never waiting for a viewer, the wire format and how the suite proves it.
license: MIT
---

# Changing the hub

The package takes one camera's compressed video, already muxed as fragmented
MP4 by the process that reads the camera, and hands it to browsers over
WebSocket. It decodes nothing, encodes nothing and runs no program. Who may watch
which camera is the application's: it writes a pass with `Signer.Issue` after its
own Policy said yes, and the node checks it with `Signer.Verify`.

## Where things live

| file | what it holds |
| --- | --- |
| `pass.go` | `Signer`, `Claims`, `ErrPass`: the short HMAC pass |
| `fmp4.go` | `Demuxer`, `Track`, `Sample`: the container, box by box, as it arrives |
| `hub.go` | `Hub`, `Subscription`, `Event`, `Config`, `ErrFull`: one read, many viewers, the picture group kept for a late viewer |
| `serve.go` | `Server`, `ServerConfig`, `Attach` and the wire format of the socket |

## The rule: nothing waits for a viewer

`Hub.Open`'s writer is called on the goroutine that drains the camera reader's
pipe. If it blocks, the process behind the pipe blocks, and the camera stops
being read for plates. So `Write` does no I/O, takes the hub's mutex for map
lookups and non-blocking channel sends only, and returns `len(p), nil` even for a
stream it cannot read. A viewer whose channel is full is emptied and waits for
the next keyframe; it never slows the others.

A new feature that would wait on a viewer, a network write or the application is
a feature for the viewer's own goroutine in `serve.go`, not for the hub.

## The wire format

Documented at the top of `serve.go`. A change to it is a breaking change for
every page that decodes it: it goes in `CHANGELOG.md` and `UPGRADE.md` even when
no Go signature moves.

## Proving it

The fixtures in `testdata/` are real ffmpeg output at 160x120, made with the
flags the camera reader uses (`-c copy -f mp4 -movflags
empty_moov+default_base_moof+frag_keyframe -frag_duration ...`): H.264, H.264
with B-frames, H.264 in fragments of five pictures, and H.265. A behaviour
without a test that fails when it breaks is not done. Run the gates in
`rtc-release` with `GOWORK=off` before anything leaves your machine.
