# Security policy

## Supported versions

The latest minor release of Live receives security fixes. Older minor
releases do not: a published Go module version is immutable, so a fix is a new
release rather than a moved tag.

## Reporting a vulnerability

Report it privately, through GitHub's advisory form:

<https://github.com/hyz-is/arandu-rtc/security/advisories/new>

Do not open a public issue and do not describe the problem in a pull request.
A report that arrives in public is a report every reader of this repository can
act on before there is a release to upgrade to.

Include what you need to reproduce it: the version, the wiring, and the request
or call that triggers it.

Expect an acknowledgement within a few days. If the report is confirmed, the
fix, the release and the advisory are published together, and you are credited
unless you ask not to be.

## What is in scope

Anything that lets somebody watch a camera the installer did not allow, or harm
the process that reads one. In particular:

- a socket opened without a pass that `Signer.Verify` accepts for that camera;
- a pass that opens a camera other than the one it names, or works after its
  expiry, or is accepted when signed with another secret;
- a page of an origin outside `ServerConfig.Origins` that opens the socket;
- a viewer, however slow or hostile, that stalls the writer `Hub.Open` returns or
  makes the hub hold more than its limits allow;
- a stream of bytes written to the hub that panics it or exhausts its memory.

## What is not

- A vulnerability in the framework itself. Report that to the framework.
- An application that issues passes without asking its Policy first. What this
  package ships checks the pass and the limits; who may be given one is the
  application's.
