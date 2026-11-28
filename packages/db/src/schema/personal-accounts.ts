import { sql } from "drizzle-orm";
import {
  bigint,
  check,
  integer,
  jsonb,
  pgTable,
  smallint,
  text,
  timestamp,
} from "drizzle-orm/pg-core";

import { organization, user } from "./auth";

/**
 * A user's personal account (ADR 0020): one per user, stored as an
 * organization row so credits, keys and the gateway treat it as any other
 * account. This table is what marks that organization as personal: it is
 * closed to other people and goes when the user's account is deleted.
 */
export const personalAccount = pgTable("personal_account", {
  userId: text("user_id")
    .primaryKey()
    .references(() => user.id, { onDelete: "cascade" }),
  organizationId: text("organization_id")
    .notNull()
    .unique()
    .references(() => organization.id, { onDelete: "cascade" }),
  createdAt: timestamp("created_at").defaultNow().notNull(),
});

/** A ceiling against a typo; nobody owns this many organizations. */
export const MAX_OWNED_ORGANIZATIONS_CEILING = 1000;

/**
 * What staff set about accounts (ADR 0020, SPEC rule 6). A single row
 * (id = 1), seeded by migration 0043; each change is recorded in
 * account_settings_change.
 *
 * - maxOwnedOrganizations: how many organizations one user may own. Their
 *   personal account is not counted. `user_organization_limit` raises or
 *   lowers it for one user.
 */
export const accountSettings = pgTable(
  "account_settings",
  {
    id: smallint().primaryKey().default(1),
    maxOwnedOrganizations: integer().notNull().default(5),
    updatedAt: timestamp({ withTimezone: true }).notNull().defaultNow(),
    updatedBy: text(),
  },
  (t) => [
    check("account_settings_single_row", sql`${t.id} = 1`),
    check(
      "account_settings_max_owned_organizations_range",
      sql`${t.maxOwnedOrganizations} between 0 and ${sql.raw(String(MAX_OWNED_ORGANIZATIONS_CEILING))}`,
    ),
  ],
);

/** Append-only: a trigger rejects UPDATE and DELETE, as for key_settings_change. */
export const accountSettingsChange = pgTable("account_settings_change", {
  id: bigint({ mode: "number" }).primaryKey().generatedAlwaysAsIdentity(),
  before: jsonb().$type<Record<string, unknown>>().notNull(),
  after: jsonb().$type<Record<string, unknown>>().notNull(),
  changedBy: text().notNull(),
  changedAt: timestamp({ withTimezone: true }).notNull().defaultNow(),
});

/**
 * One user's own limit on organizations owned, set by staff in place of the
 * general one; no row: the general limit applies. Changes are in the admin
 * audit log with before and after.
 */
export const userOrganizationLimit = pgTable(
  "user_organization_limit",
  {
    userId: text()
      .primaryKey()
      .references(() => user.id, { onDelete: "cascade" }),
    maxOwnedOrganizations: integer().notNull(),
    updatedAt: timestamp({ withTimezone: true }).notNull().defaultNow(),
    updatedBy: text(),
  },
  (t) => [
    check(
      "user_organization_limit_range",
      sql`${t.maxOwnedOrganizations} between 0 and ${sql.raw(String(MAX_OWNED_ORGANIZATIONS_CEILING))}`,
    ),
  ],
);
