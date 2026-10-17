CREATE TABLE wallets (
    id UUID PRIMARY KEY,
    player_id UUID NOT NULL,
    currency CHAR(3) NOT NULL CHECK (currency IN ('BRL', 'USD', 'EUR')),
    balance_minor BIGINT NOT NULL CHECK (balance_minor >= 0),
    version BIGINT NOT NULL CHECK (version > 0),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL CHECK (updated_at >= created_at),
    UNIQUE (player_id, currency),
    UNIQUE (id, player_id, currency),
    UNIQUE (id, currency)
);

CREATE FUNCTION guard_wallet_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.id, NEW.player_id, NEW.currency, NEW.created_at)
       IS DISTINCT FROM ROW(OLD.id, OLD.player_id, OLD.currency, OLD.created_at) THEN
        RAISE EXCEPTION 'wallet identity fields are immutable'
            USING ERRCODE = '23514';
    END IF;
    IF NEW.version <> OLD.version + 1 OR NEW.updated_at < OLD.updated_at THEN
        RAISE EXCEPTION 'wallet updates must increment version and preserve timestamp order'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER wallet_update_guard
BEFORE UPDATE ON wallets
FOR EACH ROW EXECUTE FUNCTION guard_wallet_update();

CREATE FUNCTION reject_wallet_delete() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'wallets cannot be deleted'
        USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER wallet_delete_guard
BEFORE DELETE ON wallets
FOR EACH ROW EXECUTE FUNCTION reject_wallet_delete();

CREATE TABLE wager_transactions (
    id UUID PRIMARY KEY,
    origin TEXT NOT NULL CHECK (origin IN ('EXTERNAL', 'INTERNAL')),
    provider_id TEXT,
    external_transaction_id TEXT,
    idempotency_key TEXT,
    payload_hash CHAR(64),
    wallet_id UUID NOT NULL,
    player_id UUID NOT NULL,
    currency CHAR(3) NOT NULL CHECK (currency IN ('BRL', 'USD', 'EUR')),
    round_id TEXT,
    game_id TEXT,
    kind TEXT NOT NULL CHECK (kind IN ('OPENING', 'BET', 'WIN', 'LOSS', 'REFUND', 'ROLLBACK')),
    amount_minor BIGINT NOT NULL CHECK (amount_minor >= 0),
    reference_external_transaction_id TEXT,
    resolved_reference_id UUID,
    status TEXT NOT NULL CHECK (status IN ('PENDING', 'PENDING_REFERENCE', 'PROCESSED', 'REJECTED', 'FAILED')),
    failure_code TEXT,
    result_balance_minor BIGINT CHECK (result_balance_minor >= 0),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL CHECK (updated_at >= created_at),
    UNIQUE (id, wallet_id),
    UNIQUE (id, wallet_id, currency),
    UNIQUE (id, provider_id, idempotency_key, payload_hash),
    FOREIGN KEY (wallet_id, player_id, currency)
        REFERENCES wallets (id, player_id, currency),
    FOREIGN KEY (resolved_reference_id)
        REFERENCES wager_transactions (id)
        DEFERRABLE INITIALLY DEFERRED,
    CHECK (
        (origin = 'EXTERNAL'
            AND provider_id IS NOT NULL AND length(btrim(provider_id)) > 0
            AND external_transaction_id IS NOT NULL AND length(btrim(external_transaction_id)) > 0
            AND idempotency_key IS NOT NULL AND length(btrim(idempotency_key)) > 0
            AND payload_hash ~ '^[0-9a-f]{64}$'
            AND round_id IS NOT NULL AND length(btrim(round_id)) > 0
            AND game_id IS NOT NULL AND length(btrim(game_id)) > 0
            AND kind <> 'OPENING')
        OR
        (origin = 'INTERNAL'
            AND provider_id IS NULL AND external_transaction_id IS NULL
            AND idempotency_key IS NULL AND payload_hash IS NULL
            AND round_id IS NULL AND game_id IS NULL
            AND kind = 'OPENING' AND status = 'PROCESSED'
            AND failure_code IS NULL AND result_balance_minor = amount_minor)
    ),
    CHECK (
        (kind = 'LOSS' AND amount_minor = 0)
        OR (kind <> 'LOSS' AND amount_minor > 0)
    ),
    CHECK (
        (kind IN ('REFUND', 'ROLLBACK') AND reference_external_transaction_id IS NOT NULL
            AND length(btrim(reference_external_transaction_id)) > 0)
        OR (kind NOT IN ('REFUND', 'ROLLBACK')
            AND (kind = 'WIN' OR reference_external_transaction_id IS NULL))
    ),
    CHECK (
        (status = 'PROCESSED' AND failure_code IS NULL AND result_balance_minor IS NOT NULL)
        OR (status IN ('REJECTED', 'FAILED') AND failure_code IS NOT NULL AND result_balance_minor IS NULL)
        OR (status IN ('PENDING', 'PENDING_REFERENCE') AND failure_code IS NULL AND result_balance_minor IS NULL)
    ),
    CHECK (
        failure_code IS NULL
        OR (length(failure_code) BETWEEN 1 AND 64 AND failure_code ~ '^[A-Z0-9_]+$')
    ),
    CHECK (
        status <> 'PENDING_REFERENCE'
        OR (kind IN ('WIN', 'REFUND', 'ROLLBACK') AND reference_external_transaction_id IS NOT NULL)
    ),
    CHECK (
        status <> 'PROCESSED'
        OR reference_external_transaction_id IS NULL
        OR resolved_reference_id IS NOT NULL
    )
);

CREATE UNIQUE INDEX wager_transactions_provider_external_uq
    ON wager_transactions (provider_id, external_transaction_id)
    WHERE origin = 'EXTERNAL';
CREATE UNIQUE INDEX wager_transactions_provider_idempotency_uq
    ON wager_transactions (provider_id, idempotency_key)
    WHERE origin = 'EXTERNAL';
CREATE INDEX wager_transactions_pending_reference_idx
    ON wager_transactions (created_at, id)
    WHERE status = 'PENDING_REFERENCE';
CREATE INDEX wager_transactions_wallet_created_idx
    ON wager_transactions (wallet_id, created_at DESC, id DESC);
CREATE INDEX wager_transactions_reference_lookup_idx
    ON wager_transactions (provider_id, external_transaction_id)
    WHERE origin = 'EXTERNAL';

CREATE FUNCTION guard_wager_transaction_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.id, NEW.origin, NEW.provider_id, NEW.external_transaction_id,
           NEW.idempotency_key, NEW.payload_hash, NEW.wallet_id, NEW.player_id,
           NEW.currency, NEW.round_id, NEW.game_id, NEW.kind, NEW.amount_minor,
           NEW.reference_external_transaction_id, NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.id, OLD.origin, OLD.provider_id, OLD.external_transaction_id,
           OLD.idempotency_key, OLD.payload_hash, OLD.wallet_id, OLD.player_id,
           OLD.currency, OLD.round_id, OLD.game_id, OLD.kind, OLD.amount_minor,
           OLD.reference_external_transaction_id, OLD.created_at) THEN
        RAISE EXCEPTION 'wager transaction request fields are immutable'
            USING ERRCODE = '23514';
    END IF;

    IF OLD.status IN ('PROCESSED', 'REJECTED', 'FAILED') THEN
        RAISE EXCEPTION 'terminal wager transaction is immutable'
            USING ERRCODE = '23514';
    END IF;

    IF NOT (
        (OLD.status = 'PENDING' AND NEW.status IN ('PENDING_REFERENCE', 'PROCESSED', 'REJECTED', 'FAILED'))
        OR (OLD.status = 'PENDING_REFERENCE' AND NEW.status IN ('PROCESSED', 'REJECTED', 'FAILED'))
    ) THEN
        RAISE EXCEPTION 'invalid wager transaction state transition'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER wager_transaction_update_guard
BEFORE UPDATE ON wager_transactions
FOR EACH ROW EXECUTE FUNCTION guard_wager_transaction_update();

CREATE FUNCTION reject_wager_delete() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'wager transactions cannot be deleted'
        USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER wager_transaction_delete_guard
BEFORE DELETE ON wager_transactions
FOR EACH ROW EXECUTE FUNCTION reject_wager_delete();

CREATE FUNCTION validate_wager_reference() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    ref wager_transactions%ROWTYPE;
BEGIN
    IF NEW.status = 'PROCESSED' AND NEW.reference_external_transaction_id IS NOT NULL
       AND NEW.resolved_reference_id IS NULL THEN
        RAISE EXCEPTION 'processed referenced wager has no resolved reference'
            USING ERRCODE = '23514';
    END IF;

    IF NEW.resolved_reference_id IS NOT NULL THEN
        SELECT * INTO ref FROM wager_transactions WHERE id = NEW.resolved_reference_id;
        IF ref.id IS NULL OR ref.origin <> 'EXTERNAL' OR ref.status <> 'PROCESSED'
           OR ref.provider_id <> NEW.provider_id
           OR ref.external_transaction_id <> NEW.reference_external_transaction_id
           OR ref.wallet_id <> NEW.wallet_id OR ref.player_id <> NEW.player_id
           OR ref.currency <> NEW.currency OR ref.round_id <> NEW.round_id
           OR ref.amount_minor <> NEW.amount_minor
           OR (NEW.kind = 'REFUND' AND ref.kind <> 'BET')
           OR (NEW.kind = 'ROLLBACK' AND ref.kind NOT IN ('BET', 'WIN', 'REFUND'))
           OR (NEW.kind = 'WIN' AND ref.kind <> 'BET')
           OR NEW.kind NOT IN ('WIN', 'REFUND', 'ROLLBACK') THEN
            RAISE EXCEPTION 'wager resolved reference does not match operation'
                USING ERRCODE = '23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER wager_reference_validate
BEFORE INSERT OR UPDATE OF status, resolved_reference_id
ON wager_transactions
FOR EACH ROW EXECUTE FUNCTION validate_wager_reference();

CREATE TABLE idempotency_records (
    provider_id TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    payload_hash CHAR(64) NOT NULL CHECK (payload_hash ~ '^[0-9a-f]{64}$'),
    transaction_id UUID NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (provider_id, idempotency_key),
    FOREIGN KEY (transaction_id, provider_id, idempotency_key, payload_hash)
        REFERENCES wager_transactions (id, provider_id, idempotency_key, payload_hash)
        DEFERRABLE INITIALLY DEFERRED
);

CREATE FUNCTION reject_idempotency_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'idempotency records are immutable'
        USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER idempotency_append_only
BEFORE UPDATE OR DELETE ON idempotency_records
FOR EACH ROW EXECUTE FUNCTION reject_idempotency_mutation();

CREATE TABLE wallet_ledger_entries (
    id UUID PRIMARY KEY,
    wallet_id UUID NOT NULL,
    transaction_id UUID NOT NULL,
    direction TEXT NOT NULL CHECK (direction IN ('DEBIT', 'CREDIT')),
    amount_minor BIGINT NOT NULL CHECK (amount_minor > 0),
    currency CHAR(3) NOT NULL CHECK (currency IN ('BRL', 'USD', 'EUR')),
    balance_before_minor BIGINT NOT NULL CHECK (balance_before_minor >= 0),
    balance_after_minor BIGINT NOT NULL CHECK (balance_after_minor >= 0),
    wallet_version BIGINT NOT NULL CHECK (wallet_version > 0),
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE (wallet_id, transaction_id),
    UNIQUE (wallet_id, wallet_version),
    FOREIGN KEY (transaction_id, wallet_id, currency)
        REFERENCES wager_transactions (id, wallet_id, currency),
    FOREIGN KEY (wallet_id, currency)
        REFERENCES wallets (id, currency),
    CHECK (
        (direction = 'DEBIT' AND balance_before_minor >= amount_minor
            AND balance_after_minor = balance_before_minor - amount_minor)
        OR (direction = 'CREDIT' AND balance_after_minor = balance_before_minor + amount_minor)
    )
);
CREATE INDEX wallet_ledger_entries_wallet_cursor_idx
    ON wallet_ledger_entries (wallet_id, created_at DESC, id DESC);

CREATE FUNCTION reject_ledger_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'wallet ledger entries are append-only'
        USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER wallet_ledger_append_only
BEFORE UPDATE OR DELETE ON wallet_ledger_entries
FOR EACH ROW EXECUTE FUNCTION reject_ledger_mutation();

CREATE FUNCTION validate_ledger_insert() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    tx wager_transactions%ROWTYPE;
    wallet_row wallets%ROWTYPE;
    previous_balance BIGINT;
    expected_direction TEXT;
    reference_kind TEXT;
BEGIN
    SELECT * INTO tx FROM wager_transactions WHERE id = NEW.transaction_id;
    SELECT * INTO wallet_row FROM wallets WHERE id = NEW.wallet_id;
    IF tx.id IS NULL OR wallet_row.id IS NULL
       OR tx.status <> 'PROCESSED'
       OR tx.wallet_id <> NEW.wallet_id
       OR tx.currency <> NEW.currency
       OR tx.amount_minor <> NEW.amount_minor
       OR tx.result_balance_minor <> NEW.balance_after_minor
       OR tx.updated_at <> NEW.created_at
       OR wallet_row.balance_minor <> NEW.balance_after_minor
       OR wallet_row.version <> NEW.wallet_version
       OR wallet_row.updated_at <> NEW.created_at THEN
        RAISE EXCEPTION 'ledger entry does not match processed transaction and wallet snapshot'
            USING ERRCODE = '23514';
    END IF;

    CASE tx.kind
        WHEN 'OPENING' THEN
            expected_direction := 'CREDIT';
            IF NEW.wallet_version <> 1 OR NEW.balance_before_minor <> 0 THEN
                RAISE EXCEPTION 'opening ledger entry must be the initial credit'
                    USING ERRCODE = '23514';
            END IF;
        WHEN 'BET' THEN expected_direction := 'DEBIT';
        WHEN 'WIN' THEN expected_direction := 'CREDIT';
        WHEN 'REFUND' THEN expected_direction := 'CREDIT';
        WHEN 'ROLLBACK' THEN
            SELECT kind INTO reference_kind FROM wager_transactions
                WHERE id = tx.resolved_reference_id;
            IF reference_kind IS NULL THEN
                RAISE EXCEPTION 'rollback ledger entry has no eligible reference'
                    USING ERRCODE = '23514';
            END IF;
            IF reference_kind = 'BET' THEN
                expected_direction := 'CREDIT';
            ELSE
                expected_direction := 'DEBIT';
            END IF;
        ELSE
            RAISE EXCEPTION 'transaction kind must not create a ledger entry'
                USING ERRCODE = '23514';
    END CASE;
    IF NEW.direction <> expected_direction THEN
        RAISE EXCEPTION 'ledger direction does not match transaction kind'
            USING ERRCODE = '23514';
    END IF;

    IF NEW.wallet_version > 1 THEN
        SELECT balance_after_minor INTO previous_balance
        FROM wallet_ledger_entries
        WHERE wallet_id = NEW.wallet_id AND wallet_version = NEW.wallet_version - 1;
        IF FOUND THEN
            IF previous_balance <> NEW.balance_before_minor THEN
                RAISE EXCEPTION 'ledger balance chain is discontinuous'
                    USING ERRCODE = '23514';
            END IF;
        ELSIF NEW.wallet_version <> 2 OR NEW.balance_before_minor <> 0 THEN
            RAISE EXCEPTION 'first non-opening ledger entry must start at zero balance'
                USING ERRCODE = '23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER wallet_ledger_validate_insert
BEFORE INSERT ON wallet_ledger_entries
FOR EACH ROW EXECUTE FUNCTION validate_ledger_insert();

CREATE FUNCTION assert_wallet_ledger_consistency() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    target_wallet_id UUID;
    current_balance BIGINT;
    current_version BIGINT;
    entries_count BIGINT;
    first_version BIGINT;
    last_version BIGINT;
    has_opening BOOLEAN;
    expected_count BIGINT;
BEGIN
    IF TG_TABLE_NAME = 'wallets' THEN
        target_wallet_id := NEW.id;
    ELSE
        target_wallet_id := NEW.wallet_id;
    END IF;

    SELECT balance_minor, version INTO current_balance, current_version
    FROM wallets WHERE id = target_wallet_id;
    IF NOT FOUND THEN
        RETURN NULL;
    END IF;

    SELECT count(*), COALESCE(min(entry.wallet_version), 0),
        COALESCE(max(entry.wallet_version), 0),
        COALESCE(bool_or(entry.wallet_version = 1 AND tx.kind = 'OPENING'), false)
    INTO entries_count, first_version, last_version, has_opening
    FROM wallet_ledger_entries AS entry
    JOIN wager_transactions AS tx ON tx.id = entry.transaction_id
    WHERE entry.wallet_id = target_wallet_id;

    expected_count := current_version - 1;
    IF has_opening THEN
        expected_count := expected_count + 1;
    END IF;
    IF entries_count <> expected_count THEN
        RAISE EXCEPTION 'wallet version history is missing a ledger entry'
            USING ERRCODE = '23514';
    END IF;
    IF entries_count = 0 THEN
        IF current_version <> 1 OR current_balance <> 0 THEN
            RAISE EXCEPTION 'non-zero wallet balance requires an opening ledger entry'
                USING ERRCODE = '23514';
        END IF;
        RETURN NULL;
    END IF;
    IF first_version <> (CASE WHEN has_opening THEN 1 ELSE 2 END)
       OR last_version <> current_version THEN
        RAISE EXCEPTION 'wallet ledger versions are not contiguous'
            USING ERRCODE = '23514';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM wallet_ledger_entries
        WHERE wallet_id = target_wallet_id
            AND wallet_version = current_version
            AND balance_after_minor = current_balance
    ) THEN
        RAISE EXCEPTION 'wallet balance does not match its latest ledger entry'
            USING ERRCODE = '23514';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER wallet_ledger_consistency_on_wallet
AFTER INSERT OR UPDATE ON wallets
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_wallet_ledger_consistency();

CREATE CONSTRAINT TRIGGER wallet_ledger_consistency_on_entry
AFTER INSERT ON wallet_ledger_entries
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_wallet_ledger_consistency();

CREATE TABLE wager_reversals (
    reference_transaction_id UUID PRIMARY KEY REFERENCES wager_transactions (id),
    reversal_transaction_id UUID NOT NULL UNIQUE REFERENCES wager_transactions (id),
    created_at TIMESTAMPTZ NOT NULL
);

CREATE FUNCTION reject_reversal_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'wager reversal records are immutable'
        USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER wager_reversal_append_only
BEFORE UPDATE OR DELETE ON wager_reversals
FOR EACH ROW EXECUTE FUNCTION reject_reversal_mutation();

CREATE FUNCTION validate_wager_reversal() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    ref wager_transactions%ROWTYPE;
    rev wager_transactions%ROWTYPE;
BEGIN
    SELECT * INTO ref FROM wager_transactions
        WHERE id = NEW.reference_transaction_id FOR UPDATE;
    SELECT * INTO rev FROM wager_transactions
        WHERE id = NEW.reversal_transaction_id FOR UPDATE;
    IF ref.id IS NULL OR rev.id IS NULL
       OR ref.origin <> 'EXTERNAL' OR rev.origin <> 'EXTERNAL'
       OR ref.status <> 'PROCESSED' OR rev.status <> 'PROCESSED'
       OR ref.kind NOT IN ('BET', 'WIN', 'REFUND')
       OR rev.kind NOT IN ('REFUND', 'ROLLBACK')
       OR rev.resolved_reference_id <> ref.id
       OR ref.provider_id <> rev.provider_id
       OR ref.wallet_id <> rev.wallet_id OR ref.player_id <> rev.player_id
       OR ref.currency <> rev.currency OR ref.round_id <> rev.round_id
       OR ref.amount_minor <> rev.amount_minor
       OR (rev.kind = 'REFUND' AND ref.kind <> 'BET') THEN
        RAISE EXCEPTION 'invalid processed wager reversal'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER wager_reversal_validate
BEFORE INSERT ON wager_reversals
FOR EACH ROW EXECUTE FUNCTION validate_wager_reversal();

CREATE TABLE inbox_messages (
    consumer_name TEXT NOT NULL CHECK (length(btrim(consumer_name)) > 0),
    message_id TEXT NOT NULL CHECK (length(btrim(message_id)) > 0),
    payload_hash CHAR(64) NOT NULL CHECK (payload_hash ~ '^[0-9a-f]{64}$'),
    state TEXT NOT NULL CHECK (state IN ('RECEIVED', 'COMPLETED')),
    received_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,
    PRIMARY KEY (consumer_name, message_id),
    CHECK (
        (state = 'RECEIVED' AND completed_at IS NULL)
        OR (state = 'COMPLETED' AND completed_at IS NOT NULL AND completed_at >= received_at)
    )
);
CREATE INDEX inbox_messages_received_idx ON inbox_messages (received_at)
    WHERE state = 'RECEIVED';

CREATE FUNCTION guard_inbox_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'inbox messages cannot be deleted'
            USING ERRCODE = '23514';
    END IF;
    IF ROW(NEW.consumer_name, NEW.message_id, NEW.payload_hash, NEW.received_at)
       IS DISTINCT FROM ROW(OLD.consumer_name, OLD.message_id, OLD.payload_hash, OLD.received_at)
       OR OLD.state <> 'RECEIVED' OR NEW.state <> 'COMPLETED' THEN
        RAISE EXCEPTION 'inbox message identity is immutable and completion is terminal'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER inbox_update_guard
BEFORE UPDATE OR DELETE ON inbox_messages
FOR EACH ROW EXECUTE FUNCTION guard_inbox_update();

CREATE TABLE outbox_events (
    event_id UUID PRIMARY KEY,
    aggregate_id UUID NOT NULL,
    event_type TEXT NOT NULL CHECK (length(btrim(event_type)) > 0),
    schema_version INTEGER NOT NULL CHECK (schema_version > 0),
    correlation_id TEXT NOT NULL CHECK (length(btrim(correlation_id)) > 0),
    causation_id TEXT,
    payload JSONB NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    available_at TIMESTAMPTZ NOT NULL,
    lock_owner TEXT,
    locked_until TIMESTAMPTZ,
    published_at TIMESTAMPTZ,
    last_error TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    CHECK ((lock_owner IS NULL) = (locked_until IS NULL)),
    CHECK (published_at IS NULL OR (lock_owner IS NULL AND locked_until IS NULL))
);
CREATE INDEX outbox_events_available_idx
    ON outbox_events (available_at, occurred_at, event_id)
    WHERE published_at IS NULL;

CREATE FUNCTION guard_outbox_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.event_id, NEW.aggregate_id, NEW.event_type, NEW.schema_version,
           NEW.correlation_id, NEW.causation_id, NEW.payload, NEW.occurred_at, NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.event_id, OLD.aggregate_id, OLD.event_type, OLD.schema_version,
           OLD.correlation_id, OLD.causation_id, OLD.payload, OLD.occurred_at, OLD.created_at) THEN
        RAISE EXCEPTION 'outbox event snapshot is immutable'
            USING ERRCODE = '23514';
    END IF;
    IF NEW.attempts < OLD.attempts OR NEW.attempts > OLD.attempts + 1
       OR (OLD.published_at IS NOT NULL AND NEW.published_at IS DISTINCT FROM OLD.published_at) THEN
        RAISE EXCEPTION 'invalid outbox delivery state update'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER outbox_update_guard
BEFORE UPDATE ON outbox_events
FOR EACH ROW EXECUTE FUNCTION guard_outbox_update();
