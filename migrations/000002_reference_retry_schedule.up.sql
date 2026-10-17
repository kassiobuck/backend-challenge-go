ALTER TABLE wager_transactions
    ADD COLUMN reference_next_attempt_at timestamptz;

UPDATE wager_transactions
SET reference_next_attempt_at = clock_timestamp()
WHERE status = 'PENDING_REFERENCE';

ALTER TABLE wager_transactions
    ADD CONSTRAINT wager_reference_retry_schedule_ck
    CHECK (
        (status = 'PENDING_REFERENCE' AND reference_next_attempt_at IS NOT NULL)
        OR (status <> 'PENDING_REFERENCE' AND reference_next_attempt_at IS NULL)
    );

CREATE INDEX wager_transactions_pending_reference_schedule_idx
    ON wager_transactions (reference_next_attempt_at, created_at, id)
    WHERE status = 'PENDING_REFERENCE';
