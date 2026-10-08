# Changelog

## Unreleased — security and consistency fixes

Upgrade every site: a new site joining through an older one (or the reverse) needs both on this version.

- Security: a member site can no longer write outside the synced schemas on other sites (Conduit's own
  tables, `pg_catalog`…); such changes are set aside as `schema_not_synced`
- Security: LAN pairing is proven. The code stays on the new site and the admin types it when approving;
  the new site ignores an approval that does not prove it (a mistyped code or a fake site on the LAN),
  and the code is shown on the dashboard only to its admin. Beacons with impossible values are ignored
- Id slots for new sites are always handed out by one site (the active site with the lowest slot), so two
  sites admitting newcomers at the same time can no longer give out the same id range
- A site cannot change its own id slot through gossip
- New sites also copy delete markers (tombstones), so an old update cannot bring back a row deleted
  before they joined
- Table policies are re-published only when a site's config actually differs from the cluster's
- Replay no longer crashes if a savepoint cannot be opened

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
- Tables without a primary key get REPLICA IDENTITY FULL automatically, so installing Conduit never breaks the
  app's UPDATE/DELETE on them; rows are matched NULL-safely
- Id alignment and owner guards also work when an app uses an ordinary (non-superuser) role

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
