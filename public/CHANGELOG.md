# Changelog

## v0.1.0 — first public release

- Multi-site, multi-writer Postgres sync using logical replication
- Durable outbox, retries, idempotent delivery — keeps working while sites are offline
- Last-write-wins on commit time, tombstones for delete/update races, conflict log
- Owner-only tables (writable on one site only), per-site id ranges
- Automatic joining: invite codes, LAN discovery with approval, automatic Postgres settings,
  table-structure copy and initial snapshot, site removal
- TLS 1.3 between sites with pinned certificates; per-site credentials revoked on removal
- Live isometric dashboard
- Binaries for Linux, Windows and macOS (amd64/arm64)
