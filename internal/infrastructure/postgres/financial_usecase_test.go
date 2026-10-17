package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsSQS "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kassiobuck/backend-challenge-go/internal/adapter/sqsin"
	"github.com/kassiobuck/backend-challenge-go/internal/adapter/sqsout"
	"github.com/kassiobuck/backend-challenge-go/internal/application"
	"github.com/kassiobuck/backend-challenge-go/internal/config"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/money"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/wager"
	sqsinfra "github.com/kassiobuck/backend-challenge-go/internal/infrastructure/sqs"
	"github.com/kassiobuck/backend-challenge-go/internal/messaging"
)

func TestFinancialUseCaseResolvesRefundAndRollbackAfterReferencesArriveAndRestart(t *testing.T) {
	pool := integrationPool(t)
	service := application.NewFinancialService(NewUnitOfWork(pool))
	ctx := context.Background()
	opened, err := service.OpenWallet(ctx, application.OpenWalletCommand{
		PlayerID: testPlayerID, InitialBalance: mustTestMoney(t, "100.00", money.BRL),
	})
	if err != nil {
		t.Fatalf("OpenWallet(): %v", err)
	}

	refund := testProcessCommand(t, opened.Wallet.ID(),
		"early-refund", "early-refund-key", wager.Refund, "10.00")
	refund.ReferenceExternalTransactionID = "reference-bet-arrives-later"
	pendingRefund, err := service.ProcessWager(ctx, refund)
	if err != nil {
		t.Fatalf("ProcessWager(early REFUND): %v", err)
	}
	if pendingRefund.Transaction.Status() != wager.PendingReference || pendingRefund.LedgerEntry != nil {
		t.Fatalf("early REFUND result = %+v, want pending without ledger movement", pendingRefund)
	}

	rollback := testProcessCommand(t, opened.Wallet.ID(),
		"early-rollback", "early-rollback-key", wager.Rollback, "5.00")
	rollback.ReferenceExternalTransactionID = "reference-win-arrives-later"
	pendingRollback, err := service.ProcessWager(ctx, rollback)
	if err != nil {
		t.Fatalf("ProcessWager(early ROLLBACK): %v", err)
	}
	if pendingRollback.Transaction.Status() != wager.PendingReference || pendingRollback.LedgerEntry != nil {
		t.Fatalf("early ROLLBACK result = %+v, want pending without ledger movement", pendingRollback)
	}

	// A replacement service instance must recover pending work from PostgreSQL.
	service = application.NewFinancialService(NewUnitOfWork(pool))
	bet := testProcessCommand(t, opened.Wallet.ID(),
		"reference-bet-arrives-later", "reference-bet-key", wager.Bet, "10.00")
	if _, err := service.ProcessWager(ctx, bet); err != nil {
		t.Fatalf("ProcessWager(reference BET): %v", err)
	}
	win := testProcessCommand(t, opened.Wallet.ID(),
		"reference-win-arrives-later", "reference-win-key", wager.Win, "5.00")
	if _, err := service.ProcessWager(ctx, win); err != nil {
		t.Fatalf("ProcessWager(reference WIN): %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE wager_transactions SET reference_next_attempt_at = clock_timestamp()
		WHERE provider_id = $1 AND external_transaction_id IN ($2, $3)`,
		refund.ProviderID, refund.ExternalTransactionID, rollback.ExternalTransactionID); err != nil {
		t.Fatalf("make pending references eligible after service restart: %v", err)
	}

	resolved, err := service.ResolvePendingReferences(ctx, 10)
	if err != nil {
		t.Fatalf("ResolvePendingReferences(): %v", err)
	}
	if resolved != 2 {
		t.Fatalf("resolved transaction count = %d, want both REFUND and ROLLBACK", resolved)
	}
	for _, externalID := range []string{refund.ExternalTransactionID, rollback.ExternalTransactionID} {
		result, err := service.GetProviderWager(ctx, refund.ProviderID, externalID)
		if err != nil {
			t.Fatalf("GetProviderWager(%s): %v", externalID, err)
		}
		if result.Status() != wager.Processed {
			t.Errorf("transaction %s status = %s, want PROCESSED", externalID, result.Status())
		}
	}
	walletAfter, err := service.GetWallet(ctx, opened.Wallet.ID())
	if err != nil {
		t.Fatalf("GetWallet() after reference resolution: %v", err)
	}
	if walletAfter.Balance().String() != "100.00" {
		t.Fatalf("final balance = %s, want 100.00 after BET, WIN, REFUND, ROLLBACK", walletAfter.Balance())
	}
	var movements int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1`,
		opened.Wallet.ID()).Scan(&movements); err != nil {
		t.Fatal(err)
	}
	if movements != 5 {
		t.Fatalf("ledger entries = %d, want opening and four resolved financial movements", movements)
	}
}

func TestFinancialUseCaseIdempotencyAndExternalReplay(t *testing.T) {
	pool := integrationPool(t)
	service := application.NewFinancialService(NewUnitOfWork(pool))
	ctx := context.Background()
	opened, err := service.OpenWallet(ctx, application.OpenWalletCommand{
		PlayerID: testPlayerID, InitialBalance: mustTestMoney(t, "100.00", money.BRL),
	})
	if err != nil {
		t.Fatalf("OpenWallet(): %v", err)
	}

	command := testProcessCommand(t, opened.Wallet.ID(), "usecase-external-1", "usecase-key-1", wager.Bet, "10.00")
	first, err := service.ProcessWager(ctx, command)
	if err != nil {
		t.Fatalf("first ProcessWager(): %v", err)
	}
	if first.Replay || first.Transaction.Status() != wager.Processed || first.LedgerEntry == nil {
		t.Fatalf("first result = %+v, want processed transaction and ledger entry", first)
	}

	restartedService := application.NewFinancialService(NewUnitOfWork(pool))
	replayCommand := command
	replayCommand.ProviderID = " provider-usecase "
	replayCommand.ExternalTransactionID = " usecase-external-1 "
	replayCommand.IdempotencyKey = " usecase-key-1 "
	replayCommand.Kind = wager.Kind(" bet ")
	replayed, err := restartedService.ProcessWager(ctx, replayCommand)
	if err != nil {
		t.Fatalf("replay after service restart(): %v", err)
	}
	assertReplayResult(t, replayed, first.Transaction.ID(), first.LedgerEntry.ID())
	resultBalance, hasResultBalance := replayed.Transaction.ResultBalance()
	if !hasResultBalance || resultBalance.String() != "90.00" {
		t.Fatalf("replayed result balance = %s (present=%t), want original 90.00", resultBalance, hasResultBalance)
	}

	alternateKey := command
	alternateKey.IdempotencyKey = "usecase-key-2"
	if _, err := service.ProcessWager(ctx, alternateKey); !errors.Is(err, application.ErrExternalTransactionConflict) {
		t.Fatalf("same external transaction with another key error = %v, want ErrExternalTransactionConflict", err)
	}

	keyConflict := command
	keyConflict.ExternalTransactionID = "usecase-external-2"
	if _, err := service.ProcessWager(ctx, keyConflict); !errors.Is(err, application.ErrIdempotencyConflict) {
		t.Fatalf("same key with different content error = %v, want ErrIdempotencyConflict", err)
	}
	externalConflict := alternateKey
	externalConflict.Amount = mustTestMoney(t, "11.00", money.BRL)
	if _, err := service.ProcessWager(ctx, externalConflict); !errors.Is(err, application.ErrExternalTransactionConflict) {
		t.Fatalf("same external id with different content error = %v, want ErrExternalTransactionConflict", err)
	}

	var balance int64
	if err := pool.QueryRow(ctx, `SELECT balance_minor FROM wallets WHERE id = $1`, opened.Wallet.ID()).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if balance != 9000 {
		t.Fatalf("wallet balance = %d minor units, want 9000", balance)
	}
	assertUseCaseRows(t, ctx, pool, opened.Wallet.ID(), 2, 2, 4)
}

func TestSQSInboxAndFinancialEffectCommitAtomically(t *testing.T) {
	pool := integrationPool(t)
	service := application.NewFinancialService(NewUnitOfWork(pool))
	ctx := context.Background()
	opened, err := service.OpenWallet(ctx, application.OpenWalletCommand{
		PlayerID: testPlayerID, InitialBalance: mustTestMoney(t, "100.00", money.BRL),
	})
	if err != nil {
		t.Fatalf("OpenWallet(): %v", err)
	}
	command := testProcessCommand(t, opened.Wallet.ID(), "inbox-external", "inbox-key", wager.Bet, "10.00")

	first, err := service.ProcessWagerMessage(ctx, "integration-consumer", "sqs-message-1", command)
	if err != nil {
		t.Fatalf("first ProcessWagerMessage(): %v", err)
	}
	if first.Replay || first.Transaction.Status() != wager.Processed {
		t.Fatalf("first inbox result = %+v, want newly processed transaction", first)
	}
	replayed, err := service.ProcessWagerMessage(ctx, "integration-consumer", "sqs-message-1", command)
	if err != nil {
		t.Fatalf("redelivered ProcessWagerMessage(): %v", err)
	}
	if !replayed.Replay || replayed.Transaction.ID() != first.Transaction.ID() {
		t.Fatalf("redelivery result = %+v, want replay of %s", replayed, first.Transaction.ID())
	}
	assertUseCaseRows(t, ctx, pool, opened.Wallet.ID(), 2, 2, 4)

	changed := command
	changed.Amount = mustTestMoney(t, "11.00", money.BRL)
	if _, err := service.ProcessWagerMessage(ctx, "integration-consumer", "sqs-message-1", changed); !errors.Is(err, ErrInboxHashMismatch) {
		t.Fatalf("changed payload for same SQS message error = %v, want ErrInboxHashMismatch", err)
	}

	missingWallet := command
	missingWallet.WalletID = "0192f291-27dd-7d3f-8071-5f8685deef37"
	missingWallet.ExternalTransactionID = "rolled-back-inbox-message"
	missingWallet.IdempotencyKey = "rolled-back-inbox-key"
	if _, err := service.ProcessWagerMessage(ctx, "integration-consumer", "sqs-message-rollback", missingWallet); !errors.Is(err, ErrNotFound) {
		t.Fatalf("message with missing wallet error = %v, want ErrNotFound", err)
	}
	var receipts int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM inbox_messages
		WHERE consumer_name = 'integration-consumer' AND message_id = 'sqs-message-rollback'`).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if receipts != 0 {
		t.Fatalf("inbox receipts after failed financial transaction = %d, want rollback to remove receipt", receipts)
	}
}

func TestLocalStackSQSConsumerRedeliveryAfterCommit(t *testing.T) {
	inputURL := os.Getenv("TEST_SQS_INPUT_QUEUE_URL")
	dlqURL := os.Getenv("TEST_SQS_DLQ_URL")
	if inputURL == "" || dlqURL == "" {
		t.Skip("TEST_SQS_INPUT_QUEUE_URL and TEST_SQS_DLQ_URL are required for LocalStack integration")
	}
	pool := integrationPool(t)
	client := localStackTestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	service := application.NewFinancialService(NewUnitOfWork(pool))
	opened, err := service.OpenWallet(ctx, application.OpenWalletCommand{
		PlayerID: testPlayerID, InitialBalance: mustTestMoney(t, "100.00", money.BRL),
	})
	if err != nil {
		t.Fatalf("OpenWallet(): %v", err)
	}
	command := messaging.WagerCommand{
		ProviderID: "localstack-provider", ExternalTransactionID: "localstack-redelivery",
		IdempotencyKey: "localstack-redelivery-key", WalletID: opened.Wallet.ID(),
		PlayerID: testPlayerID, RoundID: "localstack-round", GameID: "localstack-game",
		Kind: wager.Bet, Money: mustTestMoney(t, "10.00", money.BRL),
	}
	body, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	sendOutput, err := client.SendMessage(ctx, &awsSQS.SendMessageInput{
		QueueUrl: aws.String(inputURL), MessageBody: aws.String(string(body)),
		MessageGroupId:         aws.String(opened.Wallet.ID()),
		MessageDeduplicationId: aws.String("localstack-" + opened.Wallet.ID()),
	})
	if err != nil {
		t.Fatalf("send LocalStack wager command: %v", err)
	}
	messageID := aws.ToString(sendOutput.MessageId)
	if messageID == "" {
		t.Fatal("LocalStack did not return a stable SQS message ID")
	}

	queue := &deleteFailOnceQueue{Queue: client}
	consumer := sqsin.NewConsumer(service, queue, config.Config{
		SQSQueueURL: inputURL, SQSDLQURL: dlqURL,
	}, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := consumer.PollOnce(ctx); err != nil {
		t.Fatalf("first LocalStack consumer poll: %v", err)
	}
	if queue.deleteCalls != 1 {
		t.Fatalf("first poll delete calls = %d, want 1", queue.deleteCalls)
	}
	if err := consumer.PollOnce(ctx); err != nil {
		t.Fatalf("redelivery LocalStack consumer poll: %v", err)
	}
	if queue.deleteCalls != 2 {
		t.Fatalf("redelivery delete calls = %d, want 2", queue.deleteCalls)
	}
	walletAfter, err := service.GetWallet(ctx, opened.Wallet.ID())
	if err != nil {
		t.Fatalf("get wallet after redelivery: %v", err)
	}
	if walletAfter.Balance().String() != "90.00" {
		t.Fatalf("wallet balance after redelivery = %s, want 90.00", walletAfter.Balance())
	}
	var ledgerEntries, completedReceipts int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1`, opened.Wallet.ID()).Scan(&ledgerEntries); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM inbox_messages
		WHERE consumer_name = 'sqs-wager-command-v1' AND message_id = $1 AND state = 'COMPLETED'`,
		messageID).Scan(&completedReceipts); err != nil {
		t.Fatal(err)
	}
	if ledgerEntries != 2 || completedReceipts != 1 {
		t.Fatalf("ledger entries/inbox receipts = %d/%d, want opening plus one wager entry and one receipt", ledgerEntries, completedReceipts)
	}
}

func TestLocalStackInvalidCommandIsDeadLetteredBeforeDelete(t *testing.T) {
	inputURL := os.Getenv("TEST_SQS_INPUT_QUEUE_URL")
	dlqURL := os.Getenv("TEST_SQS_DLQ_URL")
	if inputURL == "" || dlqURL == "" {
		t.Skip("TEST_SQS_INPUT_QUEUE_URL and TEST_SQS_DLQ_URL are required for LocalStack integration")
	}
	pool := integrationPool(t)
	client := localStackTestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	body := "{invalid-command"
	sent, err := client.SendMessage(ctx, &awsSQS.SendMessageInput{
		QueueUrl: aws.String(inputURL), MessageBody: aws.String(body),
		MessageGroupId:         aws.String("invalid-" + time.Now().UTC().Format("150405.000000000")),
		MessageDeduplicationId: aws.String(fmt.Sprintf("invalid-%d", time.Now().UnixNano())),
	})
	if err != nil {
		t.Fatalf("send invalid LocalStack message: %v", err)
	}
	if sent.MessageId == nil {
		t.Fatal("LocalStack did not return a message ID")
	}

	consumer := sqsin.NewConsumer(
		application.NewFinancialService(NewUnitOfWork(pool)),
		client,
		config.Config{SQSQueueURL: inputURL, SQSDLQURL: dlqURL},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err := consumer.PollOnce(ctx); err != nil {
		t.Fatalf("poll invalid LocalStack command: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		output, err := client.ReceiveMessage(ctx, &awsSQS.ReceiveMessageInput{
			QueueUrl: aws.String(dlqURL), MaxNumberOfMessages: 10, WaitTimeSeconds: 1,
			MessageAttributeNames: []string{"All"},
		})
		if err != nil {
			t.Fatalf("receive LocalStack DLQ message: %v", err)
		}
		for _, message := range output.Messages {
			originalID, hasOriginalID := message.MessageAttributes["OriginalMessageId"]
			if hasOriginalID && aws.ToString(originalID.StringValue) == aws.ToString(sent.MessageId) {
				reason, hasReason := message.MessageAttributes["FailureReason"]
				if !hasReason || aws.ToString(message.Body) != body ||
					aws.ToString(reason.StringValue) != "invalid_message" {
					t.Fatalf("DLQ message did not preserve body/reason: %+v", message)
				}
				if _, err := client.DeleteMessage(ctx, &awsSQS.DeleteMessageInput{
					QueueUrl: aws.String(dlqURL), ReceiptHandle: message.ReceiptHandle,
				}); err != nil {
					t.Fatalf("delete verified LocalStack DLQ message: %v", err)
				}
				return
			}
		}
	}
	t.Fatal("invalid source message was not copied to the LocalStack DLQ")
}

func TestLocalStackOutboxResendAfterPublishConfirmationInterruption(t *testing.T) {
	outputURL := os.Getenv("TEST_SQS_OUTPUT_QUEUE_URL")
	if outputURL == "" {
		t.Skip("TEST_SQS_OUTPUT_QUEUE_URL is required for LocalStack integration")
	}
	pool := integrationPool(t)
	client := localStackTestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	service := application.NewFinancialService(NewUnitOfWork(pool))
	opened, err := service.OpenWallet(ctx, application.OpenWalletCommand{
		PlayerID: testPlayerID, InitialBalance: mustTestMoney(t, "3.00", money.BRL),
	})
	if err != nil {
		t.Fatalf("OpenWallet(): %v", err)
	}
	expected := make(map[string]struct{})
	rows, err := pool.Query(ctx, `SELECT event_id::text FROM outbox_events WHERE aggregate_id = $1`, opened.Wallet.ID())
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var eventID string
		if err := rows.Scan(&eventID); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		expected[eventID] = struct{}{}
	}
	rows.Close()
	if len(expected) != 2 {
		t.Fatalf("opening outbox events = %d, want processed and balance-changed events", len(expected))
	}

	interruptedQueue := &acceptThenLoseLeaseQueue{Queue: client, pool: pool}
	interruptedPublisher, err := sqsout.NewPublisher(
		NewUnitOfWork(pool), interruptedQueue,
		config.Config{SQSOutputQueueURL: outputURL},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := interruptedPublisher.PublishBatch(ctx); err == nil {
		t.Fatal("first publish should fail to confirm after SQS accepted the event")
	}
	if len(interruptedQueue.sentIDs) != len(expected) {
		t.Fatalf("first send attempts = %d, want %d", len(interruptedQueue.sentIDs), len(expected))
	}

	restartedPublisher, err := sqsout.NewPublisher(
		NewUnitOfWork(pool), client, config.Config{SQSOutputQueueURL: outputURL},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := restartedPublisher.PublishBatch(ctx); err != nil {
		t.Fatalf("republish outbox after publisher restart: %v", err)
	}

	var published int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND published_at IS NOT NULL`,
		opened.Wallet.ID()).Scan(&published); err != nil {
		t.Fatal(err)
	}
	if published != len(expected) {
		t.Fatalf("durably marked published events = %d, want %d", published, len(expected))
	}
	received := receiveExpectedLocalStackEvents(t, ctx, client, outputURL, expected)
	for eventID := range expected {
		if received[eventID] != 1 {
			t.Errorf("LocalStack delivered event %s %d times, want once despite resend", eventID, received[eventID])
		}
	}
}

func TestOutboxSQSFailureIsDurablyRescheduled(t *testing.T) {
	pool := integrationPool(t)
	service := application.NewFinancialService(NewUnitOfWork(pool))
	ctx := context.Background()
	opened, err := service.OpenWallet(ctx, application.OpenWalletCommand{
		PlayerID: testPlayerID, InitialBalance: mustTestMoney(t, "1.00", money.BRL),
	})
	if err != nil {
		t.Fatalf("OpenWallet(): %v", err)
	}
	queue := &failingOutboxQueue{}
	publisher, err := sqsout.NewPublisher(
		NewUnitOfWork(pool), queue, config.Config{SQSOutputQueueURL: "http://localhost/events.fifo"},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.PublishBatch(ctx); err == nil {
		t.Fatal("PublishBatch() error = nil, want transient SQS failure")
	}
	if queue.calls == 0 {
		t.Fatal("publisher did not attempt SQS send")
	}
	var attempts int
	var availableAt time.Time
	var lockOwner *string
	var publishedAt *time.Time
	if err := pool.QueryRow(ctx, `
		SELECT attempts, available_at, lock_owner, published_at
		FROM outbox_events WHERE aggregate_id = $1
		ORDER BY event_id LIMIT 1`, opened.Wallet.ID()).Scan(
		&attempts, &availableAt, &lockOwner, &publishedAt); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || !availableAt.After(time.Now().UTC()) || lockOwner != nil || publishedAt != nil {
		t.Fatalf("failed publish retry state: attempts=%d available=%s owner=%v published=%v",
			attempts, availableAt, lockOwner, publishedAt)
	}
}

func TestLocalStackConcurrentOutboxPublishersDeliverAllEvents(t *testing.T) {
	outputURL := os.Getenv("TEST_SQS_OUTPUT_QUEUE_URL")
	if outputURL == "" {
		t.Skip("TEST_SQS_OUTPUT_QUEUE_URL is required for LocalStack integration")
	}
	pool := integrationPool(t)
	client := localStackTestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	service := application.NewFinancialService(NewUnitOfWork(pool))
	opened, err := service.OpenWallet(ctx, application.OpenWalletCommand{
		PlayerID: testPlayerID, InitialBalance: mustTestMoney(t, "4.00", money.BRL),
	})
	if err != nil {
		t.Fatalf("OpenWallet(): %v", err)
	}
	expected := outboxEventIDs(t, ctx, pool, opened.Wallet.ID())
	if len(expected) != 2 {
		t.Fatalf("opening outbox events = %d, want 2", len(expected))
	}
	newPublisher := func() *sqsout.Publisher {
		t.Helper()
		publisher, err := sqsout.NewPublisher(
			NewUnitOfWork(pool), client, config.Config{SQSOutputQueueURL: outputURL},
			slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		return publisher
	}
	publishers := []*sqsout.Publisher{newPublisher(), newPublisher()}
	errorsByPublisher := make([]error, len(publishers))
	var wait sync.WaitGroup
	wait.Add(len(publishers))
	for index, publisher := range publishers {
		go func(index int, publisher *sqsout.Publisher) {
			defer wait.Done()
			errorsByPublisher[index] = publisher.PublishBatch(ctx)
		}(index, publisher)
	}
	wait.Wait()
	for index, err := range errorsByPublisher {
		if err != nil {
			t.Errorf("publisher %d: %v", index, err)
		}
	}
	var published int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND published_at IS NOT NULL`,
		opened.Wallet.ID()).Scan(&published); err != nil {
		t.Fatal(err)
	}
	if published != len(expected) {
		t.Fatalf("published outbox events = %d, want %d", published, len(expected))
	}
	received := receiveExpectedLocalStackEvents(t, ctx, client, outputURL, expected)
	for eventID := range expected {
		if received[eventID] != 1 {
			t.Errorf("concurrent publishers delivered event %s %d times, want once", eventID, received[eventID])
		}
	}
}

type deleteFailOnceQueue struct {
	sqsin.Queue
	deleteCalls int
}

type failingOutboxQueue struct {
	calls int
}

func (q *failingOutboxQueue) SendMessage(
	context.Context,
	*awsSQS.SendMessageInput,
	...func(*awsSQS.Options),
) (*awsSQS.SendMessageOutput, error) {
	q.calls++
	return nil, errors.New("simulated SQS outage")
}

func (q *deleteFailOnceQueue) DeleteMessage(
	ctx context.Context,
	input *awsSQS.DeleteMessageInput,
	options ...func(*awsSQS.Options),
) (*awsSQS.DeleteMessageOutput, error) {
	q.deleteCalls++
	if q.deleteCalls == 1 {
		_, _ = q.Queue.ChangeMessageVisibility(ctx, &awsSQS.ChangeMessageVisibilityInput{
			QueueUrl: input.QueueUrl, ReceiptHandle: input.ReceiptHandle, VisibilityTimeout: 0,
		})
		return nil, errors.New("simulated crash between database commit and SQS acknowledgement")
	}
	return q.Queue.DeleteMessage(ctx, input, options...)
}

type acceptThenLoseLeaseQueue struct {
	sqsout.Queue
	pool    *pgxpool.Pool
	sentIDs []string
}

func (q *acceptThenLoseLeaseQueue) SendMessage(
	ctx context.Context,
	input *awsSQS.SendMessageInput,
	options ...func(*awsSQS.Options),
) (*awsSQS.SendMessageOutput, error) {
	output, err := q.Queue.SendMessage(ctx, input, options...)
	if err != nil {
		return output, err
	}
	eventID := aws.ToString(input.MessageDeduplicationId)
	q.sentIDs = append(q.sentIDs, eventID)
	_, err = q.pool.Exec(ctx, `
		UPDATE outbox_events
		SET lock_owner = 'simulated-stale-owner', locked_until = clock_timestamp() - interval '1 second'
		WHERE event_id = $1`, eventID)
	if err != nil {
		return nil, fmt.Errorf("simulate crash after accepted publish: %w", err)
	}
	return output, nil
}

func localStackTestClient(t *testing.T) *awsSQS.Client {
	t.Helper()
	endpoint := os.Getenv("TEST_SQS_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_SQS_ENDPOINT is required for LocalStack integration")
	}
	client, err := sqsinfra.NewClient(config.Config{
		AWSRegion: "us-east-1", AWSAccessKeyID: "test",
		AWSSecretKey: "test", AWSEndpointURL: endpoint,
	})
	if err != nil {
		t.Fatalf("create LocalStack SQS client: %v", err)
	}
	return client
}

func receiveExpectedLocalStackEvents(
	t *testing.T,
	ctx context.Context,
	client *awsSQS.Client,
	queueURL string,
	expected map[string]struct{},
) map[string]int {
	t.Helper()
	received := make(map[string]int, len(expected))
	deadline := time.Now().Add(15 * time.Second)
	for len(received) < len(expected) && time.Now().Before(deadline) {
		output, err := client.ReceiveMessage(ctx, &awsSQS.ReceiveMessageInput{
			QueueUrl: aws.String(queueURL), MaxNumberOfMessages: 10,
			WaitTimeSeconds: 1,
		})
		if err != nil {
			t.Fatalf("receive published LocalStack events: %v", err)
		}

		for _, message := range output.Messages {
			var envelope struct {
				EventID string `json:"eventId"`
			}
			if err := json.Unmarshal([]byte(aws.ToString(message.Body)), &envelope); err != nil {
				t.Errorf("decode published event body: %v", err)
			} else if _, ok := expected[envelope.EventID]; ok {
				received[envelope.EventID]++
			}
			if _, err := client.DeleteMessage(ctx, &awsSQS.DeleteMessageInput{
				QueueUrl: aws.String(queueURL), ReceiptHandle: message.ReceiptHandle,
			}); err != nil {
				t.Errorf("delete observed LocalStack event: %v", err)
			}
		}
	}
	return received
}

func outboxEventIDs(t *testing.T, ctx context.Context, pool *pgxpool.Pool, aggregateID string) map[string]struct{} {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT event_id::text FROM outbox_events WHERE aggregate_id = $1`, aggregateID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	ids := make(map[string]struct{})
	for rows.Next() {
		var eventID string
		if err := rows.Scan(&eventID); err != nil {
			t.Fatal(err)
		}
		ids[eventID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

func TestFinancialUseCaseProcessesEveryWagerKind(t *testing.T) {
	pool := integrationPool(t)
	service := application.NewFinancialService(NewUnitOfWork(pool))
	ctx := context.Background()
	opened, err := service.OpenWallet(ctx, application.OpenWalletCommand{
		PlayerID: testPlayerID, InitialBalance: mustTestMoney(t, "100.00", money.BRL),
	})
	if err != nil {
		t.Fatalf("OpenWallet(): %v", err)
	}
	submit := func(externalID, key string, kind wager.Kind, amount, reference string) *application.WagerResult {
		t.Helper()
		command := testProcessCommand(t, opened.Wallet.ID(), externalID, key, kind, amount)
		command.ReferenceExternalTransactionID = reference
		result, err := service.ProcessWager(ctx, command)
		if err != nil {
			t.Fatalf("ProcessWager(%s): %v", kind, err)
		}
		if result.Transaction.Status() != wager.Processed {
			t.Fatalf("ProcessWager(%s) status = %s, want PROCESSED", kind, result.Transaction.Status())
		}
		return result
	}

	bet := submit("all-types-bet", "all-types-key-bet", wager.Bet, "10.00", "")
	win := submit("all-types-win", "all-types-key-win", wager.Win, "5.00", "")
	loss := submit("all-types-loss", "all-types-key-loss", wager.Loss, "0.00", "")
	refund := submit("all-types-refund", "all-types-key-refund", wager.Refund, "10.00", "all-types-bet")
	rollback := submit("all-types-rollback", "all-types-key-rollback", wager.Rollback, "5.00", "all-types-win")

	if bet.LedgerEntry == nil || win.LedgerEntry == nil || loss.LedgerEntry != nil ||
		refund.LedgerEntry == nil || rollback.LedgerEntry == nil {
		t.Fatalf("ledger presence for BET/WIN/LOSS/REFUND/ROLLBACK = %t/%t/%t/%t/%t",
			bet.LedgerEntry != nil, win.LedgerEntry != nil, loss.LedgerEntry != nil,
			refund.LedgerEntry != nil, rollback.LedgerEntry != nil)
	}
	var balance, version int64
	if err := pool.QueryRow(ctx, `SELECT balance_minor, version FROM wallets WHERE id = $1`, opened.Wallet.ID()).Scan(&balance, &version); err != nil {
		t.Fatal(err)
	}
	if balance != 10000 || version != 5 {
		t.Fatalf("wallet balance/version = %d/%d, want 10000/5", balance, version)
	}
	assertUseCaseRows(t, ctx, pool, opened.Wallet.ID(), 6, 5, 11)
}

func TestFinancialUseCaseZeroOpeningAndRejectedDebitAreDurable(t *testing.T) {
	pool := integrationPool(t)
	service := application.NewFinancialService(NewUnitOfWork(pool))
	ctx := context.Background()
	zero, err := money.Zero(money.BRL)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := service.OpenWallet(ctx, application.OpenWalletCommand{
		PlayerID: testPlayerID, InitialBalance: zero,
	})
	if err != nil {
		t.Fatalf("OpenWallet(zero): %v", err)
	}
	if opened.OpeningWager != nil {
		t.Fatalf("zero-balance wallet has opening wager: %+v", opened.OpeningWager)
	}
	assertUseCaseRows(t, ctx, pool, opened.Wallet.ID(), 0, 0, 0)

	command := testProcessCommand(t, opened.Wallet.ID(), "rejected-bet", "rejected-bet-key", wager.Bet, "1.00")
	rejected, err := service.ProcessWager(ctx, command)
	if err != nil {
		t.Fatalf("ProcessWager(insufficient funds): %v", err)
	}
	if rejected.Transaction.Status() != wager.Rejected ||
		rejected.Transaction.FailureCode() != wager.FailureInsufficientFunds {
		t.Fatalf("rejection status/code = %s/%s", rejected.Transaction.Status(), rejected.Transaction.FailureCode())
	}
	replayed, err := service.ProcessWager(ctx, command)
	if err != nil {
		t.Fatalf("replay rejected transaction: %v", err)
	}
	if !replayed.Replay || replayed.Transaction.ID() != rejected.Transaction.ID() {
		t.Fatalf("rejected transaction replay = %+v", replayed)
	}
	assertUseCaseRows(t, ctx, pool, opened.Wallet.ID(), 1, 0, 1)
}

func TestFinancialUseCaseConcurrentDuplicateHasOneFinancialEffect(t *testing.T) {
	pool := integrationPool(t)
	service := application.NewFinancialService(NewUnitOfWork(pool))
	ctx := context.Background()
	opened, err := service.OpenWallet(ctx, application.OpenWalletCommand{
		PlayerID: testPlayerID, InitialBalance: mustTestMoney(t, "100.00", money.BRL),
	})
	if err != nil {
		t.Fatalf("OpenWallet(): %v", err)
	}
	command := testProcessCommand(t, opened.Wallet.ID(), "concurrent-external", "concurrent-key", wager.Bet, "10.00")

	const callers = 50
	results := make([]*application.WagerResult, callers)
	errorsByCaller := make([]error, callers)
	var wait sync.WaitGroup
	wait.Add(callers)
	for i := 0; i < callers; i++ {
		go func(index int) {
			defer wait.Done()
			results[index], errorsByCaller[index] = service.ProcessWager(ctx, command)
		}(i)
	}
	wait.Wait()
	var transactionID string
	replays := 0
	for index, result := range results {
		if errorsByCaller[index] != nil {
			t.Errorf("caller %d ProcessWager(): %v", index, errorsByCaller[index])
			continue
		}
		if result == nil || result.Transaction == nil || result.Transaction.Status() != wager.Processed {
			t.Errorf("caller %d returned invalid result: %+v", index, result)
			continue
		}
		if transactionID == "" {
			transactionID = result.Transaction.ID()
		}
		if result.Transaction.ID() != transactionID {
			t.Errorf("caller %d transaction id = %s, want %s", index, result.Transaction.ID(), transactionID)
		}
		if result.Replay {
			replays++
		}
	}
	if transactionID == "" || replays == 0 {
		t.Fatalf("transaction id = %q, replay count = %d; expected one write and replayed callers", transactionID, replays)
	}
	assertUseCaseRows(t, ctx, pool, opened.Wallet.ID(), 2, 2, 4)
	var balance int64
	if err := pool.QueryRow(ctx, `SELECT balance_minor FROM wallets WHERE id = $1`, opened.Wallet.ID()).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if balance != 9000 {
		t.Fatalf("wallet balance = %d minor units, want one debit (9000)", balance)
	}
}

func TestFinancialUseCaseConcurrentDistinctBetsCannotOverdrawWallet(t *testing.T) {
	pool := integrationPool(t)
	service := application.NewFinancialService(NewUnitOfWork(pool))
	ctx := context.Background()
	opened, err := service.OpenWallet(ctx, application.OpenWalletCommand{
		PlayerID: testPlayerID, InitialBalance: mustTestMoney(t, "100.00", money.BRL),
	})
	if err != nil {
		t.Fatalf("OpenWallet(): %v", err)
	}

	commands := []application.ProcessWagerCommand{
		testProcessCommand(t, opened.Wallet.ID(), "concurrent-bet-a", "concurrent-bet-key-a", wager.Bet, "80.00"),
		testProcessCommand(t, opened.Wallet.ID(), "concurrent-bet-b", "concurrent-bet-key-b", wager.Bet, "80.00"),
	}
	results := make([]*application.WagerResult, len(commands))
	errorsByCaller := make([]error, len(commands))
	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(len(commands))
	for index, command := range commands {
		go func(index int, command application.ProcessWagerCommand) {
			defer wait.Done()
			<-start
			results[index], errorsByCaller[index] = service.ProcessWager(ctx, command)
		}(index, command)
	}
	close(start)
	wait.Wait()

	processed, rejected := 0, 0
	for index, result := range results {
		if errorsByCaller[index] != nil {
			t.Errorf("caller %d ProcessWager(): %v", index, errorsByCaller[index])
			continue
		}
		if result == nil || result.Transaction == nil {
			t.Errorf("caller %d returned no transaction: %+v", index, result)
			continue
		}
		switch result.Transaction.Status() {
		case wager.Processed:
			processed++
		case wager.Rejected:
			if result.Transaction.FailureCode() != wager.FailureInsufficientFunds {
				t.Errorf("rejected transaction failure code = %q, want insufficient funds", result.Transaction.FailureCode())
			}
			rejected++
		default:
			t.Errorf("caller %d transaction status = %s, want processed or rejected", index, result.Transaction.Status())
		}
	}
	if processed != 1 || rejected != 1 {
		t.Fatalf("processed/rejected transactions = %d/%d, want 1/1", processed, rejected)
	}

	var balance int64
	if err := pool.QueryRow(ctx, `SELECT balance_minor FROM wallets WHERE id = $1`, opened.Wallet.ID()).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	var debits int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM wallet_ledger_entries
		WHERE wallet_id = $1 AND direction = 'DEBIT'`,
		opened.Wallet.ID()).Scan(&debits); err != nil {
		t.Fatal(err)
	}
	if balance != 2000 || debits != 1 {
		t.Fatalf("balance/debits = %d/%d minor units, want 2000/1", balance, debits)
	}
	assertUseCaseRows(t, ctx, pool, opened.Wallet.ID(), 3, 2, 5)
}

func TestFinancialUseCaseIndependentWalletProgressesWhileAnotherIsLocked(t *testing.T) {
	pool := integrationPool(t)
	service := application.NewFinancialService(NewUnitOfWork(pool))
	ctx := context.Background()
	lockedWallet, err := service.OpenWallet(ctx, application.OpenWalletCommand{
		PlayerID: testPlayerID, InitialBalance: mustTestMoney(t, "100.00", money.BRL),
	})
	if err != nil {
		t.Fatalf("OpenWallet(locked): %v", err)
	}
	const independentPlayerID = "0192f299-27dd-7d3f-8071-5f8685deef38"
	independentWallet, err := service.OpenWallet(ctx, application.OpenWalletCommand{
		PlayerID:       independentPlayerID,
		InitialBalance: mustTestMoney(t, "100.00", money.BRL),
	})
	if err != nil {
		t.Fatalf("OpenWallet(independent): %v", err)
	}

	lockTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin wallet lock transaction: %v", err)
	}
	defer func() { _ = lockTx.Rollback(context.Background()) }()
	if err := lockTx.QueryRow(ctx, `SELECT id FROM wallets WHERE id = $1 FOR UPDATE`,
		lockedWallet.Wallet.ID()).Scan(new(string)); err != nil {
		t.Fatalf("lock first wallet: %v", err)
	}

	command := testProcessCommand(t, independentWallet.Wallet.ID(),
		"independent-wallet-progress", "independent-wallet-key", wager.Bet, "10.00")
	command.PlayerID = independentPlayerID
	processCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	resultCh := make(chan *application.WagerResult, 1)
	errorCh := make(chan error, 1)
	go func() {
		result, err := service.ProcessWager(processCtx, command)
		resultCh <- result
		errorCh <- err
	}()

	select {
	case result := <-resultCh:
		if err := <-errorCh; err != nil {
			t.Fatalf("ProcessWager on independent wallet: %v", err)
		}
		if result == nil || result.Transaction.Status() != wager.Processed {
			t.Fatalf("independent wallet result = %+v, want processed", result)
		}
	case <-processCtx.Done():
		t.Fatalf("independent wallet operation blocked while another wallet row was locked: %v", processCtx.Err())
	}
}

func TestFinancialUseCaseOutboxFailureRollsBackFinancialWrites(t *testing.T) {
	pool := integrationPool(t)
	uow := NewUnitOfWork(pool)
	ctx := context.Background()
	opened, err := application.NewFinancialService(uow).OpenWallet(ctx, application.OpenWalletCommand{
		PlayerID: testPlayerID, InitialBalance: mustTestMoney(t, "100.00", money.BRL),
	})
	if err != nil {
		t.Fatalf("OpenWallet(): %v", err)
	}
	command := testProcessCommand(t, opened.Wallet.ID(), "rollback-external", "rollback-key", wager.Bet, "10.00")
	var ids atomic.Int32
	injected := errors.New("injected event id failure")
	failingService := application.NewFinancialServiceWith(uow, func() (string, error) {
		next := ids.Add(1)
		if next == 4 {
			return "", injected
		}
		return fmt.Sprintf("11111111-1111-4111-8111-%012x", next), nil
	}, func() time.Time { return time.Now().UTC() })
	if _, err := failingService.ProcessWager(ctx, command); !errors.Is(err, injected) {
		t.Fatalf("ProcessWager() error = %v, want injected failure", err)
	}
	assertUseCaseRows(t, ctx, pool, opened.Wallet.ID(), 1, 1, 2)
	var balance int64
	if err := pool.QueryRow(ctx, `SELECT balance_minor FROM wallets WHERE id = $1`, opened.Wallet.ID()).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if balance != 10000 {
		t.Fatalf("wallet balance after rollback = %d, want 10000", balance)
	}
	recovered, err := application.NewFinancialService(uow).ProcessWager(ctx, command)
	if err != nil {
		t.Fatalf("retry after rolled-back operation: %v", err)
	}
	if recovered.Transaction.Status() != wager.Processed || recovered.Replay {
		t.Fatalf("retry result = %+v, want newly processed transaction", recovered)
	}
	assertUseCaseRows(t, ctx, pool, opened.Wallet.ID(), 2, 2, 4)
}

func TestFinancialUseCaseResolvesDurablePendingReference(t *testing.T) {
	pool := integrationPool(t)
	service := application.NewFinancialService(NewUnitOfWork(pool))
	ctx := context.Background()
	opened, err := service.OpenWallet(ctx, application.OpenWalletCommand{
		PlayerID: testPlayerID, InitialBalance: mustTestMoney(t, "100.00", money.BRL),
	})
	if err != nil {
		t.Fatalf("OpenWallet(): %v", err)
	}
	refund := testProcessCommand(t, opened.Wallet.ID(), "pending-refund", "pending-refund-key", wager.Refund, "10.00")
	refund.ReferenceExternalTransactionID = "later-bet"
	pending, err := service.ProcessWager(ctx, refund)
	if err != nil {
		t.Fatalf("ProcessWager(REFUND without reference): %v", err)
	}
	if pending.Transaction.Status() != wager.PendingReference {
		t.Fatalf("pending status = %s, want PENDING_REFERENCE", pending.Transaction.Status())
	}
	if resolved, err := service.ResolvePendingReferences(ctx, 10); err != nil || resolved != 0 {
		t.Fatalf("ResolvePendingReferences(with missing reference) = %d, %v; want no resolution", resolved, err)
	}
	if resolved, err := service.ResolvePendingReferences(ctx, 10); err != nil || resolved != 0 {
		t.Fatalf("immediate ResolvePendingReferences retry = %d, %v; want scheduled pending check", resolved, err)
	}

	bet := testProcessCommand(t, opened.Wallet.ID(), "later-bet", "later-bet-key", wager.Bet, "10.00")
	if _, err := service.ProcessWager(ctx, bet); err != nil {
		t.Fatalf("ProcessWager(BET): %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE wager_transactions SET reference_next_attempt_at = clock_timestamp()
		WHERE provider_id = $1 AND external_transaction_id = $2`,
		refund.ProviderID, refund.ExternalTransactionID); err != nil {
		t.Fatalf("make pending reference eligible for resolver: %v", err)
	}
	resolved, err := service.ResolvePendingReferences(ctx, 10)
	if err != nil {
		t.Fatalf("ResolvePendingReferences(): %v", err)
	}
	if resolved != 1 {
		t.Fatalf("resolved count = %d, want 1", resolved)
	}
	var status, resolvedReference string
	if err := pool.QueryRow(ctx, `
		SELECT status, resolved_reference_id::text FROM wager_transactions
		WHERE provider_id = $1 AND external_transaction_id = $2`,
		refund.ProviderID, refund.ExternalTransactionID).Scan(&status, &resolvedReference); err != nil {
		t.Fatal(err)
	}
	if status != string(wager.Processed) || resolvedReference == "" {
		t.Fatalf("persisted pending wager status/reference = %s/%q", status, resolvedReference)
	}
	var balance int64
	if err := pool.QueryRow(ctx, `SELECT balance_minor FROM wallets WHERE id = $1`, opened.Wallet.ID()).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if balance != 10000 {
		t.Fatalf("wallet balance after bet and refund = %d, want 10000", balance)
	}
}

func testProcessCommand(t *testing.T, walletID, externalID, key string, kind wager.Kind, amount string) application.ProcessWagerCommand {
	t.Helper()
	return application.ProcessWagerCommand{
		ProviderID: "provider-usecase", ExternalTransactionID: externalID,
		IdempotencyKey: key, WalletID: walletID, PlayerID: testPlayerID,
		RoundID: "round-usecase", GameID: "game-usecase", Kind: kind,
		Amount: mustTestMoney(t, amount, money.BRL),
	}
}

func assertReplayResult(t *testing.T, result *application.WagerResult, transactionID, ledgerID string) {
	t.Helper()
	if result == nil || !result.Replay || result.Transaction == nil ||
		result.Transaction.ID() != transactionID || result.LedgerEntry == nil ||
		result.LedgerEntry.ID() != ledgerID {
		t.Fatalf("replay result = %+v; expected transaction %s and ledger %s", result, transactionID, ledgerID)
	}
}

func assertUseCaseRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, walletID string, wagers, ledgerEntries, outboxEvents int) {
	t.Helper()
	for _, check := range []struct {
		table string
		want  int
	}{
		{table: "wager_transactions", want: wagers},
		{table: "wallet_ledger_entries", want: ledgerEntries},
		{table: "outbox_events", want: outboxEvents},
	} {
		var got int
		query := `SELECT count(*) FROM ` + check.table
		if check.table == "wager_transactions" {
			query += ` WHERE wallet_id = $1`
		} else if check.table == "wallet_ledger_entries" {
			query += ` WHERE wallet_id = $1`
		} else {
			query += ` WHERE aggregate_id = $1`
		}
		if err := pool.QueryRow(ctx, query, walletID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != check.want {
			t.Errorf("%s row count = %d, want %d", check.table, got, check.want)
		}
	}
}
