CREATE OR REPLACE FUNCTION guard_wager_transaction_update() RETURNS trigger
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

    IF OLD.status = 'PENDING_REFERENCE' AND NEW.status = 'PENDING_REFERENCE' THEN
        IF ROW(NEW.resolved_reference_id, NEW.failure_code, NEW.result_balance_minor, NEW.updated_at)
           IS NOT DISTINCT FROM
           ROW(OLD.resolved_reference_id, OLD.failure_code, OLD.result_balance_minor, OLD.updated_at) THEN
            RETURN NEW;
        END IF;
        RAISE EXCEPTION 'pending reference retry schedule cannot change transaction state'
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
