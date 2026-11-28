# ADR 0019: Credit lots, credit expiry and self-serve refunds

- Status: Accepted
- Date: 2026-11-27
- Features: ADMIN-23, BILL-10, BILL-17 (`SPEC.md`); owner decisions of 2026-11-27 (section 2: refund within 24 hours without the fee, credit may expire 365 days after purchase, both admin settings; BILL-08 decisions: promo credit expires on its own date)
- Builds on: ADR 0001 (ledger), ADR 0003 and 0012 (top-ups, refund webhook; amended here), ADR 0009 (where settlement runs), ADR 0017 (micro-SAR)
- Migration: `0038_refunds_and_credit_expiry`; amended by `0039_promo_clawback` (down scripts in `migrations/down/`)
- Amended: 2026-11-27, owner decision: a refund takes back the unused promo bonus (section "Refunds take back the unused promo bonus")

## Context

OpenRouter's Terms §4.1–4.3 (`research/openrouter/openrouter-flows-personal-2.md`): unused credit is refundable within 24 hours of purchase from the credits page, the platform fee is not refunded, and credit may expire 365 days after purchase; promo credit expires on its stated date. Both need to know, per purchase, how much of it is still unused. Our ledger (ADR 0001) knows only the organization's balance. The gateway's credits plugin settles every request into it in one statement (`settleSQL`, `plugins/credits/store.go`) as the role `gateway_ledger`, and must not become heavier.

Gateway first: the engine has budgets that reset on a period (`plugins/governance`, `ResetExpiredBudgets`) and nothing about grants of credit, their expiry or refunds. LiteLLM likewise has budgets with a `budget_duration` and no prepaid grants. Nothing to reuse; the model is OpenRouter's terms.

## Decision

### Lots

Every grant of credit is a **lot** (`credit_lot`), written in the same transaction as its ledger entry by `applyEntryIn` (the single path every grant takes):

| Grant                                        | Lot source   | Expires                                                           | Self-serve refund                                                    |
| -------------------------------------------- | ------------ | ----------------------------------------------------------------- | -------------------------------------------------------------------- |
| Top-up (`top_up`)                            | `top_up`     | purchase time + `credit_expiry_days` in force then; never if null | until purchase time + `refund_window_hours` in force then; none if 0 |
| Promo credit (`promo`: bonus or free credit) | `promo`      | the code's own `credit_expires_at`; else as a top-up              | never (it is not a purchase)                                         |
| Positive staff adjustment                    | `adjustment` | never                                                             | never                                                                |

A positive adjustment never expires: it corrects the account (a goodwill credit, a charge given back), and expiring a correction would take back what staff decided to give. Negative adjustments are debits. The terms are stored on the lot as moments, so a later change of the settings touches only purchases made after it (ADMIN-23). A window of 0 also switches every self-serve refund off at once, as a kill switch; a refund staff make in Stripe is unaffected. Credit granted before migration 0038 became lots that never expire and offer no refund: it was bought on terms that had neither.

A lot is immutable (trigger); the expiry job's one mark, `expired_at`, may be set once.

### How much of a lot is left: the attribution rule

What is left of a lot is **not stored**. It is worked out from the ledger (`attributeLots` in `credits/lots.ts`), so the gateway's settlement stays exactly as it is. Replay the organization's entries in ledger order, `(created_at, id)`:

1. A grant opens its lot with its granted amount. If there is **debt** (spending no lot covered), the new lot pays it first.
2. A `refund`, `expiry` or `promo_clawback` entry names its lot (`credit_ledger_entry.credit_lot_id`) and takes from it, up to what is left of it. Any rest is spent as in 3.
3. Usage, a negative adjustment, and a refund that names no lot (from before 0038) are spent from the open lots in this order: the earliest `expires_at` first, never-expiring last; on the same expiry, `promo` before `adjustment` before `top_up` (free credit before bought credit); then the older lot; then the lower id. What no lot covers is debt.

Consecutive usage entries all draw the same way, so Postgres sums them per gap between other entries (a window `count(*) FILTER (WHERE kind <> 'usage')`); the replay reads the grants, refunds, adjustments and expiries plus one sum per gap.

Consequences of the rule, checked by a property test and by reconciliation: every lot's remainder is ≥ 0; the remainders add up to the balance when it is positive and to 0 when it is not; debt is the negative balance. A lot can be spent until its expiry entry is written (the job runs hourly, so up to an hour past its date), because the balance the gateway checks still holds it.

Ordering by `created_at` is the time the writing transaction started. A gateway settlement inserts its entry before it takes the account's lock, so a settlement that started before an expiry job can commit after it with an earlier `created_at`. The replay then sees the usage before the expiry; rule 2 sends the expiry's excess to the other lots, which leaves the same remainders as the true order in that case and the same total in every case.

### Expiry

`/api/internal/credit-expiry`, hourly, idempotent (`credits/expiry.ts`). For each organization with a lot past `expires_at` and not marked, in one short transaction:

1. `SELECT … FOR UPDATE` on `credit_account`. The gateway's reserve, settle and reap update that row, so none runs between the job's read and its write, and both update the balance relatively (`balance = balance + amount`): no lost update. The job takes no other lock before the account; the gateway takes its reservation row, then the account; no cycle.
2. Work out the remainders (rule above).
3. Per due lot, in spending order: nothing left: mark it; something left and no more than `balance − held`: write one `expiry` entry for it (key `expiry:<lot>`, negative, naming the lot), lower the balance in the same transaction (`applyEntryIn`), mark it; more than `balance − held`: leave it for the next run, so credit held by requests in flight is never expired from under them.

Running it twice at once: the second waits on the account lock, then finds the lots marked; the key is the backstop.

### Self-serve refund (BILL-10)

`POST /api/credits/:organizationId/lots/:lotId/refund`, permission `billing: ["create"]` (owners and admins, as for a top-up). It returns the unused part of **that** top-up's lot, down to the halala: never the fee (it is not in the lot), never a promo bonus (a lot of its own, spent first on the same expiry), never credit already spent. The unused part of the bonus that came with the top-up is removed with it (below). The confirm dialog says so and shows both amounts.

1. Transaction: lock the account; refuse if the lot is not this organization's top-up, a refund of it exists (self-serve or from Stripe's dashboard), refunds are off, the window has passed, nothing whole is left, or `balance − held` cannot cover it. Write `credit_refund` (one per lot, unique) and hold the amount, plus the bonus it will take back, as a request's hold does: a `credit_reservation` with id `refund:<request>`, 15 minutes, which the gateway's reaper frees if we never finish.
2. Outside any transaction: `BillingProviderStrategy.topUp.refund` (Stripe: `refunds.create` on the payment intent, amount in halalas, `idempotencyKey: credit-refund:<request>`, metadata `purpose=credits_refund`, request id, credit).
3. Succeeded: in one transaction free the hold (reservation row, then account: the gateway's order), write a `refund` entry for the credit naming the lot, keyed `refund:stripe:<refund id>`, take back the bonus (below), mark the request. Pending: keep the hold for 14 days until the webhook. Refused or failed: free the hold, mark the request failed (final). Stripe unreachable: the request stays pending; a retry (or a double click) sends the same key and gets the same refund.

Every refund webhook (`refund.created`, `refund.updated`, `refund.failed`, `charge.refunded`, whose refunds are listed from Stripe when not expanded) is applied per refund id. A self-serve refund's webhook carries the request id and the credit in its metadata and completes the request exactly as step 3 does, under the same key, so whichever arrives first writes the entry and the other changes nothing.

### Refunds made in Stripe's dashboard

Staff may refund any part of a payment. The ledger removes **the money refunded as credit**, matched by the payment id (the top-up's lot), up to the purchase's credit less what earlier refunds of it removed: `min(refunded, granted − already refunded)`. The fee is the last part of a payment to be refunded and removes nothing. Each refund is counted once, by its id, under the account's lock, so two refunds of one payment are counted one after the other. This replaces ADR 0012's proportional share (`floor(credit × refunded / total)`): money that leaves is credit that leaves, until the credit is gone. A staff refund may exceed what is unused; the excess is spent from the other lots or becomes a negative balance, as before.

### Refunds take back the unused promo bonus (amendment, owner 2026-11-27)

A top-up paid with a bonus code has two lots: its credit (`top_up`) and its bonus (`promo`, the entry keyed `promo:<provider>:<payment>`, BILL-08). When the top-up is refunded, self-serve or in Stripe's dashboard, the unused part of the bonus is removed in the **same transaction as the refund entry**, under the account's lock (`credits/clawback.ts`):

- **Its own kind, `promo_clawback`.** It names the bonus lot (`credit_lot_id`) and the code (`promo_code_id`), and is negative (checks `credit_ledger_entry_lot_kind` and `credit_ledger_entry_promo_code`, migration 0039). Not `refund`: a `refund` entry is money paid back to the customer, keyed by the provider's refund and matched by payment (dashboard refunds sum the `refund` entries of the top-up's lot to know how much credit is left to remove; the credits page marks a lot with a `refund` entry as refunded). A clawback pays nothing out, so mixing the two would count bonus as money refunded. For the attribution rule it is one more entry that names its lot (rule 2): it takes from the bonus lot, up to what is left of it.
- **Keyed by the refund:** `promo_clawback:<provider>:<refund id>`. One refund takes back once: it is written only when that refund's entry is written now (not when its key was already applied), and the key is checked first. A self-serve refund's request and its webhooks, and every event of a dashboard refund, land on the same refund id.
- **How much.** `due = floor_halala(bonus granted × credit refunded of the purchase so far (this refund included, at most its credit) / credit granted) − taken back by earlier refunds of it`, then `min(due, floor_halala(what is left of the bonus))`, never below 0. A full refund takes back the whole unused bonus; a partial refund its share. Rounded down to the halala, in the customer's favour, so the clawback is always whole halalas and a share under a halala takes nothing; the next refund of the same purchase catches up, so the shares of a fully refunded purchase add up to the bonus (when unspent). The product is computed in bigint.
- **Only the unused part.** Bonus already spent is never taken back: the customer keeps what they used. When the bonus expires with its top-up (the code has no credit date of its own), promo is spent first on the same expiry, so spending reaches the bonus before the top-up; with an earlier code date it is spent first anyway, with a later one after. Because the cap is what the lot still holds, the remainders stay ≥ 0 and still add up to the balance (the invariants below are unchanged), and the clawback can never push the balance below 0.
- **Holds.** A self-serve refund holds the bonus it will take back together with the credit (`refund:<request>` reservation), so requests in flight cannot spend it while the provider works, and refuses as `creditInUse` when they hold it. The amount is worked out again when the refund is written: spending can only lower it. A dashboard refund has no hold, as before.
- **Unaffected:** free credit (no top-up behind it), a top-up without a bonus, and a second top-up with a one-use code that was credited without a bonus.
- **Shown:** the credits page gives each refundable top-up `bonusClawbackMicroSar`, and the confirm dialog states it; the history labels it "Promotion taken back"; the promo report (BILL-08) shows credit clawed back, in total and per use, from the ledger, so granted − clawed back is what the code left with organizations. Fee waived is unchanged: the fee is never refunded.

### Invariants checked by reconciliation

`reconciliation/invariants.ts`, part of the daily run (a finding makes it a mismatch): balance = sum of ledger entries; held = sum of open holds; every grant has a lot; lots' remainders = max(balance, 0), read in one repeatable-read snapshot per organization. A refund's hold is not a lost charge in the reaped-holds check.

## Consequences

- The gateway is untouched: settlement is one statement as before; it never reads lots.
- Working out remainders reads all of an organization's ledger once (summed per gap). It runs when the credits page loads, for a refund, for the job (only for organizations with a due lot) and for reconciliation. For an organization with millions of entries a checkpoint (remainders as of an entry) is the next step; not needed at today's volume.
- Email notices before expiry are not built: the credits page shows a notice 30 and 7 days before credit expires.
- SPEC BILL-17 said "off until the terms say so": the migration seeds 365 days as the owner decided; until the published terms say credit expires, staff should set "never" in the admin.
