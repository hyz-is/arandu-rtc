# Working on Live

This is an Arandu package: a live video hub. The process that reads a camera
writes a copy of its compressed stream, as fragmented MP4, to the hub; browsers
that hold a short pass from the application watch it over a WebSocket. It has no
table, no route, no policy and no service; the installer decides who may be given
a pass.
It is a Go module somebody `go get`s and registers by hand in their own
`bootstrap/app.go`, which is the whole difference from working in an
application. There is no service provider, no container and no discovery — if a
line of wiring is not written in the installer's repository, it does not happen.

Read `.agents/skills/` before writing code. Each skill is a procedure, and the
one you need is named by the situation you are in.

## The gates

Nothing is finished until all four exit zero.

```sh
export GOWORK=off
gofmt -l $(find . -name '*.go' -not -path '*/testdata/*' -not -name '*.kyse.go')
go build ./...
go vet ./...
go test -race ./...
```

`GOWORK=off` is not borrowed from somewhere else, and here it is not a
preference either. This checkout may sit beside a Go workspace that lists the
framework repositories and does not list this one; when it does, every command
above fails before it compiles anything:

```
pattern ./...: directory prefix . does not contain modules listed in go.work
or their selected dependencies
```

With the workspace off, the module resolves the versions in `go.mod` — which is
what CI compiles against, and what somebody's `go get` will get.

Both filters on `gofmt` are load-bearing in the toolchain even where this
repository has nothing for them to skip: `gofmt` is the only tool in the chain
that ignores build tags, and `testdata/` is where a fixture is allowed to be
invalid on purpose.

`aru doctor` is not one of the gates, and running it here costs a minute and
answers nothing: it reads applications, and this is a library. It does not read
this one after it is installed either, so whatever this package must prove about
itself it proves in its own suite.

## What this repository holds

| | measured with |
| --- | --- |
| 4 Go files, one per role, all in one package at the root | `grep -l '^package live' *.go` |
| 0 routes, 0 tables | `arandu.mod.toml` says `migrations = false`; there is no `Routes` |
| 1 direct dependency, ISC | `go list -m -f '{{if and (not .Indirect) (not .Main)}}{{.Path}} {{.Version}}{{end}}' all` |
| 4 fixtures of real ffmpeg output | `ls testdata` |

```
pass.go    Signer, Claims, ErrPass: the short HMAC pass
fmp4.go    Demuxer, Track, Sample: the container, box by box, as it arrives
hub.go     Hub, Subscription, Event, Config: one read, many viewers
serve.go   Server, ServerConfig, Attach and the wire format of the socket
```

## What does not exist here

| A model reaches for | What is here instead |
| --- | --- |
| a service provider, a container, a `Register()` that discovers things | `NewHub` and `NewServer`, called by hand in the installer's code |
| decoding or re-encoding on the node | nothing here. Pictures are forwarded as they arrived. A camera a browser cannot decode goes to `Config.Transcoder`, the installer's, only while watched |
| HLS, WebRTC, MSE, a player page | a WebSocket of coded pictures and a browser's `VideoDecoder`. A `<video>` element needs `blob:` in `media-src`, which the applications' policy does not allow |
| a table of viewers, a route to list them, a policy | the installer's. `Hub.Viewers` counts; the application renders it behind its own Policy |
| a session, a cookie, a login on the node | the pass: signed, short, for one camera, checked once at the handshake |
| another dependency | an argument, first. This module is imported into other people's builds |

## What a change has to keep true

1. **Nothing waits for a viewer.** `Hub.Open`'s writer does no I/O, takes the lock
   for map lookups and non-blocking sends only, and never fails.
2. **A socket opens only with a pass for that camera**, before the upgrade.
3. **A viewer starts on a keyframe**, and one that fell behind is skipped to the
   next one.
4. **The module needs no framework.** `tests/Unit/discipline_test.go` fails on the
   first `github.com/arandu-io` import.
5. **`arandu.mod.toml` matches the code.** It declares `network = true` and
   nothing else.
