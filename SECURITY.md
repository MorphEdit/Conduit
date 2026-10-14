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
  that a member was removed, so one compromised site cannot redirect traffic meant for another.
- LAN join requests are accepted only from private addresses, two pending per address.
- Postgres passwords are passed to `pg_dump`/`psql` through the environment, never on the command line.
- Conduit does not change Postgres settings unless an admin allows it (or `CONDUIT_CONFIGURE_POSTGRES=true`).
- The dashboard (port 7420) is plain HTTP and meant for your LAN. Protect it with
  `CONDUIT_ADMIN_PASSWORD`, and put a TLS reverse proxy in front if you expose it further.
- Invite codes are single-use and expire after 24 hours. Treat them like passwords.
- LAN joins are proven both ways with the pairing code: it is shown only on the new site's dashboard (only to
  its admin when `CONDUIT_ADMIN_PASSWORD` is set) and never sent over the network itself. The new site sends a
  commitment to it with its request; the admin types the code when approving, and the approving site makes an
  invite only if it matches that request (five wrong codes close the request). The invite works only for that
  site name, and the new site accepts only an approval proven with the code. So a fake site on the LAN can
  neither take over a join nor get itself approved under a real site's name. The dashboard shows each
  request's IP address. Set an admin password on new sites too; on an untrusted LAN, invite codes are still
  the safest way in.

---

## นโยบายความปลอดภัย (ภาษาไทย)

ถ้าเจอช่องโหว่ กรุณาแจ้ง**แบบส่วนตัว**ที่แท็บ [Security](https://github.com/MorphEdit/conduit/security)
→ **Report a vulnerability** อย่าเปิด issue หรือ pull request สาธารณะ
