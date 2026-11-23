import { sql } from "drizzle-orm";
import {
  bigint,
  check,
  index,
  pgTable,
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

export const API_KEY_LIMIT_RESETS = ["daily", "weekly", "monthly"] as const;

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
    /** Spending cap in micro-USD, separate from the prepaid balance. Null: no cap. */
    limitMicroUsd: bigint({ mode: "number" }),
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
      sql`${t.limitMicroUsd} is null or ${t.limitMicroUsd} > 0`,
    ),
    check(
      "api_key_limit_reset_needs_limit",
      sql`${t.limitReset} is null or ${t.limitMicroUsd} is not null`,
    ),
    check(
      "api_key_limit_reset_value",
      sql`${t.limitReset} is null or ${t.limitReset} in ('daily', 'weekly', 'monthly')`,
    ),
  ],
);

export const selectApiKeySchema = createSelectSchema(apiKey);
export type SelectApiKey = z.infer<typeof selectApiKeySchema>;
