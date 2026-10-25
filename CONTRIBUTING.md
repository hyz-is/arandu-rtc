# Contributing

Thank you for considering a contribution.

## Before you open a pull request

Run the four gates. They are what CI runs, and a pull request that fails one of
them fails for everybody who pulls after it.

```bash
gofmt -l $(find . -name '*.go' -not -path '*/testdata/*' -not -name '*.kyse.go')
go build ./...
go vet ./...
go test -race ./...
```

The two filters on `gofmt` are not preference. `gofmt` is the only tool in the
chain that ignores build tags, so it reads files the compiler deliberately
excludes; `testdata/` holds fixtures that are invalid on purpose.

## What a change has to keep true

Five properties are the reason this package is what it is. A change that breaks
one of them will not be merged, whatever else it improves.

1. **Nothing waits for a viewer.** The writer `Hub.Open` returns never blocks and
   never fails: the process behind the pipe would stall with it.
2. **A socket opens only with a pass for that camera**, checked before the
   upgrade.
3. **A viewer starts on a keyframe**, and one that fell behind is skipped to the
   next, never handed pictures that refer to ones it did not get.
4. **The module needs no framework** and has one dependency.
5. **`arandu.mod.toml` matches the code.** Adding a file write, a process or a
   table means declaring it there in the same commit.

The hub stays a relay of compressed pictures. Decoding, encoding, recording, a
table, a route or a policy belong to the application that installs it.

## Style

Everything in the source is in English: identifiers, doc comments, internal
comments, error messages, log messages, and the names and messages of tests.
`pkg.go.dev` publishes the doc comments, and its readers are users of this
package.

Every exported symbol carries a doc comment saying what it does. The comment
documents the symbol and nothing else: no dates, no issue numbers, no
references to other repositories. Why a signature is what it is belongs there
when it is a fact about the code; anything else belongs in the pull request.

## Commits

One commit per logical change. Formatting is not mixed with behaviour, because
a diff that does both is a diff nobody reviews.

## Reporting a vulnerability

Do not open an issue. See [SECURITY.md](SECURITY.md).

## Licence

By contributing you agree that your contribution is licensed under the MIT
licence, the same as the rest of github.com/hyz-is/arandu-rtc.
