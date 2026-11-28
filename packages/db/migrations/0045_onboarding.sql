ALTER TABLE "personal_account" ADD COLUMN "onboarded_at" timestamp;--> statement-breakpoint
ALTER TABLE "personal_account" ADD COLUMN "account_type" text;--> statement-breakpoint
ALTER TABLE "personal_account" ADD COLUMN "starter_key_id" text;--> statement-breakpoint
ALTER TABLE "personal_account" ADD CONSTRAINT "personal_account_account_type" CHECK ("personal_account"."account_type" is null or "personal_account"."account_type" in ('individual', 'organization'));--> statement-breakpoint
-- ONB-03: the welcome wizard is for new users. Everyone who already has an account has been using
-- the product without it and is not sent back to it.
UPDATE "personal_account" SET "onboarded_at" = now() WHERE "onboarded_at" IS NULL;
