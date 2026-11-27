# Staging deployment

The staging server (Ubuntu 26.04, 4 vCPU, 15 GiB RAM, 193 GB disk) runs the whole product from this repository at `/home/gateway`, branch `feat/ai-gateway`. Set up on 2026-11-27. Domain: `uat.lemru.tech` (points at the server). HTTPS is not set up yet, and the web app is not open to the internet yet.

## What runs where

| Piece         | How                                                                                           | Listens on                     |
| ------------- | --------------------------------------------------------------------------------------------- | ------------------------------ |
| Postgres 18   | Docker Compose service `db` (project `lemru`), volume `lemru_postgres_data`                   | `127.0.0.1:5432`               |
| Mailpit       | Compose service `mailpit` (catches every email; open it through an SSH tunnel)                | `127.0.0.1:1025`, `:8025` (UI) |
| Gateway       | Compose profile `gateway` (`transports/Dockerfile.local`, `apps/gateway/compose/config.json`) | `127.0.0.1:8080`               |
| Web (Next.js) | Production build, systemd unit `lemru-web.service` (`next start -H localhost -p 3000`)        | `localhost:3000`               |
| Internal jobs | systemd timers `lemru-job-*.timer` → `lemru-job@<name>.service` → `/usr/local/bin/lemru-job`  | –                              |

Only SSH (22) is open to the internet. `ufw` is inactive. Docker publishes its ports past `ufw` anyway, which is why every Compose port is bound to `127.0.0.1` (through `POSTGRES_PORT`, `MAILPIT_*_PORT` in `.env.local`). To see the site before HTTPS exists, open an SSH tunnel: `ssh -L 3000:localhost:3000 -L 8025:localhost:8025 root@<server>`, then browse `http://localhost:3000`.

Toolchain on the host: Docker 29.10 with Compose v5.8, Node 24.22.3 (from nodejs.org, checksum verified, in `/usr/local`), and pnpm 12.4.1. Go is not installed: the gateway image builds Go inside `golang:1.27.4`.

## Configuration and secrets

- `/home/gateway/.env.local` (mode 600, ignored by git) holds every variable for Compose, the migrations and the web app. It was generated with random secrets: Postgres, the gateway admin, the gateway encryption key, Better Auth, `CRON_SECRET` and the ledger login. **Never change `GATEWAY_ENCRYPTION_KEY`** once the gateway has stored provider keys.
- Compose does not read `.env.local` by itself. Always run Compose through `pnpm with-env docker compose ...`, or the defaults (`lemru`/`lemru`) are used.
- The gateway charges the ledger as `lemru_gateway`. This is a login role created by hand after the migrations: a member of `gateway_ledger` (migration 0016/0026 grants) and nothing else. It is set in `GATEWAY_LEDGER_DATABASE_URL`. The gateway's own database (`gateway`) still uses the Postgres superuser, as `docker-compose.yml` does.
- PostHog has no key yet: `NEXT_PUBLIC_POSTHOG_KEY="staging-disabled"` and the host points at a dead local port, so nothing leaves the server.
- Stripe: no keys are set (live or test). Top-ups are **switched off** in `/admin/payments`, because the admin refuses test mode until `STRIPE_TEST_SECRET_KEY` and `STRIPE_TEST_WEBHOOK_SECRET` exist. Never set the live `STRIPE_*` keys on staging.
- Staff account: `staff@lemru-staging.test`, role `admin`, two-factor (TOTP) on. Its password, TOTP secret and backup codes are in `/root/lemru-staging/staff.json` (mode 600, root only). It was made through the real sign-up and two-factor API; then `email_verified` and `role` were set in SQL, since nothing seeds staff. A test organization `staging-org` belongs to it.

## Restart everything

```sh
cd /home/gateway
pnpm with-env docker compose --profile gateway up -d --wait   # db, mailpit, gateway (no rebuild)
systemctl restart lemru-web
curl -s localhost:8080/health                                 # {"status":"ok",...}
curl -s -o /dev/null -w '%{http_code}\n' localhost:3000/      # 200
systemctl list-timers 'lemru-*'
```

The containers have restart policies and `lemru-web` is enabled, so a reboot brings everything back by itself.

Logs: `docker logs -f lemru-gateway-1`, `journalctl -u lemru-web -f`, and `journalctl -u 'lemru-job@*' -n 50` for the jobs.

## Deploy a new version

```sh
cd /home/gateway
git pull
pnpm install
pnpm with-env pnpm -F @workspace/db db:migrate
pnpm with-env docker compose --profile gateway up -d --build --wait     # rebuilds the gateway image
NODE_ENV=production pnpm with-env pnpm exec turbo run build --filter=web
systemctl restart lemru-web
```

Build the web app through turbo, not `pnpm --filter web build` alone. The CMS package generates `.content-collections`, which the web build imports. `NEXT_PUBLIC_*` values are baked in at build time, so rebuild after changing them (for example `URL` when HTTPS arrives).

## Internal jobs

`/usr/local/bin/lemru-job <name>` POSTs to `http://localhost:3000/api/internal/<name>`. It sends the bearer `CRON_SECRET`, read from `.env.local` and passed on stdin, so it never appears in `ps`.

| Timer                     | When            |
| ------------------------- | --------------- |
| `model-prices`            | every minute    |
| `data-policy`             | every 5 minutes |
| `low-balance`             | hourly, :00     |
| `credit-expiry`           | hourly, :07     |
| `key-lifetime`            | hourly, :12     |
| `reconciliation`          | daily 00:30 UTC |
| `model-price-suggestions` | daily 01:30 UTC |

Run one by hand with `lemru-job reconciliation`. Each job ran once by hand on 2026-11-27 and answered 200 (the reconciliation returned `status: ok`).

## After-deploy checklist (AGENTS.md), state on 2026-11-27

- **Import current prices:** run once; it imported 0. The gateway has no providers yet (`GET /api/providers` returns `[]`), so it has no models to price. **Run it again after adding the first provider keys** in `/admin/providers` (button on `/admin/model-prices`, or POST `/api/admin/model-prices/import`). It is idempotent.
- **Jobs:** scheduled, see above.
- **Stripe webhook:** waiting on test keys. When they exist, set `STRIPE_TEST_*` in `.env.local`, then `systemctl restart lemru-web`. Next, switch the mode to test and turn top-ups on in `/admin/payments`. The webhook goes to `https://uat.lemru.tech/api/billing/webhook/stripe` with `checkout.session.completed`, `charge.refunded`, `refund.updated` and `refund.failed`.
- **Browser check:** done on the production build with Playwright (headless Chromium). The sign-in used password and TOTP through the UI. Results below.

## Browser check, 2026-11-27

Status 200 with real content and no console errors: `/admin`, users, a user, customers, organizations, an organization (credits, top-up fee card, rate limit, data policy, members), pricing, payments, model prices, promo codes, new promo code, providers, key defaults, requests, audit, reconciliation. On the customer side: org home, credits (shows "Top-ups are temporarily unavailable"), activity, data policy, members, settings, billing, and the user dashboard, settings and security pages.

Broken:

1. **API keys page returns 500** (`/dashboard/<org>/keys`; the key page and the create-key form also call it). `formatExpiry` in `apps/web/src/modules/keys/key-form.tsx:78` passes `dateStyle`/`timeStyle` together with `timeZoneName`, which `Intl.DateTimeFormat` rejects (`TypeError: Invalid option : option`, reproduced in Node 24).
2. **`/admin` without a session returns 500** instead of sending the visitor to sign in (`You need to be logged in to access this feature!` thrown from the admin page).
3. **The cookie banner covers the "Sign in" button** at 1366×900 until it is dismissed.
4. **The web app loops on every page (307 to itself) unless the request's Host matches `next start -H`.** Next 16.4.6 builds its own origin from `-H`/`-p` (`next/dist/server/lib/router-utils/resolve-routes.js`, `initUrl`). The i18n proxy builds its rewrite from the Host header (`next-i18n-router`, `request.url`). When the two differ, Next fetches the rewrite as an external URL, the proxy runs again on `/en/...` and redirects back. Bound to `localhost`, it works for `localhost:3000`. **Behind a reverse proxy for `uat.lemru.tech` this breaks again** unless the proxy sends `Host: localhost:3000`, or the code is fixed (a relative rewrite, or `experimental.trustHostHeader`). Decide this before HTTPS.

## Still to do

- HTTPS for `uat.lemru.tech` (reverse proxy with automatic certificates). It needs ports 80 and 443 open to the internet: owner's approval first. Then set `URL`, `NEXT_PUBLIC_URL` and `BETTER_AUTH_URL` to `https://uat.lemru.tech`, rebuild the web app, and handle point 4 above.
- Firewall: turn on `ufw` with SSH (and later 80/443) only, once the owner says which addresses may use SSH.
- Provider keys, then the price import; Stripe test keys; a real PostHog key or none; email through a real provider instead of Mailpit before outside testers sign up.
