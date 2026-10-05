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
- A site can only deliver its own changes; it cannot pretend to be another site.
- A site's address, key and certificate can only be changed by that site itself. Other sites may only pass on
  that a member was removed, so one compromised site cannot redirect traffic meant for another.
- LAN join requests are accepted only from private addresses, two pending per address.
- Postgres passwords are passed to `pg_dump`/`psql` through the environment, never on the command line.
- Conduit does not change Postgres settings unless an admin allows it (or `CONDUIT_CONFIGURE_POSTGRES=true`).
- The dashboard (port 7420) is plain HTTP and meant for your LAN. Protect it with
  `CONDUIT_ADMIN_PASSWORD`, and put a TLS reverse proxy in front if you expose it further.
- Invite codes are single-use and expire after 24 hours. Treat them like passwords.
- LAN discovery trusts the first certificate it hears about (like SSH's first connection).
  On an untrusted LAN, use invite codes instead.

---

## นโยบายความปลอดภัย (ภาษาไทย)

ถ้าเจอช่องโหว่ กรุณาแจ้ง**แบบส่วนตัว**ที่แท็บ [Security](https://github.com/MorphEdit/conduit/security)
→ **Report a vulnerability** อย่าเปิด issue หรือ pull request สาธารณะ
