import { afterAll, beforeAll, describe, expect, it } from "vitest";

import type { TestDatabase } from "@workspace/db/testing";
import type postgres from "postgres";

// The site settings row with its append-only history, and the contact
// messages, against real Postgres. Skips without DATABASE_URL.
const adminUrl = process.env.DATABASE_URL;

describe.skipIf(!adminUrl)("public site (SITE-01 to SITE-03)", () => {
  let database: TestDatabase;
  let sql: postgres.Sql;
  let settings: typeof import("../settings");
  let contact: typeof import("../contact");

  beforeAll(async () => {
    const { createTestDatabase } = await import("@workspace/db/testing");
    database = await createTestDatabase(adminUrl ?? "");
    process.env.DATABASE_URL = database.url;

    sql = (await import("postgres")).default(database.url, { max: 2 });
    settings = await import("../settings");
    contact = await import("../contact");
    await sql`insert into "user" (id, name, email, email_verified, created_at, updated_at)
      values ('staff', 'Staff', 'staff@example.com', true, now(), now())`;
  });

  afterAll(async () => {
    const { db } = await import("@workspace/db/server");
    await db.$client.end();
    await sql.end();
    await database.drop();
  });

  describe("settings", () => {
    it("starts with the blog off and no email about contact messages", async () => {
      expect(await settings.getSiteSettings()).toMatchObject({
        isBlogEnabled: false,
        contactEmail: null,
      });
    });

    it("tells a visitor the switches only, never the address", async () => {
      expect(await settings.getPublicSiteSettings()).toEqual({
        isBlogEnabled: false,
      });
    });

    it("records a change with before, after and who made it", async () => {
      const change = await settings.updateSiteSettings({
        isBlogEnabled: true,
        contactEmail: "sales@example.com",
        staffId: "staff",
      });
      const before = { isBlogEnabled: false, contactEmail: null };
      const after = { isBlogEnabled: true, contactEmail: "sales@example.com" };
      expect(change.before).toEqual(before);
      expect(change.after).toEqual(after);
      expect(await settings.getPublicSiteSettings()).toEqual({
        isBlogEnabled: true,
      });

      const [latest] = await settings.getSiteSettingsChanges();
      expect(latest).toMatchObject({
        before,
        after,
        changedBy: "staff",
        changedByName: "Staff",
      });
    });

    it("keeps the history append-only", async () => {
      await expect(sql`delete from site_settings_change`).rejects.toThrow(
        /append-only/,
      );
      await expect(
        sql`update site_settings_change set changed_by = 'someone'`,
      ).rejects.toThrow(/append-only/);
    });

    it("holds a single row", async () => {
      await expect(
        sql`insert into site_settings (id) values (2)`,
      ).rejects.toThrow(/site_settings_single_row/);
    });
  });

  describe("contact messages", () => {
    const message = {
      topic: "provider" as const,
      name: "Sara Provider",
      email: "Sara@Models.example",
      company: "Models Co",
      message: "We host open models in Riyadh and would like to be listed.",
    };

    it("keeps a message as new, with the email in lower case", async () => {
      const { id } = await contact.createContactRequest(message);
      expect(await contact.getContactRequest(id)).toMatchObject({
        topic: "provider",
        name: "Sara Provider",
        email: "sara@models.example",
        company: "Models Co",
        message: message.message,
        status: "new",
        staffNote: null,
        userId: null,
      });
    });

    it("lists newest first, filters, and counts what waits for an answer", async () => {
      await contact.createContactRequest({
        ...message,
        topic: "sales",
        name: "Omar Buyer",
        company: null,
      });
      const all = await contact.listContactRequests({});
      expect(all.items.map((item) => item.name)).toEqual([
        "Omar Buyer",
        "Sara Provider",
      ]);
      expect(all).toMatchObject({ total: 2, waiting: 2 });

      const sales = await contact.listContactRequests({ topic: "sales" });
      expect(sales.items.map((item) => item.name)).toEqual(["Omar Buyer"]);
      expect(sales.total).toBe(1);
    });

    it("lets staff set the status and a note, and says what changed", async () => {
      const { items } = await contact.listContactRequests({ topic: "sales" });
      const id = items[0]?.id ?? 0;
      const change = await contact.updateContactRequest({
        id,
        status: "in_progress",
        staffNote: "Sent the Enterprise terms.",
        staffId: "staff",
      });
      expect(change.before).toEqual({ status: "new", staffNote: null });
      expect(change.after).toEqual({
        status: "in_progress",
        staffNote: "Sent the Enterprise terms.",
      });
      expect(await contact.getContactRequest(id)).toMatchObject({
        status: "in_progress",
        staffNote: "Sent the Enterprise terms.",
        updatedBy: "staff",
        updatedByName: "Staff",
        // what the sender wrote is untouched
        name: "Omar Buyer",
        message: message.message,
      });
      expect((await contact.listContactRequests({})).waiting).toBe(1);
    });

    it("answers 404 for a message that does not exist", async () => {
      await expect(contact.getContactRequest(999_999)).rejects.toMatchObject({
        status: 404,
      });
      await expect(
        contact.updateContactRequest({
          id: 999_999,
          status: "closed",
          staffNote: null,
          staffId: "staff",
        }),
      ).rejects.toMatchObject({ status: 404 });
    });

    it("refuses a topic or a status outside the lists", async () => {
      await expect(
        sql`insert into contact_request (topic, name, email, message) values ('spam', 'A B', 'a@b.example', 'hello there')`,
      ).rejects.toThrow(/contact_request_topic/);
      await expect(
        sql`update contact_request set status = 'lost'`,
      ).rejects.toThrow(/contact_request_status/);
    });

    it("refuses the sixth message from one sender within the hour, and counts again after it", async () => {
      const now = Date.UTC(2026, 10, 28, 12);
      for (
        let attempt = 1;
        attempt <= contact.CONTACT_MAX_PER_WINDOW;
        attempt++
      ) {
        await contact.countContactAttempt("203.0.113.9", now + attempt);
      }
      await expect(
        contact.countContactAttempt("203.0.113.9", now + 60_000),
      ).rejects.toMatchObject({ status: 429 });
      // another sender is not affected
      await contact.countContactAttempt("203.0.113.10", now + 60_000);
      // the window has passed
      await contact.countContactAttempt(
        "203.0.113.9",
        now + contact.CONTACT_WINDOW_MS + 60_000,
      );
    });
  });
});
