package sqs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	sqsclient "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"go.uber.org/fx"

	"github.com/kassiobuck/backend-challenge-go/internal/config"
	"github.com/kassiobuck/backend-challenge-go/internal/health"
)

func NewClient(cfg config.Config) (*sqsclient.Client, error) {
	sdkConfig, err := awsconfig.LoadDefaultConfig(
		context.Background(),
		awsconfig.WithRegion(cfg.AWSRegion),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			cfg.AWSAccessKeyID,
			cfg.AWSSecretKey,
			"",
		)),
	)
	if err != nil {
		return nil, fmt.Errorf("load AWS SDK configuration: %w", err)
	}

	return sqsclient.NewFromConfig(sdkConfig, func(options *sqsclient.Options) {
		options.BaseEndpoint = aws.String(cfg.AWSEndpointURL)
	}), nil
}

type QueueClient interface {
	GetQueueAttributes(context.Context, *sqsclient.GetQueueAttributesInput, ...func(*sqsclient.Options)) (*sqsclient.GetQueueAttributesOutput, error)
}

func RegisterStartupCheck(
	lifecycle fx.Lifecycle,
	client QueueClient,
	logger *slog.Logger,
	cfg config.Config,
) {
	lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			startupCtx, cancel := context.WithTimeout(ctx, cfg.StartupTimeout)
			defer cancel()
			for _, target := range configuredQueues(cfg) {
				_, err := client.GetQueueAttributes(startupCtx, &sqsclient.GetQueueAttributesInput{
					QueueUrl:       aws.String(target.url),
					AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn},
				})
				if err != nil {
					return fmt.Errorf("check SQS %s queue during startup: %w", target.name, err)
				}
			}
			logger.Info("SQS input, output, and dead-letter queues ready")
			return nil
		},
	})
}

type Probe struct {
	client    QueueClient
	queueURLs []queueTarget
	timeout   time.Duration
}

type queueTarget struct {
	name string
	url  string
}

func configuredQueues(cfg config.Config) []queueTarget {
	return []queueTarget{
		{name: "input", url: cfg.SQSQueueURL},
		{name: "output", url: cfg.SQSOutputQueueURL},
		{name: "dead-letter", url: cfg.SQSDLQURL},
	}
}

func NewProbe(client QueueClient, cfg config.Config) *Probe {
	return &Probe{
		client:    client,
		queueURLs: configuredQueues(cfg),
		timeout:   2 * time.Second,
	}
}

func (p *Probe) Name() string {
	return "sqs"
}

func (p *Probe) Check(ctx context.Context) error {
	checkCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	for _, target := range p.queueURLs {
		_, err := p.client.GetQueueAttributes(checkCtx, &sqsclient.GetQueueAttributesInput{
			QueueUrl:       aws.String(target.url),
			AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn},
		})
		if err != nil {
			return fmt.Errorf("check SQS %s queue: %w", target.name, err)
		}
	}
	return nil
}

var _ health.Probe = (*Probe)(nil)
