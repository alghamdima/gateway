import {
  CONTACT_COMPANY_MAX,
  CONTACT_MESSAGE_MAX,
  CONTACT_NAME_MAX,
  CONTACT_TOPICS,
  contactRequestSchema,
} from "@workspace/api/schema";

import type { ContactRequestInput } from "@workspace/api/schema";

export {
  CONTACT_COMPANY_MAX,
  CONTACT_MESSAGE_MAX,
  CONTACT_NAME_MAX,
  CONTACT_TOPICS,
  contactRequestSchema,
};

export type ContactTopic = (typeof CONTACT_TOPICS)[number];
export type ContactFormPayload = ContactRequestInput;

/** The topic a link asks for (`/contact?topic=sales`), or the first one. */
export const topicFrom = (value: string | undefined): ContactTopic =>
  CONTACT_TOPICS.find((topic) => topic === value) ?? CONTACT_TOPICS[0];
