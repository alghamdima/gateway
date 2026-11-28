import * as z from "zod";

import {
  CONTACT_COMPANY_MAX,
  CONTACT_EMAIL_MAX,
  CONTACT_MESSAGE_MAX,
  CONTACT_NAME_MAX,
  CONTACT_NOTE_MAX,
  CONTACT_STATUSES,
  CONTACT_TOPICS,
} from "@workspace/db/schema";

export {
  CONTACT_COMPANY_MAX,
  CONTACT_MESSAGE_MAX,
  CONTACT_NAME_MAX,
  CONTACT_NOTE_MAX,
  CONTACT_STATUSES,
  CONTACT_TOPICS,
};

/** What staff switch on the public site (SITE-01). */
export const updateSiteSettingsSchema = z.object({
  isBlogEnabled: z.boolean(),
  /** Where an email about each new contact message goes; null: none is sent (ADMIN-20). */
  contactEmail: z.email().max(CONTACT_EMAIL_MAX).nullable(),
});

/** A message sent through the contact page (SITE-02). */
export const contactRequestSchema = z.object({
  topic: z.enum(CONTACT_TOPICS),
  name: z.string().trim().min(2).max(CONTACT_NAME_MAX),
  email: z.email().max(CONTACT_EMAIL_MAX),
  company: z
    .string()
    .trim()
    .max(CONTACT_COMPANY_MAX)
    .transform((value) => value || null)
    .nullish(),
  message: z.string().trim().min(10).max(CONTACT_MESSAGE_MAX),
});

export type ContactRequestInput = z.input<typeof contactRequestSchema>;

/** What staff change on a message: where it stands, and their own note (SITE-03). */
export const updateContactRequestSchema = z.object({
  status: z.enum(CONTACT_STATUSES),
  staffNote: z
    .string()
    .trim()
    .max(CONTACT_NOTE_MAX)
    .transform((value) => value || null)
    .nullable(),
});

export const listContactRequestsSchema = z.object({
  status: z.enum(CONTACT_STATUSES).optional(),
  topic: z.enum(CONTACT_TOPICS).optional(),
  page: z.coerce.number().int().min(1).default(1),
});
