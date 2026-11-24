import { sql } from "drizzle-orm";
import {
  bigint,
  check,
  integer,
  pgTable,
  text,
  timestamp,
} from "drizzle-orm/pg-core";

import { organization } from "./auth";

/**
 * What staff allow an organization per minute across all its keys
 * (docs/platform/gateway-capabilities.md, step G1). This row is the record of
 * the decision; the gateway enforces it as its customer's rate limit. A null
 * column is no limit of that kind; no row is no limit at all.
 */
export const organizationRateLimit = pgTable(
  "organization_rate_limit",
  {
    organizationId: text()
      .primaryKey()
      .references(() => organization.id, { onDelete: "cascade" }),
    requestsPerMinute: integer(),
    tokensPerMinute: bigint({ mode: "number" }),
    updatedBy: text().notNull(),
    updatedAt: timestamp().notNull().defaultNow(),
  },
  (t) => [
    check(
      "organization_rate_limit_requests_positive",
      sql`${t.requestsPerMinute} is null or ${t.requestsPerMinute} > 0`,
    ),
    check(
      "organization_rate_limit_tokens_positive",
      sql`${t.tokensPerMinute} is null or ${t.tokensPerMinute} > 0`,
    ),
  ],
);

export type SelectOrganizationRateLimit =
  typeof organizationRateLimit.$inferSelect;
