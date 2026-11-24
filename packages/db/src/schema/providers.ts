import { sql } from "drizzle-orm";
import {
  bigserial,
  boolean,
  check,
  date,
  index,
  integer,
  pgTable,
  text,
  timestamp,
} from "drizzle-orm/pg-core";

import { organization } from "./auth";

/**
 * What we know about the data terms of one provider key in the gateway
 * (docs/platform/adr/0015-data-policy.md). The gateway holds the key and its
 * secret; this row is only the terms. A key with no row, or a null column, is
 * unknown, and unknown counts as retaining and training.
 */
export const PROVIDER_REGIONS = ["sa", "eu", "us", "other"] as const;
export type ProviderRegion = (typeof PROVIDER_REGIONS)[number];

export const PROVIDER_AGREEMENTS = [
  "direct",
  "public_terms",
  "private",
] as const;
export type ProviderAgreement = (typeof PROVIDER_AGREEMENTS)[number];

export const providerEndpoint = pgTable(
  "provider_endpoint",
  {
    /** The gateway's key_id for the provider key. */
    keyId: text().primaryKey(),
    /** The gateway's provider name, kept so a row still reads after its key is gone. */
    provider: text().notNull(),
    /** Where requests are processed. */
    region: text({ enum: PROVIDER_REGIONS }).notNull(),
    /** Days the provider keeps prompts and completions. 0: none. Null: unknown. */
    retentionDays: integer(),
    /** Whether the provider may train on what we send. Null: unknown. */
    trainsOnData: boolean(),
    agreement: text({ enum: PROVIDER_AGREEMENTS }).notNull(),
    /** Where the terms above come from: a URL or a contract reference. */
    source: text().notNull(),
    /** The day someone last checked the source. */
    verifiedOn: date({ mode: "string" }).notNull(),
    updatedBy: text().notNull(),
    updatedAt: timestamp().notNull().defaultNow(),
  },
  (t) => [
    check(
      "provider_endpoint_region",
      sql`${t.region} in ('sa', 'eu', 'us', 'other')`,
    ),
    check(
      "provider_endpoint_agreement",
      sql`${t.agreement} in ('direct', 'public_terms', 'private')`,
    ),
    check(
      "provider_endpoint_retention_not_negative",
      sql`${t.retentionDays} is null or ${t.retentionDays} >= 0`,
    ),
  ],
);

export type SelectProviderEndpoint = typeof providerEndpoint.$inferSelect;

/**
 * What an organization requires of the endpoints that serve it
 * (docs/platform/adr/0015-data-policy.md). No row: everything is allowed. A
 * null list allows every region or every provider.
 *
 * The gateway enforces the policy through each key's allowlist. `version`
 * counts the changes to this row; `appliedVersion` is the version every key
 * of the organization is known to carry. They differ while a change is being
 * pushed, and after a push that failed.
 */
export const organizationDataPolicy = pgTable(
  "organization_data_policy",
  {
    organizationId: text()
      .primaryKey()
      .references(() => organization.id, { onDelete: "cascade" }),
    requireZeroRetention: boolean().notNull().default(false),
    forbidTraining: boolean().notNull().default(false),
    allowedRegions: text({ enum: PROVIDER_REGIONS }).array(),
    allowedProviders: text().array(),
    version: integer().notNull().default(1),
    appliedVersion: integer().notNull().default(0),
    updatedBy: text().notNull(),
    updatedAt: timestamp().notNull().defaultNow(),
  },
  (t) => [
    check(
      "organization_data_policy_regions",
      sql`${t.allowedRegions} is null or (cardinality(${t.allowedRegions}) > 0 and ${t.allowedRegions} <@ array['sa', 'eu', 'us', 'other'])`,
    ),
    check(
      "organization_data_policy_providers",
      sql`${t.allowedProviders} is null or cardinality(${t.allowedProviders}) > 0`,
    ),
    check(
      "organization_data_policy_versions",
      sql`${t.version} >= 1 and ${t.appliedVersion} >= 0 and ${t.appliedVersion} <= ${t.version}`,
    ),
  ],
);

export type SelectOrganizationDataPolicy =
  typeof organizationDataPolicy.$inferSelect;

/**
 * Every version of every organization's data policy, as it was set: what
 * was in force when a request was made is read from here (step D3). Append
 * only: a trigger rejects UPDATE and DELETE. No foreign key, so the record
 * outlives the organization.
 */
export const organizationDataPolicyChange = pgTable(
  "organization_data_policy_change",
  {
    id: bigserial({ mode: "number" }).primaryKey(),
    organizationId: text().notNull(),
    version: integer().notNull(),
    requireZeroRetention: boolean().notNull(),
    forbidTraining: boolean().notNull(),
    allowedRegions: text({ enum: PROVIDER_REGIONS }).array(),
    allowedProviders: text().array(),
    changedBy: text().notNull(),
    changedAt: timestamp().notNull().defaultNow(),
  },
  (t) => [
    index("organization_data_policy_change_organization_idx").on(
      t.organizationId,
      t.changedAt,
    ),
  ],
);

/**
 * Every change to a provider key's data terms, as it was made: what was
 * recorded for an endpoint when a request was served is read from here (step
 * D3). `isRemoved` marks the terms being taken away; the terms columns are
 * then null. Append only: a trigger rejects UPDATE and DELETE. No foreign
 * key, so the record outlives the key.
 */
export const providerEndpointChange = pgTable(
  "provider_endpoint_change",
  {
    id: bigserial({ mode: "number" }).primaryKey(),
    keyId: text().notNull(),
    provider: text().notNull(),
    isRemoved: boolean().notNull().default(false),
    region: text({ enum: PROVIDER_REGIONS }),
    retentionDays: integer(),
    trainsOnData: boolean(),
    agreement: text({ enum: PROVIDER_AGREEMENTS }),
    source: text(),
    verifiedOn: date({ mode: "string" }),
    changedBy: text().notNull(),
    changedAt: timestamp().notNull().defaultNow(),
  },
  (t) => [
    index("provider_endpoint_change_key_idx").on(t.keyId, t.changedAt),
    // a removal carries no terms; a change carries at least where and under what agreement
    check(
      "provider_endpoint_change_removal_has_no_terms",
      sql`case when ${t.isRemoved}
        then ${t.region} is null and ${t.retentionDays} is null and ${t.trainsOnData} is null
          and ${t.agreement} is null and ${t.source} is null and ${t.verifiedOn} is null
        else ${t.region} is not null and ${t.agreement} is not null
          and ${t.source} is not null and ${t.verifiedOn} is not null
      end`,
    ),
  ],
);
