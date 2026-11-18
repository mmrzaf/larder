# Changelog

## [1.0.0] - 2026-11-18

First stable release of Larder. The production-validated npm, PyPI, apt and
Alpine caching paths and the other documented ecosystems retain their existing
interfaces and behaviors.

- Remove an unused internal HTTP handler alias.
- Align engineering documentation with the Go toolchain required by `go.mod`.
- Wait for the fake upstream and allocate distinct ports in the offline drill self-test.
- Retry the Alpine concurrent acceptance probe on transient CDN transport errors.
- Run the release image from `scratch` with the CA bundle from the builder, so the build needs no `gcr.io` runtime image.
- Forward `GO_IMAGE` and `GOPROXY` to the image build for mirror-only networks.

## [1.0.0-rc.2] - 2026-11-17

Second release candidate for 1.0.0.

- Fix npm acceptance tests for full and abbreviated metadata cache variants.
- Verify stale metadata serving when the upstream is unavailable.
- Improve npm acceptance assertions and reliability.
- Use npm acceptance as the release gate while retaining opt-in ecosystem suites.
- Update release scripts and testing documentation.


## [1.0.0-rc.1] - 2026-11-17

First release candidate for 1.0.0.

Supported ecosystems: npm, PyPI, Debian and Ubuntu apt, and Alpine apk
(validated with real clients by `scripts/acceptance.sh`); Go modules, Maven and
Gradle, crates.io, rustup and generic HTTP mirrors (covered by hermetic
tests). The `alpine` kind serves `APKINDEX.tar.gz` as mutable metadata and
`.apk` files as immutable; apk verifies signatures itself.

Reliability guarantees:

- A download that fails midway is never stored; a response that has begun and
  cannot be completed is aborted instead of ending as a clean body.
- Publication of a blob and removal of unreferenced blobs exclude each other;
  a stored blob that is missing or has the wrong size is discarded and fetched
  again rather than served.
- Storage failures are reported as such (500 or 507) and do not trip the
  upstream circuit breaker; concurrent large downloads reserve disk space and
  fall back to streaming without storing.
- The index uses `synchronous=FULL`; blobs are flushed and renamed before the
  index refers to them.
- The npm audit request is forwarded with its `Content-Encoding`; audit never
  reports a clean result that was not obtained.
- Releases are reproducible archives with `SHA256SUMS`, and versioned
  multi-architecture images.
