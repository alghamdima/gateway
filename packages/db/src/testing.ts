import { drizzle } from "drizzle-orm/postgres-js";
import { migrate } from "drizzle-orm/postgres-js/migrator";
import { fileURLToPath } from "node:url";
import postgres from "postgres";

export interface TestDatabase {
  /** Connection string of the throwaway database. */
  url: string;
  /** Drops the database. Close every connection to it first. */
  drop: () => Promise<void>;
}

/**
 * Creates an empty database next to the one in `adminUrl`, applies every
 * migration to it, and returns its URL. For tests that need real Postgres
 * behaviour (constraints, roles, triggers, concurrency). The role in
 * `adminUrl` needs CREATEDB.
 */
export const createTestDatabase = async (
  adminUrl: string,
): Promise<TestDatabase> => {
  const name = `test_${Date.now()}_${Math.random().toString(36).slice(2, 8)}`;
  const admin = postgres(adminUrl, { max: 1, onnotice: () => undefined });
  await admin.unsafe(`CREATE DATABASE "${name}"`);

  const target = new URL(adminUrl);
  target.pathname = `/${name}`;
  const url = target.toString();

  const client = postgres(url, { max: 1, onnotice: () => undefined });
  await migrate(drizzle({ client, casing: "snake_case" }), {
    migrationsFolder: fileURLToPath(new URL("../migrations", import.meta.url)),
  });
  await client.end();

  return {
    url,
    drop: async () => {
      await admin.unsafe(`DROP DATABASE "${name}" WITH (FORCE)`);
      await admin.end();
    },
  };
};
