import { sql } from "drizzle-orm";
import {
  bigint,
  boolean,
  check,
  index,
  jsonb,
  pgTable,
  smallint,
  text,
  timestamp,
} from "drizzle-orm/pg-core";

import { user } from "./auth";

/**
 * The public site: what staff switch on it, and the messages people send
 * through the contact page (SPEC SITE-01 to SITE-03).
 */

const at = () => timestamp({ withTimezone: true });

/**
 * What staff set about the public site (SPEC rule 6). A single row (id = 1),
 * seeded by migration 0047; each change is recorded in site_settings_change.
 *
 * - isBlogEnabled: whether the blog is served and linked. Off to start with:
 *   it has no posts yet (owner, 2026-11-28).
 * - contactEmail: where an email goes when someone writes through the contact
 *   page (ADMIN-20); null: none is sent, and staff read the messages in the
 *   admin, where every one is kept either way.
 */
export const siteSettings = pgTable(
  "site_settings",
  {
    id: smallint().primaryKey().default(1),
    isBlogEnabled: boolean().notNull().default(false),
    contactEmail: text(),
    updatedAt: at().notNull().defaultNow(),
    updatedBy: text(),
  },
  (t) => [
    check("site_settings_single_row", sql`${t.id} = 1`),
    check(
      "site_settings_contact_email_length",
      sql`${t.contactEmail} is null or char_length(${t.contactEmail}) between 3 and 254`,
    ),
  ],
);

/** Append-only: a trigger rejects UPDATE and DELETE, as for key_settings_change. */
export const siteSettingsChange = pgTable("site_settings_change", {
  id: bigint({ mode: "number" }).primaryKey().generatedAlwaysAsIdentity(),
  before: jsonb().$type<Record<string, unknown>>().notNull(),
  after: jsonb().$type<Record<string, unknown>>().notNull(),
  changedBy: text().notNull(),
  changedAt: at().notNull().defaultNow(),
});

/**
 * Why someone wrote: to buy (an Enterprise plan, a quote), to offer their
 * models as a provider, for help with their account, or anything else.
 */
export const CONTACT_TOPICS = [
  "sales",
  "provider",
  "support",
  "other",
] as const;
export type ContactTopic = (typeof CONTACT_TOPICS)[number];

/** Where staff are with a message. */
export const CONTACT_STATUSES = ["new", "in_progress", "closed"] as const;
export type ContactStatus = (typeof CONTACT_STATUSES)[number];

export const CONTACT_NAME_MAX = 100;
export const CONTACT_EMAIL_MAX = 254;
export const CONTACT_COMPANY_MAX = 200;
export const CONTACT_MESSAGE_MAX = 4000;
export const CONTACT_NOTE_MAX = 4000;

const inList = (values: readonly string[]) =>
  sql.raw(values.map((value) => `'${value}'`).join(", "));

/**
 * A message sent through the contact page. The row is the record staff work
 * from: who wrote, why, and where the conversation stands. What the sender
 * wrote is never changed; staff change the status and their own note, and
 * each change is in the admin audit log with before and after.
 */
export const contactRequest = pgTable(
  "contact_request",
  {
    id: bigint({ mode: "number" }).primaryKey().generatedAlwaysAsIdentity(),
    topic: text().$type<ContactTopic>().notNull(),
    name: text().notNull(),
    email: text().notNull(),
    company: text(),
    message: text().notNull(),
    /** The signed-in user who sent it, if any; kept when the account goes. */
    userId: text().references(() => user.id, { onDelete: "set null" }),
    status: text().$type<ContactStatus>().notNull().default("new"),
    /** Staff's own note: what was answered, what is agreed. Never shown to the sender. */
    staffNote: text(),
    updatedAt: at().notNull().defaultNow(),
    updatedBy: text(),
    createdAt: at().notNull().defaultNow(),
  },
  (t) => [
    index("contact_request_status_created_idx").on(t.status, t.createdAt),
    check(
      "contact_request_topic",
      sql`${t.topic} in (${inList(CONTACT_TOPICS)})`,
    ),
    check(
      "contact_request_status",
      sql`${t.status} in (${inList(CONTACT_STATUSES)})`,
    ),
    check(
      "contact_request_lengths",
      sql`char_length(${t.name}) between 1 and ${sql.raw(String(CONTACT_NAME_MAX))}
        and char_length(${t.email}) between 3 and ${sql.raw(String(CONTACT_EMAIL_MAX))}
        and (${t.company} is null or char_length(${t.company}) between 1 and ${sql.raw(String(CONTACT_COMPANY_MAX))})
        and char_length(${t.message}) between 1 and ${sql.raw(String(CONTACT_MESSAGE_MAX))}
        and (${t.staffNote} is null or char_length(${t.staffNote}) <= ${sql.raw(String(CONTACT_NOTE_MAX))})`,
    ),
  ],
);

export type SelectContactRequest = typeof contactRequest.$inferSelect;
