<div align="center">

# Conduit

**Keep Postgres databases on several sites in sync — and keep working when the internet drops.**

Made by **[MorphEdit](https://github.com/MorphEdit)** · [ภาษาไทย](README.th.md)

</div>

---

Conduit sits next to each site's Postgres (cloud server, office, branch…). Every site keeps working on
its own database even when the link between sites is down; when the link comes back, Conduit sends the
changes that piled up and every site ends up with the same data.

Conduit is **not a database**. Your data stays in Postgres and your application talks to Postgres
exactly as before — no code changes. Conduit reads what changed and delivers it to the other sites.

## Features

- **Works offline** — each site writes locally; changes queue up and are delivered when the link returns.
- **Multi-site, every direction** — host ⇄ office ⇄ branch; any site can write.
- **Conflict handling** — last write wins by commit time, delete/update races handled with tombstones,
  every conflict recorded for review.
- **Owner-only tables** — mark tables (stock, ledger, payroll…) as writable on one site only.
- **Adding a site is almost automatic** — found on the LAN and approved with one click, or joined with a
  one-time invite code. Conduit configures Postgres, copies the table structure and the data by itself.
- **Secure between sites** — TLS 1.3 with pinned certificates, a separate key for every site,
  removing a site revokes it immediately.
- **Live dashboard** — isometric map of every site and link, queues, conflicts, invites and approvals.
- **Tiny** — one binary, ~6 MB of RAM per site.

## How it works

```
[Postgres A]                                        [Postgres B]
  │ ① your app writes                                    ▲
  ▼                                                     │ ⑤ applied in one transaction,
 WAL ─② Conduit reads the change (logical replication)   │   stamped with the origin's commit time
  ▼                                                     │
 ③ queued in the outbox ──④ HTTPS (TLS 1.3, pinned) ──► Conduit B
  ▲                                                     │
  └──────────────── ⑥ "got everything up to #120" ◄─────┘
```

Conduit never compares whole databases. It reads Postgres' own change log (WAL), so it knows exactly
what was inserted, updated or deleted, in which order and when. See [docs/how-it-works.md](docs/how-it-works.md).

## Quick start (Docker)

Try two sites on one machine:

```bash
git clone https://github.com/MorphEdit/conduit.git
cd conduit/examples
docker compose up -d
```

1. Open the first site's dashboard: <http://127.0.0.1:7420> — it founded a new cluster.
2. The second site (<http://127.0.0.1:7421>) has nothing configured. It finds the first one on the LAN
   and shows a 6-digit pairing code.
3. On the first site's dashboard click **Approve** on the join request with the same code
   (admin password: `change-me`).
4. Done — the second site copies the tables and data and starts syncing. Try it:

   ```bash
   docker compose exec db-office psql -U postgres -d app -c "INSERT INTO customers (name) VALUES ('hello')"
   docker compose exec db-branch psql -U postgres -d app -c "SELECT id, name FROM customers"
   ```

## Running on your own servers

Download the binary for your platform from [Releases](https://github.com/MorphEdit/conduit/releases)
(verify it with `SHA256SUMS`), or build the image from the [Dockerfile](Dockerfile).

**First site** — founds the cluster:

```bash
CONDUIT_DATABASE="postgres://user:pass@localhost:5432/app" \
CONDUIT_BOOTSTRAP=true \
CONDUIT_ADMIN_PASSWORD="choose-a-password" \
CONDUIT_ADVERTISE="https://this-site.example.com:7443" \
conduit
```

**Every other site** — needs only its database and a way in:

| Where is the new site? | What you do |
|---|---|
| Same LAN | Start it. Approve the join request (matching pairing code) on any running site's dashboard. |
| Anywhere else | Click **＋ Add site** on a dashboard (or run `conduit invite`) and give the code to the new site: `CONDUIT_JOIN=cdt1_…` or paste it on its dashboard. |
| Existing Postgres without the right settings | Conduit sets them itself and asks you to **restart Postgres once**. |

Everything else is automatic: site name, unique id range, table structure (if the database is empty),
the initial copy of all data, and every other site learning about the new one.

## Configuration

All settings are environment variables; a YAML file (`/etc/conduit/conduit.yaml`) is optional.

| Variable | Default | Meaning |
|---|---|---|
| `CONDUIT_DATABASE` | — (required) | This site's Postgres |
| `CONDUIT_ADMIN_PASSWORD` | — | Enables dashboard actions (invite, approve, remove) |
| `CONDUIT_BOOTSTRAP` | `false` | `true` on the first site only |
| `CONDUIT_JOIN` | — | Invite code `cdt1_…` |
| `CONDUIT_NODE_ID` | host name | Site name (a–z, 0–9, _) |
| `CONDUIT_ADVERTISE` | `https://<host>:7443` | Address other sites use to reach this one |
| `CONDUIT_LISTEN` | `:7420` | Dashboard (LAN discovery uses UDP 7420) |
| `CONDUIT_PEER_LISTEN` | `:7443` | HTTPS port for other sites |
| `CONDUIT_DISCOVERY` | `true` | Find / announce on the LAN |

More (owner-only tables, retention…) in [docs/configuration.md](docs/configuration.md).

## Requirements

- PostgreSQL **16+** (`wal_level=logical`, `track_commit_timestamp=on` — Conduit sets these for you)
- A superuser for Conduit
- Every table needs a primary key; the schema must be the same on every site
  (new, empty sites get it copied automatically — needs `pg_dump`/`psql` 16, included in the Docker image)
- Clocks in sync (NTP)

## Ports

| Port | Purpose | Open to |
|---|---|---|
| `7443/tcp` | Sites talk to each other (HTTPS) | Other sites — safe to expose to the internet |
| `7420/tcp` | Dashboard + admin actions (HTTP) | Your LAN only, or put a TLS reverse proxy in front |
| `7420/udp` | LAN discovery | Your LAN only |

## Good to know

- Business numbers your application generates itself (invoice / PO numbers, custom counters) are not
  coordinated by Conduit — use a per-site prefix or make that table owner-only.
- Last write wins per row: if two sites change different columns of the same row at the same time,
  the later change wins as a whole.
- Conduit syncs rows, not DDL or `TRUNCATE`: change the schema on every site yourself.
- Files your application stores outside the database are not synced.

## Support

- **Bugs & feature requests:** [open an issue](https://github.com/MorphEdit/conduit/issues/new/choose)
- **Improvements to the docs or examples:** pull requests are welcome — see [CONTRIBUTING.md](CONTRIBUTING.md)
- **Security problems:** please report privately — see [SECURITY.md](SECURITY.md)

## License

Conduit is **free to use**, including commercially, under the [Conduit Freeware License](LICENSE).
It may not be modified, reverse engineered, resold or re-branded. The source code is not published.

---

<div align="center">

**Conduit** — made with care by **[MorphEdit](https://github.com/MorphEdit)**

</div>
