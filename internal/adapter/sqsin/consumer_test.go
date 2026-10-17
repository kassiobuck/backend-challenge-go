package sqsin

import (
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"go.uber.org/fx"

	"github.com/kassiobuck/backend-challenge-go/internal/config"
)

type fakeQueue struct {
	operations     []string
	sent           *sqs.SendMessageInput
	visibility     *sqs.ChangeMessageVisibilityInput
	deleteErr      error
	sendMessageErr error
	blockReceive   bool
	receiveStarted chan struct{}
	receiveOnce    sync.Once
}

func (q *fakeQueue) ReceiveMessage(ctx context.Context, _ *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	if q.receiveStarted != nil {
		q.receiveOnce.Do(func() { close(q.receiveStarted) })
	}
	if q.blockReceive {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return &sqs.ReceiveMessageOutput{}, nil
}

func (q *fakeQueue) DeleteMessage(_ context.Context, input *sqs.DeleteMessageInput, _ ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	q.operations = append(q.operations, "delete")
	if q.deleteErr != nil {
		return nil, q.deleteErr
	}
	return &sqs.DeleteMessageOutput{}, nil
}

func (q *fakeQueue) ChangeMessageVisibility(_ context.Context, input *sqs.ChangeMessageVisibilityInput, _ ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error) {
	q.operations = append(q.operations, "visibility")
	q.visibility = input
	return &sqs.ChangeMessageVisibilityOutput{}, nil
}

func (q *fakeQueue) SendMessage(_ context.Context, input *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	q.operations = append(q.operations, "send")
	q.sent = input
	return &sqs.SendMessageOutput{}, q.sendMessageErr
}

func TestInvalidMessageIsCopiedToDLQBeforeAcknowledgement(t *testing.T) {
	queue := &fakeQueue{}
	consumer := NewConsumer(nil, queue, config.Config{
		SQSQueueURL: "http://localhost/source.fifo", SQSDLQURL: "http://localhost/dlq.fifo",
	}, slog.New(slog.NewTextHandler(httptest.NewRecorder(), nil)))
	message := types.Message{
		MessageId: aws.String("message-1"), ReceiptHandle: aws.String("receipt-1"),
		Body: aws.String("{malformed"),
		Attributes: map[string]string{
			string(types.MessageSystemAttributeNameMessageGroupId): "wallet-1",
		},
	}

	consumer.handleMessage(context.Background(), message)

	if !reflect.DeepEqual(queue.operations, []string{"send", "delete"}) {
		t.Fatalf("queue operations = %v, want DLQ send then source delete", queue.operations)
	}
	if aws.ToString(queue.sent.QueueUrl) != "http://localhost/dlq.fifo" ||
		aws.ToString(queue.sent.MessageGroupId) != "wallet-1" ||
		aws.ToString(queue.sent.MessageDeduplicationId) == "" {
		t.Fatalf("DLQ request is missing FIFO routing/deduplication: %+v", queue.sent)
	}
	if aws.ToString(queue.sent.MessageBody) != "{malformed" {
		t.Fatalf("DLQ body = %q, want original payload", aws.ToString(queue.sent.MessageBody))
	}
}

func TestDLQFailureDoesNotDeleteSourceMessage(t *testing.T) {
	queue := &fakeQueue{sendMessageErr: errors.New("SQS unavailable")}
	consumer := NewConsumer(nil, queue, config.Config{
		SQSQueueURL: "http://localhost/source.fifo", SQSDLQURL: "http://localhost/dlq.fifo",
	}, slog.New(slog.NewTextHandler(httptest.NewRecorder(), nil)))

	consumer.handleMessage(context.Background(), types.Message{
		MessageId: aws.String("message-2"), ReceiptHandle: aws.String("receipt-2"), Body: aws.String("{"),
		Attributes: map[string]string{
			string(types.MessageSystemAttributeNameApproximateReceiveCount): "3",
		},
	})

	if !reflect.DeepEqual(queue.operations, []string{"send", "visibility"}) {
		t.Fatalf("queue operations = %v, want failed DLQ send and delayed retry without delete", queue.operations)
	}
	if queue.visibility == nil || queue.visibility.VisibilityTimeout <= 0 {
		t.Fatalf("visibility retry not scheduled: %+v", queue.visibility)
	}
}

func TestTransientProcessingFailureLeavesMessageUnacknowledged(t *testing.T) {
	queue := &fakeQueue{}
	consumer := NewConsumer(nil, queue, config.Config{
		SQSQueueURL: "http://localhost/source.fifo", SQSDLQURL: "http://localhost/dlq.fifo",
	}, slog.New(slog.NewTextHandler(httptest.NewRecorder(), nil)))
	consumer.handleMessage(context.Background(), types.Message{
		MessageId: aws.String("message-3"), ReceiptHandle: aws.String("receipt-3"),
		Body: aws.String(`{"providerId":"provider","externalTransactionId":"external","idempotencyKey":"key","walletId":"0192f291-27dd-7d3f-8071-5f8685deef37","playerId":"0192f291-27dd-7d3f-8071-5f8685deef38","roundId":"round","gameId":"game","kind":"BET","money":{"amount":"1.00","currency":"BRL"}}`),
		Attributes: map[string]string{
			string(types.MessageSystemAttributeNameApproximateReceiveCount): "2",
		},
	})

	if !reflect.DeepEqual(queue.operations, []string{"visibility"}) {
		t.Fatalf("queue operations = %v, want retry scheduling without acknowledgement", queue.operations)
	}
	if queue.visibility == nil {
		t.Fatal("transient failure did not schedule a retry")
	}
}

func TestConsumerFxLifecycleCancelsAndWaitsForPolling(t *testing.T) {
	queue := &fakeQueue{blockReceive: true, receiveStarted: make(chan struct{})}
	logger := slog.New(slog.NewTextHandler(httptest.NewRecorder(), nil))
	cfg := config.Config{
		SQSQueueURL:     "http://localhost/source.fifo",
		SQSDLQURL:       "http://localhost/dlq.fifo",
		ShutdownTimeout: time.Second,
	}
	consumer := NewConsumer(nil, queue, cfg, logger)
	app := fx.New(
		fx.Provide(func() *Consumer { return consumer }),
		fx.Provide(func() config.Config { return cfg }),
		fx.Provide(func() *slog.Logger { return logger }),
		fx.Invoke(RegisterConsumer),
		fx.NopLogger,
	)
	startCtx, cancelStart := context.WithTimeout(context.Background(), time.Second)
	defer cancelStart()
	if err := app.Start(startCtx); err != nil {
		t.Fatalf("start Fx consumer: %v", err)
	}
	select {
	case <-queue.receiveStarted:
	case <-startCtx.Done():
		t.Fatal("SQS consumer did not begin polling after Fx startup")
	}
	stopCtx, cancelStop := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelStop()
	if err := app.Stop(stopCtx); err != nil {
		t.Fatalf("stop Fx consumer: %v", err)
	}
}
