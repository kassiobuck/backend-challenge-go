package sqsout

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"go.uber.org/fx"

	"github.com/kassiobuck/backend-challenge-go/internal/application/port"
	"github.com/kassiobuck/backend-challenge-go/internal/config"
	"github.com/kassiobuck/backend-challenge-go/internal/messaging"
)

type Queue interface {
	SendMessage(context.Context, *sqs.SendMessageInput, ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
}

type Publisher struct {
	unitOfWork port.UnitOfWork
	queue      Queue
	queueURL   string
	logger     *slog.Logger
	owner      string
	lease      time.Duration
	interval   time.Duration
}

func NewPublisher(
	unitOfWork port.UnitOfWork,
	queue Queue,
	cfg config.Config,
	logger *slog.Logger,
) (*Publisher, error) {
	if logger == nil {
		logger = slog.Default()
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return nil, fmt.Errorf("generate outbox publisher identity: %w", err)
	}
	return &Publisher{
		unitOfWork: unitOfWork, queue: queue, queueURL: cfg.SQSOutputQueueURL,
		logger: logger, owner: "outbox-" + hex.EncodeToString(random),
		lease: 2 * time.Minute, interval: time.Second,
	}, nil
}

func (p *Publisher) Run(ctx context.Context) {
	failures := 0
	for ctx.Err() == nil {
		err := p.PublishBatch(ctx)
		delay := p.interval
		if err != nil && ctx.Err() == nil {
			failures++
			p.logger.Error("publish outbox batch failed", "error", err)
			delay = messaging.Backoff(failures, time.Second, time.Minute)
		} else {
			failures = 0
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (p *Publisher) PublishBatch(ctx context.Context) error {
	var events []port.OutboxEvent
	err := p.unitOfWork.Within(ctx, func(ctx context.Context, repos port.Repositories) error {
		var err error
		events, err = repos.Outbox().ClaimBatch(ctx, p.owner, time.Now().UTC(), p.lease, 10)
		return err
	})
	if err != nil {
		return fmt.Errorf("claim outbox events: %w", err)
	}

	var failures []error
	for _, event := range events {
		if err := p.publishOne(ctx, event); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (p *Publisher) publishOne(ctx context.Context, event port.OutboxEvent) error {
	if strings.TrimSpace(event.EventID) == "" ||
		strings.TrimSpace(event.AggregateID) == "" ||
		len(event.Payload) == 0 {
		err := errors.New("claimed outbox event has missing event ID, aggregate ID, or payload")
		return p.reschedule(ctx, event, err)
	}
	_, err := p.queue.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl: aws.String(p.queueURL), MessageBody: aws.String(string(event.Payload)),
		MessageGroupId:         aws.String(event.AggregateID),
		MessageDeduplicationId: aws.String(event.EventID),
		MessageAttributes: map[string]types.MessageAttributeValue{
			"EventId":   {DataType: aws.String("String"), StringValue: aws.String(event.EventID)},
			"EventType": {DataType: aws.String("String"), StringValue: aws.String(event.EventType)},
			"SchemaVersion": {
				DataType:    aws.String("Number"),
				StringValue: aws.String(fmt.Sprintf("%d", event.SchemaVersion)),
			},
		},
	})
	if err != nil {
		return p.reschedule(ctx, event, fmt.Errorf("send event to SQS: %w", err))
	}

	publishedAt := time.Now().UTC()
	if err := p.unitOfWork.Within(ctx, func(ctx context.Context, repos port.Repositories) error {
		return repos.Outbox().MarkPublished(ctx, event.EventID, event.LockOwner, publishedAt)
	}); err != nil {
		return fmt.Errorf("mark outbox event %s published after SQS acceptance: %w", event.EventID, err)
	}
	p.logger.Info("outbox event published", "event_id", event.EventID, "event_type", event.EventType)
	return nil
}

func (p *Publisher) reschedule(ctx context.Context, event port.OutboxEvent, cause error) error {
	retryAt := time.Now().UTC().Add(messaging.Backoff(event.Attempts, time.Second, 5*time.Minute))
	failure := cause.Error()
	if len(failure) > 1024 {
		failure = failure[:1024]
	}
	if err := p.unitOfWork.Within(ctx, func(ctx context.Context, repos port.Repositories) error {
		return repos.Outbox().Reschedule(ctx, event.EventID, event.LockOwner, failure, retryAt)
	}); err != nil {
		return errors.Join(cause, fmt.Errorf("reschedule outbox event %s: %w", event.EventID, err))
	}
	p.logger.Warn("outbox event scheduled for retry",
		"event_id", event.EventID, "attempt", event.Attempts, "retry_at", retryAt, "error", cause)
	return cause
}

func RegisterPublisher(lifecycle fx.Lifecycle, publisher *Publisher, cfg config.Config, logger *slog.Logger) {
	var cancel context.CancelFunc
	var done chan struct{}
	lifecycle.Append(fx.Hook{
		OnStart: func(context.Context) error {
			workerCtx, cancelFn := context.WithCancel(context.Background())
			cancel = cancelFn
			done = make(chan struct{})
			go func() {
				defer close(done)
				publisher.Run(workerCtx)
			}()
			logger.Info("SQS outbox publisher started")
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
				return fmt.Errorf("stop outbox publisher within %s: %w", cfg.ShutdownTimeout, stopCtx.Err())
			}
		},
	})
}

var _ Queue = (*sqs.Client)(nil)
