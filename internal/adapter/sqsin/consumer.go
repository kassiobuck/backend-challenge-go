package sqsin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"go.uber.org/fx"

	"github.com/kassiobuck/backend-challenge-go/internal/application"
	"github.com/kassiobuck/backend-challenge-go/internal/application/port"
	"github.com/kassiobuck/backend-challenge-go/internal/config"
	"github.com/kassiobuck/backend-challenge-go/internal/domain"
	"github.com/kassiobuck/backend-challenge-go/internal/messaging"
)

const wagerConsumerName = "sqs-wager-command-v1"

type Queue interface {
	ReceiveMessage(context.Context, *sqs.ReceiveMessageInput, ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(context.Context, *sqs.DeleteMessageInput, ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
	ChangeMessageVisibility(context.Context, *sqs.ChangeMessageVisibilityInput, ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error)
	SendMessage(context.Context, *sqs.SendMessageInput, ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
}

type Consumer struct {
	service     *application.FinancialService
	queue       Queue
	queueURL    string
	dlqURL      string
	logger      *slog.Logger
	visibility  int32
	pollBase    time.Duration
	pollMaximum time.Duration
}

func NewConsumer(service *application.FinancialService, queue Queue, cfg config.Config, logger *slog.Logger) *Consumer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Consumer{
		service: service, queue: queue, queueURL: cfg.SQSQueueURL, dlqURL: cfg.SQSDLQURL,
		logger: logger, visibility: 30, pollBase: 250 * time.Millisecond, pollMaximum: 5 * time.Second,
	}
}

func (c *Consumer) Run(ctx context.Context) {
	failures := 0
	for ctx.Err() == nil {
		err := c.PollOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			failures = 0
			continue
		}
		failures++
		c.logger.Error("SQS receive failed", "error", err, "attempt", failures)
		if !sleep(ctx, messaging.Backoff(failures, c.pollBase, c.pollMaximum)) {
			return
		}
	}
}

func (c *Consumer) PollOnce(ctx context.Context) error {
	output, err := c.queue.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl: aws.String(c.queueURL), MaxNumberOfMessages: 10,
		WaitTimeSeconds: 20, VisibilityTimeout: c.visibility,
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{
			types.MessageSystemAttributeNameApproximateReceiveCount,
			types.MessageSystemAttributeNameMessageGroupId,
		},
	})
	if err != nil {
		return fmt.Errorf("receive wager commands: %w", err)
	}
	for _, message := range output.Messages {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.handleMessage(ctx, message)
	}
	return nil
}

func (c *Consumer) handleMessage(ctx context.Context, message types.Message) {
	messageID := aws.ToString(message.MessageId)
	if messageID == "" {
		messageID = bodyDigest(aws.ToString(message.Body))
	}
	command, err := messaging.DecodeWagerCommand([]byte(aws.ToString(message.Body)))
	if err != nil {
		c.deadLetter(ctx, message, messageID, "invalid_message", err)
		return
	}
	_, err = c.service.ProcessWagerMessage(ctx, wagerConsumerName, messageID, application.ProcessWagerCommand{
		ProviderID: command.ProviderID, ExternalTransactionID: command.ExternalTransactionID,
		IdempotencyKey: command.IdempotencyKey, WalletID: command.WalletID,
		PlayerID: command.PlayerID, RoundID: command.RoundID, GameID: command.GameID,
		Kind: command.Kind, Amount: command.Money,
		ReferenceExternalTransactionID: command.ReferenceExternalTransactionID,
		CorrelationID:                  command.CorrelationID,
	})
	if err != nil {
		if isPermanent(err) {
			c.deadLetter(ctx, message, messageID, "invalid_or_conflicting_command", err)
			return
		}
		c.retryLater(ctx, message, err)
		return
	}
	if _, err := c.queue.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl: aws.String(c.queueURL), ReceiptHandle: message.ReceiptHandle,
	}); err != nil {
		c.logger.Error("delete committed SQS wager message failed; redelivery is safe",
			"message_id", messageID, "error", err)
		c.retryLater(ctx, message, fmt.Errorf("delete committed SQS message: %w", err))
		return
	}
	c.logger.Info("SQS wager command committed and acknowledged", "message_id", messageID)
}

func (c *Consumer) deadLetter(ctx context.Context, message types.Message, messageID, reason string, cause error) {
	groupID := message.Attributes[string(types.MessageSystemAttributeNameMessageGroupId)]
	if groupID == "" {
		groupID = "invalid-message"
	}
	deduplicationID := bodyDigest(messageID + "\n" + aws.ToString(message.Body))
	_, err := c.queue.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl: aws.String(c.dlqURL), MessageBody: message.Body,
		MessageGroupId: aws.String(groupID), MessageDeduplicationId: aws.String(deduplicationID),
		MessageAttributes: map[string]types.MessageAttributeValue{
			"OriginalMessageId": {DataType: aws.String("String"), StringValue: aws.String(messageID)},
			"FailureReason":     {DataType: aws.String("String"), StringValue: aws.String(reason)},
		},
	})
	if err != nil {
		c.logger.Error("move invalid SQS message to DLQ failed",
			"message_id", messageID, "cause", cause, "error", err)
		c.retryLater(ctx, message, fmt.Errorf("send invalid message to DLQ: %w", err))
		return
	}
	if _, err := c.queue.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl: aws.String(c.queueURL), ReceiptHandle: message.ReceiptHandle,
	}); err != nil {
		c.logger.Error("delete dead-lettered SQS message failed",
			"message_id", messageID, "error", err)
		c.retryLater(ctx, message, fmt.Errorf("delete dead-lettered SQS message: %w", err))
		return
	}
	c.logger.Warn("invalid SQS wager message moved to DLQ",
		"message_id", messageID, "reason", reason, "error", cause)
}

func (c *Consumer) retryLater(ctx context.Context, message types.Message, cause error) {
	attempt := 1
	if raw := message.Attributes[string(types.MessageSystemAttributeNameApproximateReceiveCount)]; raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			attempt = parsed
		}
	}
	delay := messaging.Backoff(attempt, time.Second, 5*time.Minute)
	seconds := int32((delay + time.Second - 1) / time.Second)
	if _, err := c.queue.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl: aws.String(c.queueURL), ReceiptHandle: message.ReceiptHandle,
		VisibilityTimeout: seconds,
	}); err != nil {
		c.logger.Error("schedule SQS wager retry failed; broker visibility timeout remains active",
			"message_id", aws.ToString(message.MessageId), "error", err, "cause", cause)
		return
	}
	c.logger.Warn("SQS wager command will be retried",
		"message_id", aws.ToString(message.MessageId), "attempt", attempt,
		"retry_after", delay, "error", cause)
}

func isPermanent(err error) bool {
	return errors.Is(err, application.ErrIdempotencyConflict) ||
		errors.Is(err, application.ErrExternalTransactionConflict) ||
		errors.Is(err, application.ErrWalletOwnership) ||
		errors.Is(err, port.ErrInboxHashMismatch) ||
		errors.Is(err, port.ErrNotFound) ||
		errors.Is(err, port.ErrConflict) ||
		errors.Is(err, port.ErrConstraintViolation) ||
		errors.Is(err, domain.ErrInvalidID) ||
		errors.Is(err, domain.ErrInvalidWallet) ||
		errors.Is(err, domain.ErrInvalidMoney) ||
		errors.Is(err, domain.ErrInvalidCurrency) ||
		errors.Is(err, domain.ErrUnsupportedCurrency) ||
		errors.Is(err, domain.ErrInvalidWager) ||
		errors.Is(err, domain.ErrInvalidWagerKind) ||
		errors.Is(err, domain.ErrReferenceRequired) ||
		errors.Is(err, domain.ErrCurrencyMismatch) ||
		errors.Is(err, domain.ErrInvalidTimestamp)
}

func bodyDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func sleep(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func RegisterConsumer(lifecycle fx.Lifecycle, consumer *Consumer, cfg config.Config, logger *slog.Logger) {
	var cancel context.CancelFunc
	var done chan struct{}
	lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			workerCtx, cancelFn := context.WithCancel(context.Background())
			done = make(chan struct{})
			cancel = cancelFn
			go func() {
				defer close(done)
				consumer.Run(workerCtx)
			}()
			logger.Info("SQS wager consumer started")
			return nil
		},
		OnStop: func(ctx context.Context) error {
			if cancel == nil {
				return nil
			}
			cancel()
			stopCtx, stopCancel := context.WithTimeout(ctx, cfg.ShutdownTimeout)
			defer stopCancel()
			select {
			case <-done:
				return nil
			case <-stopCtx.Done():
				return fmt.Errorf("stop SQS consumer within %s: %w", cfg.ShutdownTimeout, stopCtx.Err())
			}
		},
	})
}

var _ Queue = (*sqs.Client)(nil)
