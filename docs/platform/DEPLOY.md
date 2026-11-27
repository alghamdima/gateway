# Staging deployment

The staging server (Ubuntu 26.04, 4 vCPU, 15 GiB RAM, 193 GB disk) runs the whole product from this repository at `/home/gateway`, branch `feat/ai-gateway`. Set up on 2026-11-27. Public address: **https://uat.lemru.tech** (HTTPS since 2026-11-27, owner's approval).

## What runs where

| Piece           | How                                                                                              | Listens on                         |
| --------------- | ------------------------------------------------------------------------------------------------ | ---------------------------------- |
| Postgres 18     | Docker Compose service `db` (project `lemru`), volume `lemru_postgres_data`                      | `127.0.0.1:5432`                   |
| Mailpit         | Compose service `mailpit` (catches every email; open it through an SSH tunnel)                   | `127.0.0.1:1025`, `:8025` (UI)     |
| Gateway         | Compose profile `gateway` (`transports/Dockerfile.local`, `apps/gateway/compose/config.json`)    | `127.0.0.1:8080`                   |
| Caddy 2.6 (apt) | Reverse proxy with automatic Let's Encrypt certificates, `/etc/caddy/Caddyfile`, `caddy.service` | `*:80` (redirects to 443), `*:443` |
| Web (Next.js)   | Production build, systemd unit `lemru-web.service` (`next start -H localhost -p 3000`)           | `localhost:3000`                   |
| Internal jobs   | systemd timers `lemru-job-*.timer` → `lemru-job@<name>.service` → `/usr/local/bin/lemru-job`     | –                                  |

Open to the internet: SSH (22, from anywhere, as the owner chose), HTTP (80) and HTTPS (443), all through `ufw` (active, everything else refused). Only the web app is public: Caddy proxies `uat.lemru.tech` to `localhost:3000`. The gateway, Postgres and Mailpit stay private. Docker publishes its ports past `ufw`, which is why every Compose port is bound to `127.0.0.1` (through `POSTGRES_PORT`, `MAILPIT_*_PORT` in `.env.local`). To read the emails Mailpit caught: `ssh -L 8025:localhost:8025 root@uat.lemru.tech`, then browse `http://localhost:8025`.

Keep `next start -H localhost`, never `-H 127.0.0.1`: Next turns any `127.x` host into `localhost` in the proxy's URL (`next/dist/server/web/next-url.js`) but not in its own origin (`router-utils/resolve-routes.js`, `initUrl`). The i18n proxy's rewrite then looks external, Next fetches it as a new request, the proxy runs again on `/en/...` and redirects back: every page answers 307 to itself.

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
curl -s -o /dev/null -w '%{http_code}\n' https://uat.lemru.tech/  # 200
systemctl reload caddy                                        # after editing the Caddyfile
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

Build the web app through turbo, not `pnpm --filter web build` alone. The CMS package generates `.content-collections`, which the web build imports. `NEXT_PUBLIC_*` values are baked in at build time, so rebuild after changing them (`URL`, `NEXT_PUBLIC_URL` and `BETTER_AUTH_URL` are `https://uat.lemru.tech`).

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

Found broken, fixed the same day and checked again over `https://uat.lemru.tech` (28 screens, all 200, no errors; the create-key dialog shows "Key will expire on: Feb 25, 2027, 9:45 PM UTC"):

1. **API keys page returned 500** (`/dashboard/<org>/keys`, the key page and the create-key form). `formatExpiry` (`apps/web/src/modules/keys/key-form.tsx`) passed `dateStyle`/`timeStyle` with `timeZoneName`, which `Intl.DateTimeFormat` refuses (`Invalid option : option`). It now spells the fields out.
2. **`/admin` without a session returned 500**: the redirect sat only in the Suspense'd sidebar, while the page read the admin API, which refused the visitor. The admin layout now checks the staff member before the page renders (`getStaffUser`), and `e2e/specs/admin.signed-out.spec.ts` covers it. Signed out, `/admin` and `/admin/users` now answer 307 to `/auth/login`.

Not a bug: the cookie banner covers the "Sign in" button until the visitor answers it (the e2e suite answers it with `installConsentBannerHandler`).

## Still to do

- Provider keys, then the price import; Stripe test keys; a real PostHog key or none; email through a real provider instead of Mailpit before outside testers sign up.
