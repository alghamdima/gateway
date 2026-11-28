-- Reverses 0042 (personal accounts, ADR 0020). Run by hand, then remove the 0042 row from
-- drizzle.__drizzle_migrations. Deleting a personal account's organization row cascades to its
-- keys, credits and ledger: move or refund any balance first, and remove each one from the
-- gateway (removeOrganizationFromGateway) before running this.
DELETE FROM "organization" WHERE "id" IN (SELECT "organization_id" FROM "personal_account");
DROP TABLE "personal_account";
