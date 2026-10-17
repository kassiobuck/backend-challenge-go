DROP INDEX wager_transactions_pending_reference_schedule_idx;

ALTER TABLE wager_transactions
    DROP CONSTRAINT wager_reference_retry_schedule_ck;

ALTER TABLE wager_transactions
    DROP COLUMN reference_next_attempt_at;
