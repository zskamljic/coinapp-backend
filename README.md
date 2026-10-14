# coinapp-backend

Single static Go binary that keeps the coin catalog fresh and serves the share API.

Two jobs:

1. **Sync** — scrape the ECB's 22 commemorative-coin year pages, match the result
   against the catalog already in Firebase, and write back only what changed.
2. **Share API** — let a signed-in user hand a snapshot of their collection to
   someone else, without either of them ever being able to read the other's data.

## Why Go

Single core, 1 GB total for the box. The binary is **7.8 MB, statically linked,
CGO-free**, and idles at a few MB of RSS. Dependencies are two: `golang.org/x/oauth2`
for the service-account JWT, and the standard library for everything else. The full
`google-api-go-client` was deliberately avoided — the catalog is one node read with
GET and written with PUT, so a direct REST client is smaller and clearer.

Measured envelope (see `deploy/coinapp-backend.service`): `MemoryMax=192M`,
`CPUQuota=80%`, `GOMAXPROCS=1`.

## The one rule this package exists to keep

> **A coin's id is assigned once and is never changed.**

Users' counts live at `users/{uid}/owned/{coinId}`. If a coin were ever re-keyed, a
user's coins would silently move onto a different commemorative piece. The ECB
renumbers nothing and renames constantly, so a naive `index = position in the list`
id is wrong, and so is a hash of the coin's text.

What the ECB actually does, observed between July 2025 and today:

- added 2 coins *in the middle of existing 2025 entries* (Germany, Latvia)
- withdrew 1 coin entirely
- retyped a feature from `Remembrance cornflower (Bleuet de France)` to
  `Remembrance cornflower ( Bleuet de France )` — one space inside the parentheses

That last edit would re-key a content hash. So:

- **id** — 16 random hex chars, minted once, immutable. Readability is not a goal.
- **matching** — a separate concern, used only to decide *which* id a scrape maps to.
  Exact fingerprint (`country|year|feature`) first, then Jaccard similarity on the
  feature's word set within `country|year`. The punctuation-level ECB edit scores
  1.000 and is absorbed without touching the id.
- **withdrawn coins are tombstoned**, never deleted, so ids stay valid and a restored
  coin comes back with its original id.

`report.rematched` in the logs is the case worth watching: it is the only time a coin's
identity was questioned and resolved in favour of the existing id.

### Migration from the legacy array

The original Java seeder wrote `coins` as a positional array (0–501). `ParseExisting`
accepts both shapes, and for the array form **each coin keeps its legacy index as its
id**. Any counts already stored at `users/{uid}/owned/263` still point at the same
French cornflower. Those indexes are then frozen — never reused, never renumbered.

## Usage

```sh
./deploy/build.sh

# Preview. Needs no credentials at all: /coins is world-readable.
./coinapp-backend --dry-run --once

# Sync for real (needs Admin credentials).
GOOGLE_APPLICATION_CREDENTIALS=/etc/coinapp/service-account.json ./coinapp-backend --once

# Long-running: sync every SYNC_INTERVAL_HOURS and serve the share API.
./coinapp-backend
```

Flags: `--once` sync and exit · `--dry-run` scrape and report, write nothing.

Configuration is environment-only; see `.env.example`. No credential is ever read from
a file inside this repository.

## Data model

```
/coins/{id}      catalog. Backend-only write (Admin SDK bypasses rules).
/meta            {count,total,schema,syncedAt,lastSync}. Public read.
/users/{uid}/owned/{coinId}  count >= 0. Owner-only read/write, validated server-side.
/shares/{id}     unreadable and unwritable by clients.
/inbox/{uid}/{id}  shares sent to me. My read only; nobody can write.
/outbox/{uid}/{id} shares I sent. My read only; nobody can write.
```

Deploy `firebase-rules.json` to the project. Two changes from the previous rules, both
explained inline in that file: `/meta` made readable, and the root `.write` grant to a
browser account removed.

## Share API

All endpoints require `Authorization: Bearer <Firebase ID token>`. The uid always
comes from the verified token, never from the request body.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/shares/inbox` | lists collections shared with me |
| `POST` | `/api/shares/{id}/accept` | accepts one |
| `POST` | `/api/shares` | `{title, recipients:[uid]}` — shares my collection |
| `DELETE` | `/api/shares/{id}` | revokes a share I own |
| `GET` | `/healthz` | liveness |

`POST /api/shares` snapshots `users/{uid}/owned` and copies it into each recipient's
own `inbox` node. That is the mechanism behind "only the backend can tell you what
somebody else has": the sharer's counts are unreadable to the recipient, so the only
route is what the backend wrote for them.

The client derives the hint itself — for each coin in the snapshot, compare against
your own count — so no extra round trip is needed.

## Tests

```sh
go test ./...
COINAPP_LIVE=1 go test ./internal/ecb/ -run TestLiveEcb -v   # hits the real ECB
```

58 tests. The load-bearing ones are in `internal/catalog`: they replay the real
502-coin July 2025 snapshot twice to prove idempotency, and replay the three observed
ECB changes to prove nothing gets re-keyed. The ECB parser is pinned against saved
copies of the live pages (36 coins on the 2025 page, 6 on 2004).

## Layout

```
cmd/coinapp-backend     entry point: sync loop + HTTP server
internal/derive         normalisation, sort fields, fingerprint, similarity
internal/ecb            ECB scraping
internal/catalog        matching, immutable ids, legacy-array migration
internal/firebase       minimal RTDB REST client
internal/sync           orchestration
internal/shares         share API
internal/config         environment configuration
testdata/               saved ECB pages + the July 2025 snapshot + a live backup
```
