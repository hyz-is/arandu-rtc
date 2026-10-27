# Upgrade Guide

## v0.2.0

Almost nothing to change. A pass without a kind is byte for byte what it was and `Verify` still
opens the live video. What is new is that `Verify` now refuses a pass that carries the kind
`snapshot` or `tile`, so the pass of a page that fetches stills or small pictures for many
cameras cannot open the full video. `Signer.IssueKind`/`VerifyKind`, `KindSnapshot`, `KindTile`,
`ServerConfig.Kind`, `ServerConfig.FullSignal`, `ServerConfig.Sent` and `Config.Convert` are
new and optional. A node that serves a kind must be released before the application starts
issuing passes of that kind: an older node refuses them with 401.

## v0.1.1

Nothing to change. A converted picture now carries the camera's own time, so the boxes of a
plate reader line up with it; no API changed.

## v0.1.0
