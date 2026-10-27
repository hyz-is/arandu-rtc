# Changelog

Everything worth knowing about a release of RTC is recorded here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
the versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

A published module version is immutable: Go serves it from the proxy forever, so
a release is corrected by another release and never by moving a tag.

## [0.2.0] - 2026-10-26

### Added

- A pass can carry a kind. `Signer.IssueKind` writes one, `Signer.VerifyKind` accepts only that kind,
  and `KindSnapshot` marks the pass of a camera's still picture. `Verify` (the live video's check)
  refuses a snapshot pass, so the pass of a page that fetches stills for many cameras does not open
  a socket. A pass without a kind is byte for byte what it was.
- `KindTile` marks the pass of a camera's small picture. `Verify` refuses it as it refuses a still's,
  and `ServerConfig.Kind` makes a Server take only the pass of one kind, so a node serves the live
  video and the tile on the same route from two hubs, each Server refusing the other's pass.
- `ServerConfig.FullSignal` renames the state a viewer is told when the camera or the node has all
  its viewers, for a hub of small pictures whose page answers "busy" by showing a still picture.
- `ServerConfig.Sent` is called with the size of every message written to a browser, for a node's
  counter of what it sends.
- `Config.Convert` decides which tracks the hub's Transcoder converts. Nil keeps the rule that
  converts what is not H.264; a hub of small pictures also converts an H.264 picture that is too
  large or too fast.

### Changed

- A pass with a kind is version 2 and one without is still version 1. A node that predates kinds
  reads only version 1, so it refuses a still's or a tile's pass and does not take it for the live
  video's. Publish this version to the nodes before the application issues passes with a kind.

## [0.1.1] - 2026-10-26

### Fixed

- A converted picture is dated when the camera's own picture reached the node, not when the
  converter finished it. The conversion starts at a keyframe, so the first converted picture is
  that keyframe's, and each later one is the camera's at the same distance from it. Boxes found on
  the camera's frame carry the camera's time; a picture dated by the converter's clock was shown
  with the boxes of a moment later, about 400 ms in the measurement on a 2560x1440 HEVC camera.

## [0.1.0] - 2026-10-25

### Added

- The live video hub: `Hub` takes a camera's fragmented MP4 once, through
  `Hub.Open`, and hands its pictures to any number of viewers, with a limit per
  camera and in all, a picture group kept for a viewer who arrives between
  keyframes, and a viewer that falls behind skipped to the next keyframe.
- `Demuxer`, the fragmented MP4 reader for H.264 and H.265, with the codec string,
  the decoder configuration record and each picture's presentation and wall time.
- `Signer`, the short HMAC pass that opens one camera's socket, and `Server`, the
  WebSocket that checks it, writes the video and carries the application's own
  messages beside it.
- Built for a proxy in front of the node: no message is larger than
  `ServerConfig.MaxMessage` (128 KiB), a larger picture is sent in pieces the page joins back
  (`ContinuationMessage`, `MoreFlag`); the socket carries a keepalive every `PingInterval`
  (15 s) whether or not the camera sends, so a proxy that closes a WebSocket with no data for
  about 100 seconds never sees an idle one; the answers carry `Cache-Control: no-store` and
  `X-Accel-Buffering: no`. The viewer limits count cameras and viewers, never addresses.
- Cameras a browser may not decode are converted on demand. A camera whose track is not H.264
  (H.265 chiefly) is handed to `Config.Transcoder` only while somebody watches, from the container
  the reader wrote (`Demuxer.Boxes` keeps it as it came), and for `Config.TranscodeIdle` (12 s)
  after the last viewer; viewers get the converted H.264 track. One conversion per camera, never
  one per viewer, at most `Config.MaxTranscodes` on the node: a viewer of one more is told
  `{"type":"signal","state":"busy"}` and the socket closes. An H.264 camera never reaches the
  transcoder. A conversion that stops by itself is started again for the viewers who remain.
- A camera whose stream ends keeps its viewers' sockets through `Config.EndGrace` (15 s): they
  are told the picture is `starting` again, never `lost`, if the reader comes back inside it, as
  it does when a camera's address is renewed. The viewer limit is also told in a message
  (`"full"`) after the upgrade, because a browser cannot read a refused handshake's status.
