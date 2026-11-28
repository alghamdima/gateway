import { and, count, desc, eq, sql } from "@workspace/db";
import { contactRequest, user } from "@workspace/db/schema";
import { db } from "@workspace/db/server";
import { HttpStatusCode } from "@workspace/shared/constants";
import { HttpException } from "@workspace/shared/utils";

import type { ContactStatus, ContactTopic } from "@workspace/db/schema";

/**
 * Messages sent through the contact page (SITE-02, SITE-03): someone who
 * wants a quote, a company that wants to offer its models, a customer who
 * needs help. Every message is kept, so staff answer from the admin and the
 * conversation's state survives a lost email.
 */

/** A sender may write this many messages in the window; the next is refused. */
export const CONTACT_MAX_PER_WINDOW = 5;
export const CONTACT_WINDOW_MS = 3_600_000;

export const PAGE_SIZE = 25;

const tooMany = () =>
  new HttpException(HttpStatusCode.TOO_MANY_REQUESTS, {
    message: "Too many messages. Try again later.",
  });

/**
 * Counts one message from this sender and refuses past the limit. Counted in
 * rate_limit, the table Better Auth's limits use, so every instance of the
 * web app shares the count. The sender is the client address, or the email
 * when the address is unknown.
 */
export const countContactAttempt = async (sender: string, now = Date.now()) => {
  const [row] = await db.execute<{ count: number }>(sql`
    INSERT INTO rate_limit (id, key, count, last_request)
    VALUES (gen_random_uuid()::text, ${`contact:${sender}`}, 1, ${now})
    ON CONFLICT (key) DO UPDATE SET
      count = CASE WHEN rate_limit.last_request < ${now - CONTACT_WINDOW_MS} THEN 1 ELSE rate_limit.count + 1 END,
      last_request = CASE WHEN rate_limit.last_request < ${now - CONTACT_WINDOW_MS} THEN ${now} ELSE rate_limit.last_request END
    RETURNING count`);
  if ((row?.count ?? 0) > CONTACT_MAX_PER_WINDOW) {
    throw tooMany();
  }
};

export interface NewContactRequest {
  topic: ContactTopic;
  name: string;
  email: string;
  company?: string | null;
  message: string;
  /** The signed-in user who sent it, if any. */
  userId?: string | null;
}

export const createContactRequest = async (input: NewContactRequest) => {
  const [row] = await db
    .insert(contactRequest)
    .values({
      topic: input.topic,
      name: input.name,
      email: input.email.toLowerCase(),
      company: input.company ?? null,
      message: input.message,
      userId: input.userId ?? null,
    })
    .returning({ id: contactRequest.id });
  if (!row) {
    throw new Error("contact_request insert returned no row");
  }
  return row;
};

const columns = {
  id: contactRequest.id,
  topic: contactRequest.topic,
  name: contactRequest.name,
  email: contactRequest.email,
  company: contactRequest.company,
  message: contactRequest.message,
  userId: contactRequest.userId,
  status: contactRequest.status,
  staffNote: contactRequest.staffNote,
  createdAt: contactRequest.createdAt,
  updatedAt: contactRequest.updatedAt,
  updatedBy: contactRequest.updatedBy,
  updatedByName: user.name,
};

/** Messages newest first, with how many wait for a first answer. */
export const listContactRequests = async ({
  status,
  topic,
  page = 1,
}: {
  status?: ContactStatus;
  topic?: ContactTopic;
  page?: number;
}) => {
  const where = and(
    status ? eq(contactRequest.status, status) : undefined,
    topic ? eq(contactRequest.topic, topic) : undefined,
  );
  const [items, [total], [waiting]] = await Promise.all([
    db
      .select(columns)
      .from(contactRequest)
      .leftJoin(user, eq(user.id, contactRequest.updatedBy))
      .where(where)
      .orderBy(desc(contactRequest.id))
      .limit(PAGE_SIZE)
      .offset((page - 1) * PAGE_SIZE),
    db.select({ value: count() }).from(contactRequest).where(where),
    db
      .select({ value: count() })
      .from(contactRequest)
      .where(eq(contactRequest.status, "new")),
  ]);
  return {
    items,
    total: total?.value ?? 0,
    waiting: waiting?.value ?? 0,
    pageSize: PAGE_SIZE,
  };
};

const notFound = () =>
  new HttpException(HttpStatusCode.NOT_FOUND, { message: "No such message" });

export const getContactRequest = async (id: number) => {
  const [row] = await db
    .select(columns)
    .from(contactRequest)
    .leftJoin(user, eq(user.id, contactRequest.updatedBy))
    .where(eq(contactRequest.id, id));
  if (!row) {
    throw notFound();
  }
  return row;
};

/** Staff set where a message stands and their own note; what the sender wrote is never changed. */
export const updateContactRequest = async ({
  id,
  status,
  staffNote,
  staffId,
}: {
  id: number;
  status: ContactStatus;
  staffNote: string | null;
  staffId: string;
}) =>
  db.transaction(async (tx) => {
    const [current] = await tx
      .select({
        status: contactRequest.status,
        staffNote: contactRequest.staffNote,
      })
      .from(contactRequest)
      .where(eq(contactRequest.id, id))
      .for("update");
    if (!current) {
      throw notFound();
    }
    await tx
      .update(contactRequest)
      .set({ status, staffNote, updatedAt: new Date(), updatedBy: staffId })
      .where(eq(contactRequest.id, id));
    return { before: { ...current }, after: { status, staffNote } };
  });
