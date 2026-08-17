# Club Directory

A searchable member directory for a club that runs entirely on your own network.
One Go binary, no database server, no CDN, no external API, no internet access
required at any point — including for the authenticator setup, which generates
its own QR codes.

## What it does

- **Public listing** — name and call sign only, for anyone who can reach the server.
- **Members** — sign in by email link, manage their own record, and tick a box for
  each detail they want other signed-in members to see.
- **Admins** — see and manage everything, set membership status, and read an
  activity log.

Statuses: Pending, Active, Former (departed or dues unpaid), Silent Key, Banned.

## Quick start

```bash
go build -o clubdir .
./clubdir -addr 0.0.0.0:8080 \
          -data /var/lib/clubdir \
          -base-url http://directory.lan:8080 \
          -bootstrap-admin you@example.com
```

Then open `/login`, enter that address, and follow the link. With no SMTP relay
configured the message is written to `data/outbox/` **and printed to the log**,
so you can copy the link straight out of the console on first run.

Because you are an admin, the app will require you to set up two-step
verification before it lets you into the roster.

## Configuration

Every flag has an environment variable equivalent.

| Flag | Env | Default | Notes |
|---|---|---|---|
| `-addr` | `CD_ADDR` | `127.0.0.1:8080` | Use `0.0.0.0:8080` to serve the LAN |
| `-data` | `CD_DATA` | `./data` | JSON store, photos, outbox, audit log |
| `-trusted-proxies` | `CD_TRUSTED_PROXIES` | — | IPs/CIDRs of reverse proxies whose `X-Forwarded-For` may be believed. **Set this if you run behind nginx or Caddy**, or rate limiting and the audit log will see the proxy instead of the member |
| `-base-url` | `CD_BASE_URL` | derived | Must match how members reach the server, or emailed links break |
| `-bootstrap-admin` | `CD_BOOTSTRAP_ADMIN` | — | Creates this admin at startup if absent. Safe to leave in place |
| `-secure-cookies` | `CD_SECURE_COOKIES` | off | Turn on when behind HTTPS |
| `-echo-mail` | `CD_ECHO_MAIL` | off | Also print outgoing mail to the log |
| `-smtp-host` | `CD_SMTP_HOST` | — | Empty means "spool to `data/outbox`" |
| `-smtp-port` | `CD_SMTP_PORT` | `25` | |
| `-smtp-user` / `-smtp-pass` | `CD_SMTP_USER` / `CD_SMTP_PASS` | — | Omit for an open internal relay |
| `-smtp-from` | `CD_SMTP_FROM` | `club-directory@localhost` | |
| `-smtp-starttls` | `CD_SMTP_STARTTLS` | off | Used only if the relay offers it |
| `-smtp-insecure` | `CD_SMTP_INSECURE` | off | For a relay with a self-signed certificate |

Club name, call sign, tagline, whether the listing is public, whether silent keys
and former members appear publicly, and whether people may sign themselves up are
all set in the app under **Settings**.

## How signing in works

1. Member enters their email address. If it is on the roster, a single-use link
   valid for 15 minutes is sent. Requesting a new link invalidates the old one.
2. If they have two-step verification on, the link takes them to a code prompt
   rather than straight in.
3. **Admins must have two-step verification.** Until an admin enrols, they hold
   the role but not the sight: they are redirected to the setup page, and on
   every other page — including the public listing and member pages — they are
   treated as having self-service access only. The role grants nothing until the
   second factor is on. Members may choose.

The authenticator secret is only stored once the member proves they can generate
a valid code from it. Ten single-use backup codes are issued at that point and
stored as SHA-256 hashes.

Enrolment QR codes are generated inside the binary, so nothing is fetched from a
Google chart API or any other outside service.

The response to a sign-in request is identical whether or not the address is on
the roster — otherwise the login form would be a way for anyone on the network to
test who is a member.

## Who can do what

Access is decided in one place (`accessOf` in `store.go`), from status, role and
enrolment together. Every route reads that decision, so a page outside the admin
middleware cannot accidentally grant more than one inside it.

| Status | May sign in | Sees |
|---|---|---|
| Pending (self sign-up) | yes | their own record only, until an admin approves |
| Active member | yes | the roster, plus what each member has shared |
| Active admin, not enrolled | yes | their own record only, and the enrolment page |
| Active admin, enrolled | yes | everything |
| Former | yes | their own record only — so they can withdraw details they once shared |
| Silent Key | **no** | — |
| Banned | **no** | — |

Self sign-up is **off by default**; turn it on in Settings if your club wants it.
Approving a pending sign-up is one click on the roster.

## The privacy model

This is the part worth being precise about, because it was the point of the brief.

|                         | Public | Signed-in member | Admin |
|-------------------------|:------:|:----------------:|:-----:|
| Name, call sign, status, silent-key date | ✓ | ✓ | ✓ |
| Email, phone, address, photo | — | only if the member ticked that box | ✓ |
| Sign-in email, admin notes, last login | — | — | ✓ |
| Banned records | — | — | ✓ |

Two rules make this hold:

- **Only the member can change their own sharing boxes.** An admin can *record* a
  phone number or address — clubs collect these on paper forms all the time — but
  cannot publish it. The record is flagged for review and the member sees a notice
  next time they sign in.
- **Changing a value withdraws the consent that covered it.** If a member agreed
  to share a phone number and an admin edits that number, the share flag for
  *that field* is cleared: they consented to share that number, not whatever
  replaces it. Fields the admin did not touch keep their existing consent.
- **Templates never see unshared data.** Handlers project each record through a
  `Card` built for one specific audience, so a field the viewer is not entitled to
  is absent from the data the template renders, not merely hidden by an `if`.

Photos are bounded (8 MB request, 6 MB of image data, 25 megapixels declared —
checked with `DecodeConfig` *before* decoding, so a small file claiming enormous
dimensions is rejected rather than allocated), then decoded, cropped square,
resized to 512px and re-encoded as JPEG. Re-encoding strips EXIF, including GPS
coordinates from a phone photo, and guarantees the stored file is a real image
rather than something with an image extension.

## Deployment behind the firewall

Bind to a LAN address and leave it there. Nothing in the app calls out; the
Content-Security-Policy also forbids the browser from loading anything
cross-origin, so a stray copy-paste cannot quietly add a CDN font.

Systemd unit:

```ini
[Unit]
Description=Club Directory
After=network.target

[Service]
User=clubdir
ExecStart=/usr/local/bin/clubdir -addr 0.0.0.0:8080 -data /var/lib/clubdir -base-url http://directory.lan:8080
Restart=on-failure
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=/var/lib/clubdir

[Install]
WantedBy=multi-user.target
```

For HTTPS, put it behind nginx or Caddy on the LAN, add `-secure-cookies`, and
set `-trusted-proxies` to the proxy's address. With real addresses and phone
numbers in it, treat HTTPS as required rather than optional — the app warns at
startup if the base URL is `https://` but secure cookies are off.

Also redact sign-in links from your proxy's access log, since the URL *is* the
credential for its 15-minute life:

```nginx
location /auth/ { access_log off; proxy_pass http://127.0.0.1:8080; }
```

**Backups** are a copy of the data directory:

```
data/
  directory.db     SQLite database: roster, settings, and the cookie signing key
  directory.db-wal   \_ WAL journal files (present while the app is running)
  directory.db-shm   /
  photos/          member photos
  outbox/          spooled mail when no relay is configured
  audit.log        append-only JSONL
```

`directory.db` holds the HMAC key that signs session cookies, so treat it as a
secret and keep the directory at mode 700. Deleting it signs everyone out and
loses the roster. Back up the whole `data/` directory, not just `directory.db`
on its own — while the app is running, uncommitted writes can live in the
`-wal` file rather than the main database file.

## Building and testing

```bash
go test ./...      # 16 tests: access matrix, projection rules, regressions
go vet ./...
```

The suite covers every `{viewer status × role × enrolment}` combination against
the projection rules, plus named regression tests for three authorisation defects
found in review: an unenrolled admin reading admin-only fields through routes
outside the admin middleware; a pending sign-up reading members' shared details;
and an admin edit republishing a field under consent given for the old value.

`go.mod` declares the minimum Go language version, not a pin — build releases
with a **current, patched Go toolchain**. The roster store depends on
[`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite), a pure-Go SQLite
driver, so the binary still has no cgo requirement and no external database
server; everything else is standard library. Add `govulncheck ./...` to your
release step; it reports reachable vulnerabilities in that dependency as well
as the standard library.

## Notes and limits

- The roster lives in a SQLite database (`directory.db`) with a single
  connection, which is the right shape for the few hundred members a club
  has. It is not sized for tens of thousands.
- Sessions are stateless signed cookies with a per-member epoch, so they survive
  a restart, and "sign out everywhere" works by bumping that epoch. Pending
  magic links are held in memory and are dropped by a restart — request a new one.
- Sign-in requests are rate limited to 5 per 15 minutes per address and per IP.
- Banning a member revokes their sessions immediately.
- Deleting a member is permanent. Marking someone Former or Silent Key is
  usually what you want, and keeps the club's history intact.
- Sign-in links are redeemed by a plain GET, so a mail system that pre-fetches
  links (Outlook Safe Links and similar) can burn one before the member clicks.
  On an internal relay this is unlikely; if you hit it, the fix is a confirmation
  page that redeems by POST.
- The sign-in response is identical for known and unknown addresses, but only a
  known address triggers mail delivery, so a determined observer could still
  distinguish them by timing.
- CSV exports prefix cells starting with `=`, `+`, `-` or `@` with an apostrophe,
  so a name like `=HYPERLINK(...)` cannot become a formula in a spreadsheet.

## Layout

```
main.go       config, routing, template loading
store.go      data model, SQLite-backed store, and the audience projection rules
auth.go       signed cookies, CSRF, magic links, rate limiting, audit log
totp.go       RFC 6238 codes and backup codes
qr.go         QR encoder (byte mode, ECC L, versions 1-9)
mail.go       SMTP with an outbox fallback
photo.go      upload validation, cropping, resizing
handlers.go   public directory, sign-in, member self-service
admin.go      roster management, settings, export
web/          templates and stylesheet, embedded into the binary
```
