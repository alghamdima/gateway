CREATE TYPE "public"."credit_entry_kind" AS ENUM('top_up', 'usage', 'refund', 'adjustment', 'promo');--> statement-breakpoint
CREATE TABLE "credit_account" (
	"organization_id" text PRIMARY KEY NOT NULL,
	"balance" bigint DEFAULT 0 NOT NULL,
	"held" bigint DEFAULT 0 NOT NULL,
	"created_at" timestamp DEFAULT now() NOT NULL,
	"updated_at" timestamp DEFAULT now() NOT NULL,
	CONSTRAINT "credit_account_held_not_negative" CHECK ("credit_account"."held" >= 0)
);
--> statement-breakpoint
CREATE TABLE "credit_ledger_entry" (
	"id" text PRIMARY KEY NOT NULL,
	"organization_id" text NOT NULL,
	"amount" bigint NOT NULL,
	"kind" "credit_entry_kind" NOT NULL,
	"idempotency_key" text NOT NULL,
	"reference" text,
	"metadata" jsonb,
	"created_at" timestamp DEFAULT now() NOT NULL,
	CONSTRAINT "credit_ledger_entry_idempotencyKey_unique" UNIQUE("idempotency_key"),
	CONSTRAINT "credit_ledger_entry_amount_not_zero" CHECK ("credit_ledger_entry"."amount" <> 0)
);
--> statement-breakpoint
CREATE TABLE "credit_reservation" (
	"id" text PRIMARY KEY NOT NULL,
	"organization_id" text NOT NULL,
	"amount" bigint NOT NULL,
	"expires_at" timestamp NOT NULL,
	"settled_at" timestamp,
	"created_at" timestamp DEFAULT now() NOT NULL,
	CONSTRAINT "credit_reservation_amount_positive" CHECK ("credit_reservation"."amount" > 0)
);
--> statement-breakpoint
ALTER TABLE "credit_account" ADD CONSTRAINT "credit_account_organization_id_organization_id_fk" FOREIGN KEY ("organization_id") REFERENCES "public"."organization"("id") ON DELETE cascade ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "credit_reservation" ADD CONSTRAINT "credit_reservation_organization_id_organization_id_fk" FOREIGN KEY ("organization_id") REFERENCES "public"."organization"("id") ON DELETE cascade ON UPDATE no action;--> statement-breakpoint
CREATE INDEX "credit_ledger_entry_organizationId_createdAt_idx" ON "credit_ledger_entry" USING btree ("organization_id","created_at");--> statement-breakpoint
CREATE INDEX "credit_reservation_open_expiresAt_idx" ON "credit_reservation" USING btree ("expires_at") WHERE "credit_reservation"."settled_at" is null;--> statement-breakpoint
CREATE OR REPLACE FUNCTION "public"."reject_credit_ledger_entry_change"()
RETURNS TRIGGER AS $$
BEGIN
  RAISE EXCEPTION 'credit_ledger_entry is append-only: % is not allowed', TG_OP
    USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;--> statement-breakpoint
CREATE TRIGGER "credit_ledger_entry_append_only_trigger"
BEFORE UPDATE OR DELETE ON "public"."credit_ledger_entry"
FOR EACH ROW
EXECUTE FUNCTION "public"."reject_credit_ledger_entry_change"();--> statement-breakpoint
DO $$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'gateway_ledger') THEN
    CREATE ROLE "gateway_ledger" NOLOGIN;
  END IF;
END
$$;--> statement-breakpoint
GRANT USAGE ON SCHEMA "public" TO "gateway_ledger";--> statement-breakpoint
GRANT SELECT, INSERT ON "public"."credit_ledger_entry" TO "gateway_ledger";--> statement-breakpoint
GRANT SELECT, UPDATE ON "public"."credit_account" TO "gateway_ledger";--> statement-breakpoint
GRANT SELECT, INSERT, UPDATE ON "public"."credit_reservation" TO "gateway_ledger";
