# Configuration

Conduit reads an optional YAML file (`-config`, default `/etc/conduit/conduit.yaml`) and then
`CONDUIT_*` environment variables, which win. Only the database URL is required.

## Environment variables

| Variable | Default | Meaning |
|---|---|---|
| `CONDUIT_DATABASE` | — (required) | This site's Postgres, e.g. `postgres://user:pass@host:5432/app` |
| `CONDUIT_ADMIN_PASSWORD` | — | Unlocks dashboard actions. Without it the buttons are hidden. |
| `CONDUIT_BOOTSTRAP` | `false` | `true` makes this site found a new cluster. Use on the first site only. |
| `CONDUIT_JOIN` | — | Invite code (`cdt1_…`) to join an existing cluster |
| `CONDUIT_NODE_ID` | host name | Site name: lowercase letters, digits, `_` |
| `CONDUIT_ADVERTISE` | `https://<host>:7443` | URL other sites use to reach this one |
| `CONDUIT_LISTEN` | `:7420` | Dashboard address |
| `CONDUIT_PEER_LISTEN` | `:7443` | HTTPS address for other sites |
| `CONDUIT_DISCOVERY` | `true` | Announce / listen on the LAN (UDP 7420) |
| `CONDUIT_CONFIGURE_POSTGRES` | `false` | Change `wal_level` / `track_commit_timestamp` without asking. Otherwise the dashboard asks an admin first. |

## YAML

```yaml
# Table policies set on any site are shared with the whole cluster.
tables:
  stock_lots:   { owner: office }   # only "office" may write; others receive
  payroll:      { owner: office }

capture:
  schemas: [public]                 # which schemas to sync (default: public)

tombstone_ttl: 168h                 # how long deletes are remembered for conflict
                                    # resolution; longer than your longest outage
outbox_retention: 1h                # keep delivered changes a while for sites
                                    # that are joining right now
```

## Commands

| Command | What it does |
|---|---|
| `conduit` | Run the site |
| `conduit invite` | Print a one-time invite code (valid 24 h) |
| `conduit cleanup --yes` | Remove Conduit from this database (slot, publication, event trigger, `conduit` schema). Stop Conduit first. |
| `conduit version` | Print the version |

## Id ranges

Every site gets its own id slot (1, 2, 3…, up to 10). `SERIAL` / `IDENTITY` columns on site *n* then
produce ids ending in *n* (`id % 10 = n`), so two sites never create the same id, even offline.
Removed sites keep their slot so old rows never collide with new ones.
An event trigger re-aligns sequences the moment a table is created or altered, so new tables are safe
without restarting Conduit.
