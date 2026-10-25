---
name: rtc-package
description: Install, wire and use the live video hub (Go, Arandu) in an application. Use when the request is to "install live", "go get github.com/hyz-is/arandu-rtc", "show the camera live in the panel", "stream a camera from the node that reads it", "a pass for a camera", "wire the hub into bootstrap", "the viewer limit", "the socket closes after 15 minutes", "the picture is grey for a second", or when a project's go.mod already requires github.com/hyz-is/arandu-rtc. Covers what the hub is and is not, the pass, the wiring on the node and in the application, the wire format, limits and what the browser needs.
license: MIT
metadata:
  audience: app
---

# Using RTC

Live video from the machine that reads the camera. The process that opens the
camera (an engine running ffmpeg) already holds its compressed stream; it muxes
a copy as fragmented MP4 into a pipe, the hub parses that once, and every browser
that watches gets the same pictures over a WebSocket. Nothing is decoded or
encoded on the node, and nothing passes through the application's own servers.

It owns no table, no route and no policy. The application decides who may watch
which camera.

## Install

```bash
go get github.com/hyz-is/arandu-rtc
```

No migration, no framework requirement. The manifest declares `network = true`.

## Two sides, one secret

**The application** (where people sign in) authorizes with its own Policy, then
writes a pass for that one camera and hands the browser the node's address:

```go
signer, err := live.NewSigner(secret, 0) // 32 bytes at least; 0 is 60 seconds
pass, expires := signer.Issue(cameraID, accountID)
// give the page: wss://node.example/live/<cameraID>?pass=<pass>
```

**The node** (where the camera is read) holds the same secret, a hub for the
camera reader to write to, and a route that checks the pass:

```go
hub := live.NewHub(live.Config{ViewersPerCamera: 4, Viewers: 32})
server, err := live.NewServer(live.ServerConfig{
	Hub: hub, Signer: signer, Origins: []string{"https://app.example"},
})
// the reader writes each camera's fragmented MP4 to hub.Open(cameraID)
// the route:
server.Serve(w, r, r.PathValue("camera"), nil) // 401 bad pass, 429 full, else a socket
```

`Hub.Open(cameraID)` returns an `io.WriteCloser`; give it to whatever reads the
camera (arandu-alpr's `VideoTap`). The reader must keep draining its pipe
whatever the hub says: `Write` never fails, on purpose.

`Attach` is the application's way to put its own text messages on the same
socket, such as the boxes drawn over the picture.

## What the browser does

It opens the socket, reads the first text message (`{"type":"video",...}`),
configures a `VideoDecoder` with `codec`, size and the base64 `description`, and
feeds each binary message to it as an `EncodedVideoChunk`. The layout of a
binary message and the other text messages are at the top of `serve.go`. This
needs `connect-src` for the node and nothing else: no `media-src`, no `blob:`.
Write the source as `wss://node.example`: `https://node.example` does not admit a
WebSocket (CSP matches `ws` to `wss` and `http` to `https`, never across), and
the browser refuses the socket with a violation that names the policy. A canvas's
stream given to a `<video>` through `srcObject` loads nothing, so the element
needs no `media-src` either.

## Limits

| field | default | what it does |
| --- | --- | --- |
| `Config.ViewersPerCamera` | 4 | the camera is read once; this bounds what the node sends |
| `Config.Viewers` | 64 | viewers on the node in all |
| `Config.Queue` | 120 | pictures a viewer may be behind before it is skipped to the next keyframe |
| `Config.GOPBytes` | 8 MiB | the group kept so a late viewer starts at once |
| `ServerConfig.MaxSession` | 15m | the socket closes with 4001; the page asks for a new pass |
| `Config.Transcoder` | none | converts a camera that is not H.264, only while somebody watches |
| `Config.MaxTranscodes` | 1 | cameras converted at once; one more is told `busy` |
| `Config.TranscodeIdle` | 12s | how long a conversion outlives its last viewer |
| `Config.EndGrace` | 15s | how long a camera that lost its stream keeps its viewers' sockets (a renewed address reconnects inside it) |
| `ServerConfig.Origins` | none | pages allowed to open the socket; the page's origin, whatever host the proxy forwards |
| `ServerConfig.MaxMessage` | 128 KiB | the largest message written; a larger picture is sent in pieces |
| `ServerConfig.PingInterval` | 15s | the longest the socket is quiet, camera or not |

## Behind a proxy

The socket is built to cross one (Cloudflare, Traefik): messages stay small, a keepalive goes out
every `PingInterval` even when the camera sends nothing, and the answers are marked `no-store`
and unbuffered. Nothing depends on the client's address: limits count viewers, and the pass is
what authorizes, so `CF-Connecting-IP` and `X-Forwarded-For` are never read. The Origin check
compares the page's `Origin` header, which a proxy passes through, and not the `Host` it rewrites.

## When something goes wrong

| symptom | cause |
| --- | --- |
| 401 on the handshake | the pass is for another camera, expired, or signed with another secret |
| 429 on the handshake | the camera or the node has all its viewers |
| the socket opens and says `waiting` | the node has no picture of that camera: it is not running there, or its reader has no tap |
| the picture starts a second late | the viewer arrived between keyframes; it starts at the last one |
| the socket closes at 4001 | `MaxSession`; open another with a new pass |
