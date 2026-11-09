-- +goose Up

-- Row-level security. The application role (fifty_app, the user-request path) can
-- see and change only the rows of groups its caller belongs to, whatever a handler's own
-- query says. The caller is the transaction-local setting app.user_id, which the api sets
-- as the first statement of every transaction it runs for a user (internal/dbscope). This
-- is defense in depth: the handlers still check membership and the BOLA suite still tests
-- it; this is what stands behind a handler that forgets.
--
-- What it enforces is MEMBERSHIP of the group. It does not enforce roles inside a group
-- (payer-only finalize and edit, owner-or-payer void, sender-only settlement): those stay
-- in the application.
--
-- What it does not cover: the identity is self-asserted (code running as fifty_app can set
-- app.user_id to anything); the worker and the api's no-user paths run as fifty_worker and
-- fifty_system, whose policies below let them see everything their grants allow; the
-- table owner is a superuser in this setup and bypasses row-level security even when it
-- is forced; `jobs` is not protected (it holds ids, and the api inserts its own jobs).

-- The caller. NULL when the setting is absent or empty, so a query run outside
-- dbscope matches no rows. (Once a session has set a custom setting even
-- transaction-locally, Postgres reports '' rather than NULL afterwards, hence NULLIF.)
-- +goose StatementBegin
CREATE FUNCTION app_user_id() RETURNS uuid
    LANGUAGE sql STABLE PARALLEL SAFE
AS $$ SELECT NULLIF(current_setting('app.user_id', true), '')::uuid $$;
-- +goose StatementEnd

-- The groups the caller belongs to. SECURITY DEFINER because a policy on memberships that
-- selected from memberships would recurse ("infinite recursion detected in policy"); the
-- function runs as the migration owner, a superuser, which is not subject to the policies.
-- Everything in it is schema-qualified and the search_path is pinned with pg_temp last, so
-- nobody can substitute a temporary table or function for what it reads. Policies call it
-- as an UNCORRELATED subquery so Postgres evaluates it once per statement, not once per row.
-- +goose StatementBegin
CREATE FUNCTION my_group_ids() RETURNS SETOF uuid
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$ SELECT m.group_id FROM public.memberships m WHERE m.user_id = public.app_user_id() $$;
-- +goose StatementEnd
REVOKE EXECUTE ON FUNCTION my_group_ids() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION my_group_ids() TO fifty_app;

-- Joining a group. A user is not yet a member, so no membership-based policy can let them
-- read the code or add themselves; and a policy that let a user insert their own membership
-- would let raw SQL skip the join code. So fifty_app gets NO way to insert a member row
-- (below, only the owner row of a group the caller created) and redeems a code through this
-- function, which does what LockRedeemableJoinCode, AddMembershipIfAbsent and
-- IncrementJoinCodeUses did, in one place: lock the redeemable code (a code that is not found,
-- revoked, expired or exhausted all return no row, indistinguishable), add the caller (taken
-- from app.user_id; there is no way to pass someone else) unless already a member, and spend
-- a use only when a member was added. The row lock serializes concurrent redeemers: under
-- READ COMMITTED a waiter re-evaluates the WHERE clause after the lock is released, so for the
-- last use the loser sees uses = max_uses and gets no row.
-- +goose StatementBegin
CREATE FUNCTION redeem_join_code(p_code_hash bytea)
    RETURNS TABLE (group_id uuid, join_code_id uuid, added boolean)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
#variable_conflict use_column
DECLARE
    v_user  uuid := public.app_user_id();
    v_code  uuid;
    v_group uuid;
    v_added integer;
BEGIN
    IF v_user IS NULL THEN
        RAISE EXCEPTION 'redeem_join_code: no caller (app.user_id is not set)' USING ERRCODE = '28000';
    END IF;

    SELECT jc.id, jc.group_id INTO v_code, v_group
    FROM public.join_codes jc
    WHERE jc.code_hash = p_code_hash
      AND jc.revoked_at IS NULL
      AND jc.expires_at > now()
      AND jc.uses < jc.max_uses
    FOR UPDATE;
    IF NOT FOUND THEN
        RETURN;
    END IF;

    INSERT INTO public.memberships (group_id, user_id, role)
    VALUES (v_group, v_user, 'member')
    ON CONFLICT ON CONSTRAINT memberships_pkey DO NOTHING;
    GET DIAGNOSTICS v_added = ROW_COUNT;

    IF v_added = 1 THEN
        UPDATE public.join_codes SET uses = uses + 1 WHERE id = v_code;
    END IF;

    group_id := v_group;
    join_code_id := v_code;
    added := (v_added = 1);
    RETURN NEXT;
END
$$;
-- +goose StatementEnd
REVOKE EXECUTE ON FUNCTION redeem_join_code(bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION redeem_join_code(bytea) TO fifty_app;

-- Tables, forced so that a non-superuser owner would be subject to them too. Not enabled
-- on `jobs` (the queue; holds ids only) or goose_db_version.
ALTER TABLE users               ENABLE ROW LEVEL SECURITY;
ALTER TABLE groups              ENABLE ROW LEVEL SECURITY;
ALTER TABLE memberships         ENABLE ROW LEVEL SECURITY;
ALTER TABLE join_codes          ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys    ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_events        ENABLE ROW LEVEL SECURITY;
ALTER TABLE expenses            ENABLE ROW LEVEL SECURITY;
ALTER TABLE expense_items       ENABLE ROW LEVEL SECURITY;
ALTER TABLE claims              ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger_transactions ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger_entries      ENABLE ROW LEVEL SECURITY;
ALTER TABLE settlements         ENABLE ROW LEVEL SECURITY;
ALTER TABLE receipts            ENABLE ROW LEVEL SECURITY;
ALTER TABLE receipt_quota       ENABLE ROW LEVEL SECURITY;

ALTER TABLE users               FORCE ROW LEVEL SECURITY;
ALTER TABLE groups              FORCE ROW LEVEL SECURITY;
ALTER TABLE memberships         FORCE ROW LEVEL SECURITY;
ALTER TABLE join_codes          FORCE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys    FORCE ROW LEVEL SECURITY;
ALTER TABLE audit_events        FORCE ROW LEVEL SECURITY;
ALTER TABLE expenses            FORCE ROW LEVEL SECURITY;
ALTER TABLE expense_items       FORCE ROW LEVEL SECURITY;
ALTER TABLE claims              FORCE ROW LEVEL SECURITY;
ALTER TABLE ledger_transactions FORCE ROW LEVEL SECURITY;
ALTER TABLE ledger_entries      FORCE ROW LEVEL SECURITY;
ALTER TABLE settlements         FORCE ROW LEVEL SECURITY;
ALTER TABLE receipts            FORCE ROW LEVEL SECURITY;
ALTER TABLE receipt_quota       FORCE ROW LEVEL SECURITY;

-- fifty_app: the rows of the caller's groups. FOR ALL with no separate WITH CHECK means the
-- same condition applies to rows written, so a row cannot be inserted into, or moved to,
-- a group the caller is not in. The grants already limit which commands exist per table.
CREATE POLICY app_group ON expenses            FOR ALL TO fifty_app USING (group_id IN (SELECT my_group_ids()));
CREATE POLICY app_group ON claims              FOR ALL TO fifty_app USING (group_id IN (SELECT my_group_ids()));
CREATE POLICY app_group ON ledger_transactions FOR ALL TO fifty_app USING (group_id IN (SELECT my_group_ids()));
CREATE POLICY app_group ON ledger_entries      FOR ALL TO fifty_app USING (group_id IN (SELECT my_group_ids()));
CREATE POLICY app_group ON settlements         FOR ALL TO fifty_app USING (group_id IN (SELECT my_group_ids()));
CREATE POLICY app_group ON receipts            FOR ALL TO fifty_app USING (group_id IN (SELECT my_group_ids()));
CREATE POLICY app_group ON audit_events        FOR ALL TO fifty_app USING (group_id IN (SELECT my_group_ids()));
CREATE POLICY app_group ON join_codes          FOR ALL TO fifty_app USING (group_id IN (SELECT my_group_ids()));

-- Items have no group_id; they follow their expense, whose own policy applies inside this one.
CREATE POLICY app_group ON expense_items FOR ALL TO fifty_app
    USING (EXISTS (SELECT 1 FROM expenses e WHERE e.id = expense_id));

-- A group is visible to its members, and to its creator before the owner membership exists
-- (INSERT ... RETURNING needs the new row to be visible).
CREATE POLICY app_group ON groups FOR ALL TO fifty_app
    USING (id IN (SELECT my_group_ids()) OR created_by = app_user_id());

-- Memberships: visible to the group's members. The only row fifty_app can insert is the
-- owner row of a group the caller created; every other member arrives through
-- redeem_join_code. No update or delete policy: nothing in the app does either.
CREATE POLICY app_select ON memberships FOR SELECT TO fifty_app
    USING (group_id IN (SELECT my_group_ids()));
CREATE POLICY app_insert_owner ON memberships FOR INSERT TO fifty_app
    WITH CHECK (user_id = app_user_id() AND role = 'owner'
                AND EXISTS (SELECT 1 FROM groups g WHERE g.id = group_id AND g.created_by = app_user_id()));

-- Users: yourself and the people you share a group with (the memberships policy already
-- limits that subquery to your groups). fifty_app never writes users.
CREATE POLICY app_select ON users FOR SELECT TO fifty_app
    USING (id = app_user_id() OR id IN (SELECT user_id FROM memberships));

-- Per-user tables.
CREATE POLICY app_user ON idempotency_keys FOR ALL TO fifty_app USING (user_id = app_user_id());
CREATE POLICY app_user ON receipt_quota    FOR ALL TO fifty_app USING (user_id = app_user_id());

-- fifty_worker and fifty_system: explicit, visible exemptions (not BYPASSRLS). Each role has
-- a policy on exactly the tables its grants (00015) cover; test/rls checks the two agree.
CREATE POLICY worker_all ON audit_events        FOR ALL TO fifty_worker USING (true) WITH CHECK (true);
CREATE POLICY worker_all ON expense_items       FOR ALL TO fifty_worker USING (true) WITH CHECK (true);
CREATE POLICY worker_all ON expenses            FOR ALL TO fifty_worker USING (true) WITH CHECK (true);
CREATE POLICY worker_all ON groups              FOR ALL TO fifty_worker USING (true) WITH CHECK (true);
CREATE POLICY worker_all ON idempotency_keys    FOR ALL TO fifty_worker USING (true) WITH CHECK (true);
CREATE POLICY worker_all ON ledger_entries      FOR ALL TO fifty_worker USING (true) WITH CHECK (true);
CREATE POLICY worker_all ON ledger_transactions FOR ALL TO fifty_worker USING (true) WITH CHECK (true);
CREATE POLICY worker_all ON receipts            FOR ALL TO fifty_worker USING (true) WITH CHECK (true);
CREATE POLICY worker_all ON settlements         FOR ALL TO fifty_worker USING (true) WITH CHECK (true);

CREATE POLICY system_all ON audit_events        FOR ALL TO fifty_system USING (true) WITH CHECK (true);
CREATE POLICY system_all ON groups              FOR ALL TO fifty_system USING (true) WITH CHECK (true);
CREATE POLICY system_all ON ledger_entries      FOR ALL TO fifty_system USING (true) WITH CHECK (true);
CREATE POLICY system_all ON ledger_transactions FOR ALL TO fifty_system USING (true) WITH CHECK (true);
CREATE POLICY system_all ON settlements         FOR ALL TO fifty_system USING (true) WITH CHECK (true);
CREATE POLICY system_all ON users               FOR ALL TO fifty_system USING (true) WITH CHECK (true);

-- +goose Down
-- +goose StatementBegin
DO $$
DECLARE r record;
BEGIN
    FOR r IN SELECT tablename, policyname FROM pg_policies WHERE schemaname = 'public' LOOP
        EXECUTE format('DROP POLICY %I ON public.%I', r.policyname, r.tablename);
    END LOOP;
END
$$;
-- +goose StatementEnd

ALTER TABLE users               NO FORCE ROW LEVEL SECURITY;
ALTER TABLE groups              NO FORCE ROW LEVEL SECURITY;
ALTER TABLE memberships         NO FORCE ROW LEVEL SECURITY;
ALTER TABLE join_codes          NO FORCE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys    NO FORCE ROW LEVEL SECURITY;
ALTER TABLE audit_events        NO FORCE ROW LEVEL SECURITY;
ALTER TABLE expenses            NO FORCE ROW LEVEL SECURITY;
ALTER TABLE expense_items       NO FORCE ROW LEVEL SECURITY;
ALTER TABLE claims              NO FORCE ROW LEVEL SECURITY;
ALTER TABLE ledger_transactions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE ledger_entries      NO FORCE ROW LEVEL SECURITY;
ALTER TABLE settlements         NO FORCE ROW LEVEL SECURITY;
ALTER TABLE receipts            NO FORCE ROW LEVEL SECURITY;
ALTER TABLE receipt_quota       NO FORCE ROW LEVEL SECURITY;

ALTER TABLE users               DISABLE ROW LEVEL SECURITY;
ALTER TABLE groups              DISABLE ROW LEVEL SECURITY;
ALTER TABLE memberships         DISABLE ROW LEVEL SECURITY;
ALTER TABLE join_codes          DISABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys    DISABLE ROW LEVEL SECURITY;
ALTER TABLE audit_events        DISABLE ROW LEVEL SECURITY;
ALTER TABLE expenses            DISABLE ROW LEVEL SECURITY;
ALTER TABLE expense_items       DISABLE ROW LEVEL SECURITY;
ALTER TABLE claims              DISABLE ROW LEVEL SECURITY;
ALTER TABLE ledger_transactions DISABLE ROW LEVEL SECURITY;
ALTER TABLE ledger_entries      DISABLE ROW LEVEL SECURITY;
ALTER TABLE settlements         DISABLE ROW LEVEL SECURITY;
ALTER TABLE receipts            DISABLE ROW LEVEL SECURITY;
ALTER TABLE receipt_quota       DISABLE ROW LEVEL SECURITY;

DROP FUNCTION redeem_join_code(bytea);
DROP FUNCTION my_group_ids();
DROP FUNCTION app_user_id();
