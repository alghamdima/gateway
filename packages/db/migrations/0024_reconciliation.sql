CREATE TABLE "reconciliation_run" (
	"id" text PRIMARY KEY NOT NULL,
	"period_start" timestamp NOT NULL,
	"period_end" timestamp NOT NULL,
	"status" text NOT NULL,
	"findings" jsonb NOT NULL,
	"created_at" timestamp DEFAULT now() NOT NULL,
	CONSTRAINT "reconciliation_run_status" CHECK ("reconciliation_run"."status" in ('ok', 'mismatch', 'error'))
);
--> statement-breakpoint
CREATE INDEX "reconciliation_run_periodStart_idx" ON "reconciliation_run" USING btree ("period_start");