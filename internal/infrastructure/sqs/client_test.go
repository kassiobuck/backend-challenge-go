package sqs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	sqsclient "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"go.uber.org/fx"

	"github.com/kassiobuck/backend-challenge-go/internal/config"
)

type queueClientStub struct {
	queueURLs []string
	calls     int
	err       error
}

func (q *queueClientStub) GetQueueAttributes(
	_ context.Context,
	input *sqsclient.GetQueueAttributesInput,
	_ ...func(*sqsclient.Options),
) (*sqsclient.GetQueueAttributesOutput, error) {
	q.calls++
	if input.QueueUrl != nil {
		q.queueURLs = append(q.queueURLs, *input.QueueUrl)
	}
	if q.err != nil {
		return nil, q.err
	}
	return &sqsclient.GetQueueAttributesOutput{
		Attributes: map[string]string{string(types.QueueAttributeNameQueueArn): "arn:aws:sqs:local:000000000000:queue.fifo"},
	}, nil
}

func TestProbeChecksInputOutputAndDeadLetterQueues(t *testing.T) {
	queue := &queueClientStub{}
	probe := NewProbe(queue, config.Config{
		SQSQueueURL:       "http://localhost/input.fifo",
		SQSOutputQueueURL: "http://localhost/events.fifo",
		SQSDLQURL:         "http://localhost/dlq.fifo",
	})
	if err := probe.Check(context.Background()); err != nil {
		t.Fatalf("Probe.Check() error = %v", err)
	}
	wantURLs := []string{
		"http://localhost/input.fifo",
		"http://localhost/events.fifo",
		"http://localhost/dlq.fifo",
	}
	if queue.calls != len(wantURLs) {
		t.Fatalf("probe calls = %d, want %d", queue.calls, len(wantURLs))
	}
	for index, got := range queue.queueURLs {
		if got != wantURLs[index] {
			t.Fatalf("probe queue URLs = %v, want %v", queue.queueURLs, wantURLs)
		}
	}
}

func TestStartupCheckRequiresConfiguredQueue(t *testing.T) {
	tests := []struct {
		name     string
		queueErr error
		wantErr  bool
	}{
		{name: "queue exists"},
		{name: "queue unavailable", queueErr: errors.New("queue not found"), wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			queue := &queueClientStub{err: test.queueErr}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			cfg := config.Config{
				SQSQueueURL:       "http://localhost:4566/000000000000/queue.fifo",
				SQSOutputQueueURL: "http://localhost:4566/000000000000/events.fifo",
				SQSDLQURL:         "http://localhost:4566/000000000000/dlq.fifo",
				StartupTimeout:    time.Second,
			}
			app := fx.New(
				fx.Provide(func() QueueClient { return queue }),
				fx.Supply(logger, cfg),
				fx.Invoke(RegisterStartupCheck),
				fx.NopLogger,
			)

			startErr := app.Start(context.Background())
			if (startErr != nil) != test.wantErr {
				_ = app.Stop(context.Background())
				t.Fatalf("app.Start() error = %v, wantErr %t", startErr, test.wantErr)
			}
			if !test.wantErr {
				if err := app.Stop(context.Background()); err != nil {
					t.Fatalf("app.Stop() error = %v", err)
				}
			}
			wantCalls := 3
			if test.wantErr {
				wantCalls = 1
			}
			if queue.calls != wantCalls {
				t.Fatalf("queue check calls = %d, want %d", queue.calls, wantCalls)
			}
			wantURLs := []string{cfg.SQSQueueURL, cfg.SQSOutputQueueURL, cfg.SQSDLQURL}
			for index, queueURL := range queue.queueURLs {
				if queueURL != wantURLs[index] {
					t.Fatalf("checked queue URLs = %v, want %v", queue.queueURLs, wantURLs)
				}
			}
		})
	}
}
