-- Reverses 0045 (the onboarding wizard's state, ONB-03). Run by hand, then remove the 0045 row
-- from drizzle.__drizzle_migrations. Revert the web app first: it reads onboarded_at on every
-- dashboard page. Starter keys already minted stay as ordinary keys.
ALTER TABLE "personal_account" DROP CONSTRAINT "personal_account_account_type";
ALTER TABLE "personal_account" DROP COLUMN "starter_key_id";
ALTER TABLE "personal_account" DROP COLUMN "account_type";
ALTER TABLE "personal_account" DROP COLUMN "onboarded_at";
