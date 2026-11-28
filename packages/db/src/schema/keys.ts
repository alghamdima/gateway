import { sql } from "drizzle-orm";
import {
  bigint,
  check,
  index,
  integer,
  jsonb,
  pgTable,
  smallint,
  text,
  timestamp,
} from "drizzle-orm/pg-core";

import { generateId } from "@workspace/shared/utils";

import { createSelectSchema } from "../lib/zod";
import { organization } from "./auth";

import type * as z from "zod";

/**
 * Which gateway customer an organization is (docs/platform/adr/0004-key-model.md).
 * The gateway generates customer ids, so they cannot simply be organization ids.
 * The credits plugin reads this table, as gateway_ledger, to charge the right
 * organization.
 */
export const gatewayCustomer = pgTable("gateway_customer", {
  organizationId: text()
    .primaryKey()
    .references(() => organization.id, { onDelete: "cascade" }),
  customerId: text().notNull().unique(),
  createdAt: timestamp().notNull().defaultNow(),
});

/**
 * When a key's spending limit starts counting again: the UTC calendar day, the
 * ISO week (from Monday) and the calendar month (ADR 0013). The windows are
 * defined once, by the SQL functions key_limit_window_start and
 * key_limit_window_end (migration 0040), which the credits plugin and the
 * control plane both call.
 */
export const API_KEY_LIMIT_RESETS = ["daily", "weekly", "monthly"] as const;
export type ApiKeyLimitReset = (typeof API_KEY_LIMIT_RESETS)[number];

/**
 * The lifetimes a key may be created with (KEYS-03), in seconds, shortest
 * first, as OpenRouter offers them. "1y" is 365 days. No expiration is null.
 * The same values bound the default expiry and the maximum lifetime staff set
 * (ADMIN-12).
 */
export const KEY_EXPIRIES = [
  "1h",
  "1d",
  "7d",
  "30d",
  "90d",
  "180d",
  "1y",
] as const;
export type KeyExpiry = (typeof KEY_EXPIRIES)[number];
export const KEY_EXPIRY_SECONDS: Readonly<Record<KeyExpiry, number>> = {
  "1h": 3_600,
  "1d": 86_400,
  "7d": 604_800,
  "30d": 2_592_000,
  "90d": 7_776_000,
  "180d": 15_552_000,
  "1y": 31_536_000,
};

/** 4,000,000 SAR in micro-SAR: a ceiling on a key's limit against typos, not a business rule. */
export const MAX_KEY_LIMIT_MICRO_SAR = 4_000_000_000_000;
/** How many quick-pick limits the key form offers at most. */
export const MAX_KEY_LIMIT_PRESETS = 6;

const expirySeconds = sql.raw(Object.values(KEY_EXPIRY_SECONDS).join(", "));

/**
 * One dashboard key is one gateway virtual key. The key itself is shown once
 * at creation and never stored here: only its SHA-256 and a display prefix.
 */
export const apiKey = pgTable(
  "api_key",
  {
    id: text().primaryKey().$defaultFn(generateId),
    organizationId: text()
      .notNull()
      .references(() => organization.id, { onDelete: "cascade" }),
    /** The user who created it. */
    createdBy: text().notNull(),
    name: text().notNull(),
    virtualKeyId: text().notNull().unique(),
    /** The first characters of the key, to tell keys apart. */
    prefix: text().notNull(),
    /** SHA-256 hex of the key, to recognize a key someone pastes. */
    valueHash: text().notNull().unique(),
    /** Spending cap in micro-SAR, separate from the prepaid balance. Null: no cap. */
    limitMicroSar: bigint({ mode: "number" }),
    /** When the cap resets. Null with a cap: it never resets. */
    limitReset: text({ enum: API_KEY_LIMIT_RESETS }),
    expiresAt: timestamp(),
    disabledAt: timestamp(),
    createdAt: timestamp().notNull().defaultNow(),
    updatedAt: timestamp()
      .notNull()
      .defaultNow()
      .$onUpdate(() => new Date()),
  },
  (t) => [
    index("api_key_organizationId_idx").on(t.organizationId),
    check(
      "api_key_limit_positive",
      sql`${t.limitMicroSar} is null or ${t.limitMicroSar} > 0`,
    ),
    check(
      "api_key_limit_reset_needs_limit",
      sql`${t.limitReset} is null or ${t.limitMicroSar} is not null`,
    ),
    check(
      "api_key_limit_reset_value",
      sql`${t.limitReset} is null or ${t.limitReset} in ('daily', 'weekly', 'monthly')`,
    ),
    check(
      "api_key_expires_after_created",
      sql`${t.expiresAt} is null or ${t.expiresAt} > ${t.createdAt}`,
    ),
  ],
);

/**
 * What a new key starts from, and how long any key may live (ADMIN-12). A
 * single row (id = 1), seeded by migration 0040; staff change it in the admin,
 * each change recorded in key_settings_change.
 *
 * - The default limit and reset fill the create form; null is no limit.
 * - The default expiry fills the create form; null is no expiration.
 * - The maximum lifetime caps every key, counted from its creation: a new key
 *   cannot be created beyond it, and a key that would outlive a newly set
 *   maximum is cut to created_at + maximum, in the gateway and here
 *   (packages/api/src/modules/keys/lifetime.ts). Null is no maximum.
 */
export const keySettings = pgTable(
  "key_settings",
  {
    id: smallint().primaryKey().default(1),
    /** micro-SAR; null: new keys have no limit by default. */
    defaultLimitMicroSar: bigint({ mode: "number" }),
    /** Null with a limit: it never resets. */
    defaultLimitReset: text({ enum: API_KEY_LIMIT_RESETS }),
    /** One of KEY_EXPIRY_SECONDS; null: no expiration. */
    defaultExpirySeconds: integer().default(KEY_EXPIRY_SECONDS["90d"]),
    /** One of KEY_EXPIRY_SECONDS; null: no maximum. */
    maxLifetimeSeconds: integer(),
    /** The limits offered as quick picks in the key form, micro-SAR. */
    limitPresetsMicroSar: bigint({ mode: "number" })
      .array()
      .notNull()
      .default(sql`'{50000000,200000000,1000000000}'::bigint[]`),
    updatedAt: timestamp({ withTimezone: true }).notNull().defaultNow(),
    updatedBy: text(),
  },
  (t) => [
    check("key_settings_single_row", sql`${t.id} = 1`),
    check(
      "key_settings_default_limit_range",
      sql`${t.defaultLimitMicroSar} is null or ${t.defaultLimitMicroSar} between 1 and ${sql.raw(String(MAX_KEY_LIMIT_MICRO_SAR))}`,
    ),
    check(
      "key_settings_default_reset_needs_limit",
      sql`${t.defaultLimitReset} is null or (${t.defaultLimitMicroSar} is not null and ${t.defaultLimitReset} in ('daily', 'weekly', 'monthly'))`,
    ),
    check(
      "key_settings_default_expiry_value",
      sql`${t.defaultExpirySeconds} is null or ${t.defaultExpirySeconds} in (${expirySeconds})`,
    ),
    check(
      "key_settings_max_lifetime_value",
      sql`${t.maxLifetimeSeconds} is null or ${t.maxLifetimeSeconds} in (${expirySeconds})`,
    ),
    // with a maximum, the default must be an expiry within it
    check(
      "key_settings_default_within_max",
      sql`${t.maxLifetimeSeconds} is null or (${t.defaultExpirySeconds} is not null and ${t.defaultExpirySeconds} <= ${t.maxLifetimeSeconds})`,
    ),
    check(
      "key_settings_limit_presets_range",
      sql`cardinality(${t.limitPresetsMicroSar}) between 1 and ${sql.raw(String(MAX_KEY_LIMIT_PRESETS))} and array_position(${t.limitPresetsMicroSar}, null) is null and 0 < all(${t.limitPresetsMicroSar}) and ${sql.raw(String(MAX_KEY_LIMIT_MICRO_SAR))} >= all(${t.limitPresetsMicroSar})`,
    ),
  ],
);

/** Append-only: a trigger rejects UPDATE and DELETE, as for pricing_change. */
export const keySettingsChange = pgTable("key_settings_change", {
  id: bigint({ mode: "number" }).primaryKey().generatedAlwaysAsIdentity(),
  before: jsonb().$type<Record<string, unknown>>().notNull(),
  after: jsonb().$type<Record<string, unknown>>().notNull(),
  changedBy: text().notNull(),
  changedAt: timestamp({ withTimezone: true }).notNull().defaultNow(),
});

export const selectApiKeySchema = createSelectSchema(apiKey);
export type SelectApiKey = z.infer<typeof selectApiKeySchema>;
export const selectKeySettingsSchema = createSelectSchema(keySettings);
export type SelectKeySettings = z.infer<typeof selectKeySettingsSchema>;
