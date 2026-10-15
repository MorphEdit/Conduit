# Security policy

## Reporting a vulnerability

Please report security problems **privately** so they can be fixed before they are public:

1. Go to the [Security tab](https://github.com/MorphEdit/conduit/security) of this repository.
2. Click **Report a vulnerability** and describe the problem and how to reproduce it.

Please do not open a public issue, pull request or discussion about it. You will get a reply as soon
as possible, and credit in the release notes if you would like it.

## Supported versions

Security fixes are released for the latest version. Please upgrade before reporting.

## How Conduit protects your data

- Sites talk to each other only over TLS 1.3 on port 7443. Each site has its own self-signed
  certificate that other sites pin by fingerprint (shared through invite codes and the member list).
- Each site authenticates with its own secret; other sites only store its hash. Removing a site
  revokes it everywhere as soon as the news spreads (about 10 seconds).
- A site can only deliver its own changes; it cannot pretend to be another site. Changes are applied only to
  the synced schemas: never to Conduit's own tables (`conduit`) or the system catalogs.
- A site's address, key and certificate can only be changed by that site itself. Other sites may only pass on
  that a member was removed, so one compromised site cannot redirect traffic meant for another. A new member is
  learned only from the site that hands out id slots or from that member itself, so a site cannot invent
  members with credentials of its own; the id step fixed when the cluster was founded never changes.
- Member sites trust each other with the data: any member may write every synced table on every other site
  (except owner-only tables it does not own), read a full snapshot, and change the shared table policies.
  Only admit sites you control, and remove a site you no longer trust.
- Conflicts are decided by commit time, so keep every site's clock in sync (NTP): a site whose clock runs
  ahead wins every conflict until the others catch up.
- LAN join requests are accepted only from private addresses, two pending per address.
- Postgres passwords are passed to `pg_dump`/`psql` through the environment, never on the command line.
- Conduit does not change Postgres settings unless an admin allows it (or `CONDUIT_CONFIGURE_POSTGRES=true`).
- The dashboard (port 7420) is plain HTTP and meant for your LAN. Protect it with
  `CONDUIT_ADMIN_PASSWORD`, and put a TLS reverse proxy in front if you expose it further (or set
  `CONDUIT_LISTEN=127.0.0.1:7420`). Conduit warns at start when it listens on every interface. Ten wrong
  admin passwords from one address lock that address out for 15 minutes.
- Invite codes are single-use and expire after 24 hours. Treat them like passwords.
- LAN joins are proven both ways with the pairing code: it is shown only on the new site's dashboard (only to
  its admin when `CONDUIT_ADMIN_PASSWORD` is set) and never sent over the network itself. The code is the
  password of a PAKE (CPace): the new site sends its half of the exchange with its request, the admin types the
  code when approving, and the approving site seals the invite under the key the exchange derives. Only a site
  that knows the code can open it, and nothing sent lets anyone test guesses offline: a fake site gets one guess
  per request, and the new site waits longer after each failed approval. The exchange is bound to the new
  site's name and to the approving site's certificate, and the invite works only for that site name. So a fake
  site on the LAN can neither take over a join, relay it, nor get itself approved under a real site's name. A
  mistyped code is not caught on the approving site: the new site shows a fresh code and asks again. The dashboard shows each
  request's IP address. On a site without an admin password the code is shown only to a browser on that
  machine itself and in Conduit's log. Set an admin password on new sites too; on an untrusted LAN, invite
  codes are still the safest way in.

---

## นโยบายความปลอดภัย (ภาษาไทย)

ถ้าเจอช่องโหว่ กรุณาแจ้ง**แบบส่วนตัว**ที่แท็บ [Security](https://github.com/MorphEdit/conduit/security)
→ **Report a vulnerability** อย่าเปิด issue หรือ pull request สาธารณะ
