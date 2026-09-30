# How Conduit works

## Capturing changes

Postgres writes every change to its write-ahead log (WAL) before anything else. Conduit asks Postgres
for a readable stream of those changes (logical replication, `pgoutput`), so it knows exactly which rows
were inserted, updated or deleted, in which order and at what commit time. It never compares whole
databases.

Each committed transaction becomes one row in Conduit's queue (the *outbox*, in the `conduit` schema of
your database) with a sequence number. The replication slot only moves forward after the queue row is
safely stored, so nothing is lost if Conduit or the machine stops.

## Delivering changes

For every other site Conduit remembers the last sequence number that site confirmed and sends the rest,
in order, over HTTPS. The receiving site applies each transaction in one database transaction and records
the sequence number in the same transaction, so a repeated delivery is simply skipped.

If a site is unreachable, its changes wait in the queue and are retried (1 s, 2 s, 4 s … up to 30 s).

## No loops

Rows written by Conduit on the receiving side are tagged with a replication origin. Conduit's own capture
ignores tagged rows, so a change never bounces back to where it came from.

## Conflicts

Every row remembers the commit time of its last change (`track_commit_timestamp`), and rows applied from
another site keep the *original* site's commit time. All sites therefore compare the same timestamps and
reach the same answer:

| While disconnected… | Result everywhere |
|---|---|
| A and B both edit row X, B later | B's version |
| A deletes X, B edits X later | X comes back with B's version |
| B edits X, A deletes X later | X is deleted (remembered as a tombstone) |
| A and B create rows that clash on a unique column | The clashing row is skipped and logged; the queue keeps moving. Resolve it by hand. |

Every change that was not applied as sent is recorded in `conduit.conflicts` and shown on the dashboard.

## Owner-only tables

Tables marked with an owner can only be written on that site. Other sites get a guard trigger that
rejects local writes, while still receiving the owner's changes. Use this for data that must never be
edited in two places, such as stock levels or accounting.

## Joining

A new site redeems an invite (or is approved on the LAN), receives an id slot and the member list, copies
the table structure if its database is empty, and loads a consistent snapshot of every table. The
snapshot keeps each row's original commit time and records, for every site, which changes it already
contains — so nothing is missed or applied twice when normal syncing starts.

Sites exchange their member lists every 10 seconds, so all sites learn about a new or removed site
without any configuration change or restart.
