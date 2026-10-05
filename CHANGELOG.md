# Changelog

## v0.2.0 — hardening from stress tests

- Large transactions are captured in parts and applied atomically on the receiver from an on-disk spool:
  200,000-row transactions sync without running out of memory (was: crash loop at 50,000 rows)
- Faster apply: one round trip per chunk instead of four per row (~2x on big transactions)
- Go respects the container memory limit; smaller send batches
- Dashboard warns when Postgres keeps more than 1 GB of WAL for Conduit; `conduit cleanup` command
- Postgres settings are changed only after an admin allows it (or `CONDUIT_CONFIGURE_POSTGRES=true`)
- Passwords no longer appear in the process list while copying table structure
- New tables get per-site id ranges immediately (event trigger), no restart needed
- A removed site stops capturing, drops its replication slot and clears its queue
- LAN join requests: private addresses only, two pending per address
- Gossip trust rules: only a site itself can change its address, key or certificate; sites repair their own record
- A table that differs on one site no longer blocks the queue: changes are set aside and can be replayed
- Module path is now github.com/MorphEdit/conduit; image exposes 7420/tcp, 7420/udp and 7443/tcp
- More unit tests; `scripts/stress.py` measures throughput, latency and every failure case

## v0.1.0 — first public release

- Multi-site, multi-writer Postgres sync using logical replication
- Durable outbox, retries, idempotent delivery — keeps working while sites are offline
- Last-write-wins on commit time, tombstones for delete/update races, conflict log
- Owner-only tables (writable on one site only), per-site id ranges
- Automatic joining: invite codes, LAN discovery with approval, automatic Postgres settings,
  table-structure copy and initial snapshot, site removal
- TLS 1.3 between sites with pinned certificates; per-site credentials revoked on removal
- Live isometric dashboard
- Source-available under the PolyForm Strict License 1.0.0; Docker image built from source
