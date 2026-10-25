<p align="center">
  <img src=".github/logo.svg" alt="Arandu" width="180">
</p>

# Arandu RTC

Live camera video for Arandu applications, served from the machine that reads the
camera.

A server that reads cameras already holds each one's compressed stream. This
package lets that same server show it: the reader writes a copy as fragmented MP4,
the hub parses it once, and every browser that watches gets the pictures over a
WebSocket, as the camera sent them. Nothing is decoded or encoded on the node, a
camera is read once however many people watch, and the video never passes through
the application where people sign in.

That application only authorizes. It writes a short pass for one camera with its
own Policy behind it, and the browser opens the node directly.

## Install

```bash
go get github.com/hyz-is/arandu-rtc
```

## Wire it

On the application, after its Policy allowed the viewer:

```go
signer, err := live.NewSigner(secret, 0) // 32 bytes at least; a pass lasts 60 seconds
pass, expires := signer.Issue(cameraID, accountID)
// wss://node.example/live/<cameraID>?pass=<pass>
```

On the node, with the same secret:

```go
hub := live.NewHub(live.Config{ViewersPerCamera: 4, Viewers: 32})
server, err := live.NewServer(live.ServerConfig{
	Hub: hub, Signer: signer, Origins: []string{"https://app.example"},
})

// The process that reads the camera writes its fragmented MP4 here.
tap := hub.Open(cameraID)

// The route.
server.Serve(w, r, r.PathValue("camera"), nil)
```

`Serve` answers 401 for a pass that is not for this camera or is expired, and 429
when the camera or the node has all the viewers it allows, both before the
upgrade.

## What the browser gets

A text message with the decoder's configuration, then one binary message per
picture with its wall-clock time, so the application can align what it draws on
top (boxes, plates) to the picture they were found on. The browser decodes with
`VideoDecoder` and draws to a canvas: it needs `connect-src` for the node and no
`media-src`, no `blob:`. The source is `wss://node.example`, not `https://node.example`:
a `https:` source does not admit a WebSocket, and a browser refuses the socket. The layout of the messages is at the top of `serve.go`.

## What it does for a slow viewer

Nothing waits for a viewer. The writer `Hub.Open` returns never blocks and never
fails, because the process behind the pipe would stall with it. A viewer that
falls behind is emptied and skipped to the next keyframe, and the others do not
notice. A viewer that arrives between keyframes receives the group since the last
one, so the picture starts at once.

## Cameras a browser cannot decode

H.265 decodes in some browsers and not in others. With a `Config.Transcoder` the hub converts a
camera that is not H.264, and only while somebody watches: the container the reader wrote is fed
to it, its H.264 output is what viewers get, and it stops a few seconds after the last viewer.
The package runs no program itself; the transcoder is the installer's (arandu-alpr's `transcode`
runs ffmpeg on the card's encoder or the CPU). An H.264 camera is handed on untouched.

## What it does not do

No recording, no HLS, no table, no route and no policy, and no conversion of its own. Audio is not
carried.
