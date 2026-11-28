# ADR 0020: Personal accounts beside organizations

Status: **accepted** (owner, 2026-11-28). All five phases are approved (answers below). All five phases are on staging.

## Context

SPEC decision D2 was open: (a) an organization always, or (b) OpenRouter's model. The owner chose **(b)** on 2026-11-28: a personal account with its own wallet, organizations with separate wallets, a context switcher that the whole app follows, and moving credit from personal to an organization (ORG-01, ORG-08, BILL-12, ONB-03, ONB-04, ONB-14).

What the code does today:

- Credits, credit lots, holds, keys, the gateway customer, the data policy, the rate limit, promo redemptions and the top-up fee override are all keyed by `organization.id` (`packages/db/src/schema/{credits,keys,limits,providers,promo-codes,pricing}.ts`). The credits plugin in the gateway (Go) reads the same key.
- A user must create an organization before anything works. `/dashboard` is only an organization picker; the app lives under `/dashboard/[organization]`.
- A user may create any number of organizations (the template's behaviour; nothing limits it).
- Two-factor sign-in exists for every user (Better Auth `twoFactor`), and is required only of staff.
- Ledger entry kinds: `top_up`, `usage`, `refund`, `adjustment`, `promo`, `expiry`, `promo_clawback`. Every grant is a credit lot with an expiry and a refund deadline (ADR 0019).

## Decision

1. **A personal account is stored as an organization row that is marked personal.** A new table `personal_account (user_id primary key, organization_id unique)` names it; one per user, created when the user is created, and for every existing user by the migration. Nothing else changes key: the ledger, lots, holds, keys, the gateway customer, the data policy and the credits plugin work for a personal account exactly as for an organization, with no change to the Go code. The customer never sees it as an organization: it is shown as "Personal".
2. **A personal account is closed to other people.** No invitations, no members, no roles, no rename, no leave, no delete on its own: it goes when the user's account is deleted. The members page and the organization settings are not shown in the personal context.
3. **The context switcher lists Personal first, then each organization.** `/dashboard` opens the personal context instead of the picker. The context stays in the URL.
4. **Moving credit, personal to organization only.** Two new entry kinds, `transfer_out` (a named lot debit in the personal account) and `transfer_in` (a grant in the organization), written in one transaction under one idempotency key. Lots are taken earliest expiry first; each receiving lot keeps its source lot's expiry. Reconciliation gains an invariant: transfers out and in sum to zero.
5. **Organizations a user may own are limited by a staff setting**, audited like the other settings (SPEC rule 6).
6. **Onboarding** (ONB-03) asks "individual or organization" once after the first sign-in. Individual lands in Personal; organization creates one and lands there. The personal account exists either way.

## Phases

Each phase is one vertical slice, green on its own, with a migration that has a down script.

| #   | Phase                                                                                                                                                 | Done when                                                                                                                                                                                                 | Blocked by |
| --- | ----------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------- |
| 1   | Personal account exists: table, migration with backfill, creation at sign-up, the closed-to-others rules in the API, shown as Personal in admin lists | A new user has a personal account with a gateway customer and the default data policy; inviting into it, renaming it and deleting it are refused, each with a test                                        | answers    |
| 2   | Context switcher and routing: Personal first, `/dashboard` opens it, members and organization settings hidden there                                   | Keys, credits, activity and data policy work in the personal context on the production build in a browser                                                                                                 | 1          |
| 3   | Organization limit: staff setting, enforced at creation                                                                                               | Creating one past the limit is refused with a clear message; the change is in the audit log                                                                                                               | 1          |
| 4   | Credit transfer: kinds, lot rules, 2FA gate, the form on the personal credits page, reconciliation invariant, audit                                   | Proven through the gateway binary: credit moved is spendable by an organization key and no longer by a personal key; a repeated request moves nothing twice; the invariant fails when one side is removed | 1, 2       |
| 5   | Onboarding wizard (ONB-03, ONB-04)                                                                                                                    | Shown once; both branches end on a working home                                                                                                                                                           | 2          |

Highest risk is phase 4 (money): it runs the credits plugin tests with the binary and the race detector, as AGENTS.md requires.

## Checked against OpenRouter (2026-11-28)

Sources: `docs/platform/research/openrouter/`, and OpenRouter's organization-management and FAQ pages read on 2026-11-28.

| #   | Question                                      | OpenRouter                                                                                                       | Status                                                   |
| --- | --------------------------------------------- | ---------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------- |
| 1   | Personal to organization only                 | The only transfer documented or shown is personal to organization; nothing moves credit back                     | same, approved                                           |
| 2   | Any member may fund the organization          | The transfer is done from the personal side with no role named; buying credit for the organization is admin only | same, approved                                           |
| 3   | Bought credit only                            | Says "eligible credits" without defining them                                                                    | **open**: our own rule                                   |
| 4   | No refund after a move, expiry kept           | Not stated                                                                                                       | **open**: our own rule                                   |
| 5   | Two-factor on the account                     | Required. A fresh code at each transfer is not stated                                                            | first half same; the fresh code is **open**              |
| 6   | Limit on organizations a user owns            | No limit stated; a user may be in several                                                                        | **open**: OpenRouter has none, the limit is our addition |
| 7   | Onboarding wizard, individual or organization | Has it                                                                                                           | same, approved                                           |

OpenRouter also has three rules this ADR did not propose: a new member must wait before the organization can receive their transfer (tenure), an organization that just received a transfer has a cooldown before the next, and an organization billed by invoice cannot receive one. The owner decides whether we copy the first two (the third does not apply: we have no invoiced billing).

## Owner's answers (2026-11-28)

- Questions 3 and 4: yes. Bought credit only; moved credit is not refundable and keeps its expiry.
- Question 5: yes, a fresh two-factor code at every move.
- Question 6: a limit of 5 organizations owned per user, for everyone, as a staff setting; staff can raise it for a chosen customer (a per-user override, audited).
- Tenure and cooldown: yes, copy both from OpenRouter, each a staff setting.

All five phases are approved.

## Progress

| Phase                          | Status                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| ------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1 Personal account exists      | done 2026-11-28, on staging (migration 0042 run)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| 2 Context switcher and routing | done 2026-11-28, on staging; checked in a browser on the production build with a new sign-up: `/dashboard` opens Personal, keys, credits, activity and data policy load, members and settings lead back to home                                                                                                                                                                                                                                                                                                                                                 |
| 3 Organization limit           | done 2026-11-28, on staging (migration 0043): 5 for everyone in `account_settings`, one user's own limit in `user_organization_limit`, both set in the admin and audited. Checked in a browser as a customer: the sixth organization is refused with the reason. The two admin screens are not checked in a browser yet (staff sign-in needs the owner's second factor)                                                                                                                                                                                         |
| 4 Credit transfer              | done 2026-11-28, on staging (migration 0044). `modules/credits/transfer.ts`; 20 tests on real Postgres (lots, expiry kept, bought credit only, held and refunding credit left, idempotency, two at once, both waits, the attempt limit, the reconciliation check); the API suite with the gateway binary: 932 pass, 0 skipped. Checked in a browser: the new-member wait, a wrong code, a move of SAR 5, the organization's side, the wait between transfers, the two staff settings. The Go credits plugin was not changed and its own race tests were not run |
| 5 Onboarding wizard            | done 2026-11-28, on staging (migration 0045). `/onboarding`, `modules/onboarding`: welcome with individual or organization, the organization's name and first invitations, a starter key shown once and a first request in cURL, TypeScript and Python. A new user is sent to it from any dashboard page; existing users and anyone who accepts an invitation are not. Checked in a browser on both paths. Open: the address in the snippet does not serve chat completions from outside yet (the owner has not decided on opening the public API route)        |

## How the transfer is built

- The gateway is not involved: its credits plugin reads the cached balance and writes usage entries only (`apps/gateway/plugins/credits/store.go`), so a transfer is control-plane work on the ledger, as refunds and expiry are.
- `credit_transfer` is the request (append-only, found again by the customer's idempotency key). For each lot the credit leaves, earliest expiry first: a `transfer_out` entry naming the lot in the personal account and a `transfer_in` entry in the organization, both with the transfer's id as reference, in one transaction under both accounts' locks (taken in id order).
- The organization's lot has source `transfer`, the expiry of the lot the credit left (copied in Postgres) and no refund window. It is spent as bought credit is.
- What can move: what is left of the personal account's open top-up lots, less anything held by requests in flight or by a pending refund. Promo credit and staff adjustments stay.
- Gates, in the route: two-factor on, at most five attempts a minute per user (counted in `rate_limit`), then a fresh TOTP code checked by Better Auth. In the ledger: the destination is an organization the user belongs to, the member is past `transfer_member_tenure_hours` (24 to start) and the organization past `transfer_cooldown_hours` since its last transfer (1 to start). OpenRouter does not publish its two values; ours are staff settings.
- Reconciliation checks that every transfer took out exactly what it put in, which is the amount it records.
- Known and accepted: if staff refund a top-up in the payment provider's dashboard after part of it was moved, the refund entry takes what is left of the lot and the rest becomes debt in the personal account, as it does for credit already spent.
- Deleting an account now warns with the credit that would be lost.

## Assumptions

- Existing organizations stay organizations. Every existing user gets an empty personal account.
- An invited user gets a personal account too.
- A personal account counts as its own account for a promo code's "one use" rule and for a top-up fee override.
- Deleting a user account removes the personal account; credit left in it is lost, after a warning that names the amount.
- Workspaces (D1) stay as decided: none for now.

## Never in this work

- Organization to personal transfers, or between two organizations.
- A wallet keyed by user in the ledger or in the gateway plugin.
- Changes to prices, fees, expiry or refund rules.
