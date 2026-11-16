# Changelog

## 1.0.0

First stable release.

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
