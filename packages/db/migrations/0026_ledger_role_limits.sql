-- The gateway connects as gateway_ledger. Its statements (apps/gateway/plugins/credits/store.go)
-- only ever charge usage, never credit an account, and only close or extend holds. These limits
-- make that true of anything the role can do, so a compromised gateway cannot mint credit.
-- Production must connect as gateway_ledger itself (ALTER ROLE gateway_ledger LOGIN PASSWORD ...),
-- not as a member of it: the checks name the role.

-- holds: only settled_at and expires_at may change
REVOKE UPDATE ON "public"."credit_reservation" FROM "gateway_ledger";--> statement-breakpoint
GRANT UPDATE ("settled_at", "expires_at") ON "public"."credit_reservation" TO "gateway_ledger";--> statement-breakpoint

-- accounts: only the balance and the held amount move, and the balance never goes up
REVOKE UPDATE ON "public"."credit_account" FROM "gateway_ledger";--> statement-breakpoint
GRANT UPDATE ("balance", "held", "updated_at") ON "public"."credit_account" TO "gateway_ledger";--> statement-breakpoint
CREATE OR REPLACE FUNCTION "public"."limit_gateway_ledger_account"()
RETURNS TRIGGER AS $$
BEGIN
  IF current_user = 'gateway_ledger' AND NEW.balance > OLD.balance THEN
    RAISE EXCEPTION 'gateway_ledger cannot raise a balance (% to %)', OLD.balance, NEW.balance
      USING ERRCODE = 'insufficient_privilege';
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;--> statement-breakpoint
CREATE TRIGGER "credit_account_gateway_ledger_limit_trigger"
BEFORE UPDATE ON "public"."credit_account"
FOR EACH ROW
EXECUTE FUNCTION "public"."limit_gateway_ledger_account"();--> statement-breakpoint

-- ledger: only usage entries, which take money out
CREATE OR REPLACE FUNCTION "public"."limit_gateway_ledger_entry"()
RETURNS TRIGGER AS $$
BEGIN
  IF current_user = 'gateway_ledger' AND (NEW.kind <> 'usage' OR NEW.amount > 0) THEN
    RAISE EXCEPTION 'gateway_ledger may only write usage charges, not % of %', NEW.kind, NEW.amount
      USING ERRCODE = 'insufficient_privilege';
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;--> statement-breakpoint
CREATE TRIGGER "credit_ledger_entry_gateway_ledger_limit_trigger"
BEFORE INSERT ON "public"."credit_ledger_entry"
FOR EACH ROW
EXECUTE FUNCTION "public"."limit_gateway_ledger_entry"();
