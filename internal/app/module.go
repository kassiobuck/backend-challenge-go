package app

import (
	"log/slog"

	sqsclient "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"github.com/kassiobuck/backend-challenge-go/internal/adapter/httpapi"
	"github.com/kassiobuck/backend-challenge-go/internal/adapter/sqsin"
	"github.com/kassiobuck/backend-challenge-go/internal/adapter/sqsout"
	"github.com/kassiobuck/backend-challenge-go/internal/application"
	"github.com/kassiobuck/backend-challenge-go/internal/application/port"
	"github.com/kassiobuck/backend-challenge-go/internal/config"
	"github.com/kassiobuck/backend-challenge-go/internal/health"
	oidcinfra "github.com/kassiobuck/backend-challenge-go/internal/infrastructure/oidc"
	"github.com/kassiobuck/backend-challenge-go/internal/infrastructure/postgres"
	sqsinfra "github.com/kassiobuck/backend-challenge-go/internal/infrastructure/sqs"
	"github.com/kassiobuck/backend-challenge-go/internal/worker"
	migrationfiles "github.com/kassiobuck/backend-challenge-go/migrations"
)

var Module = fx.Module(
	"application",
	fx.Provide(config.Load),
	fx.Provide(func() *slog.Logger { return slog.Default() }),
	fx.Provide(postgres.NewPool),
	fx.Provide(func(pool *pgxpool.Pool) *postgres.UnitOfWork {
		return postgres.NewUnitOfWork(pool)
	}),
	fx.Provide(func(unitOfWork *postgres.UnitOfWork) port.UnitOfWork {
		return unitOfWork
	}),
	fx.Provide(application.NewFinancialService),
	fx.Provide(oidcinfra.NewVerifierFromConfig),
	fx.Provide(func(verifier *oidcinfra.Verifier) port.TokenVerifier { return verifier }),
	fx.Provide(sqsinfra.NewClient),
	fx.Provide(func(pool *pgxpool.Pool) postgres.PoolLifecycle { return pool }),
	fx.Provide(func(client *sqsclient.Client) sqsinfra.QueueClient { return client }),
	fx.Provide(func(client *sqsclient.Client) sqsin.Queue { return client }),
	fx.Provide(func(client *sqsclient.Client) sqsout.Queue { return client }),
	fx.Provide(sqsin.NewConsumer),
	fx.Provide(sqsout.NewPublisher),
	fx.Provide(worker.NewReferenceResolver),
	fx.Provide(NewReadinessChecker),
	fx.Provide(func(checker *health.Checker) httpapi.ReadinessChecker { return checker }),
	fx.Provide(httpapi.NewHTTPServer),
	fx.Invoke(
		func(lifecycle fx.Lifecycle, pool *pgxpool.Pool, logger *slog.Logger, cfg config.Config) {
			postgres.RegisterMigrationLifecycle(lifecycle, pool, logger, cfg, migrationfiles.Files)
		},
		postgres.RegisterPoolLifecycle,
		sqsinfra.RegisterStartupCheck,
		sqsin.RegisterConsumer,
		sqsout.RegisterPublisher,
		worker.RegisterReferenceResolver,
		httpapi.RegisterHTTPServer,
	),
)

func NewReadinessChecker(database *pgxpool.Pool, queue *sqsclient.Client, cfg config.Config) *health.Checker {
	return health.NewChecker(
		postgres.NewProbe(database),
		sqsinfra.NewProbe(queue, cfg),
	)
}
