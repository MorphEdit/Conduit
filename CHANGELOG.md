# Changelog

## Unreleased

- New dashboard: a full-screen 3D map of every site (drag to rotate, scroll to zoom, arrow keys / WASD to
  pan) with live sync lines, packets, alarms and labels; a one-line health summary, a "needs attention" panel,
  the site list and the selected site's details float over it. Works on phones (map on top, panels below)
- three.js r170 (MIT) is bundled in the binary, so the dashboard keeps working on a LAN without internet;
  the dashboard no longer loads web fonts either
- Sample data shows only with `?demo`, never in place of a site that cannot be reached
- Security: LAN pairing is now proven on the approving site too. A new site sends a commitment to its code;
  an approval with any other code is refused before an invite is made, so a fake site asking under a real
  site's name gets nothing. The invite works only for that site name, five wrong codes close the request,
  and the dashboard shows each request's IP. Sites older than this version must be updated (or use an invite) to join over
  the LAN
- Fix: a site offline for longer than `tombstone_ttl` could bring a deleted row back on the other sites
  while it stayed deleted on that site. Delete markers are now kept until every active site has caught up
  past them: an idle site tells each peer every minute how far it is caught up. Upgrade every site; until a
  site sends this, the others keep their delete markers
- Fix: a row the receiving table rejects (a CHECK or exclusion constraint only that site has, a shorter
  varchar, a smaller numeric, a bad date…) no longer stops every later change from the same site. It is set
  aside as `schema_mismatch` like a missing column, and **Replay** applies it once the table is fixed
- Fix: two changes to one row committed at exactly the same instant on two sites could leave each site with
  the other's version. Ties are now broken by site id (the higher id wins) for rows, delete markers and
  rows copied to a new site, so every site keeps the same version
- Security: ten wrong admin passwords from one address lock it out for 15 minutes. A site without an admin
  password shows its pairing code only on that machine itself (and in its log), no longer to anyone on the
  LAN. Conduit warns at start when the plain-HTTP dashboard listens on every interface
- Security: a member learns a new site only from the site that hands out id slots or from the new site
  itself, so one member can no longer invent members with credentials of its own; the cluster's id step
  can no longer be changed through gossip
- The dashboard warns when another site's clock is 2 seconds or more off (conflicts are decided by commit
  time, so a clock that runs ahead wins them all)
- Docs: every site must reach every other site directly (full mesh); a site never relays another's changes

## v0.3.0 — security and consistency fixes

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
- golang.org/x/text 0.41.0 (fixes GO-2026-6629 and GO-2026-5970, reachable when connecting to Postgres)

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
