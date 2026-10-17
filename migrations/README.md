# Migrations PostgreSQL

As migrations são pares SQL numerados (`NNNNNN_nome.up.sql` e
`NNNNNN_nome.down.sql`). `embed.go` embute os pares no binário e o runner em
`internal/infrastructure/postgres` aplica cada versão em uma transação protegida
por advisory lock. A tabela `schema_migrations` registra as versões aplicadas.

O binário aplica migrations pendentes no startup antes de iniciar os serviços.
O runner também permite aplicar e reverter versões de forma explícita; `Down`
remove as versões mais recentes e é usado pelos testes de integração em schemas
temporários isolados.

A migration `000002_reference_retry_schedule` adiciona uma data durável para
próxima tentativa de resolução de referências, com índice parcial para os
workers; referências ausentes são reagendadas após cinco segundos.
`000003_reference_retry_guard` permite atualizar somente o agendamento de uma
transação pendente sem afrouxar a proteção de transições de estado e campos
financeiros.

Para executar a suite de integração contra um PostgreSQL real:

```powershell
$env:TEST_DATABASE_URL = "postgres://jungle:jungle-local-password@localhost:5432/jungle?sslmode=disable"
go test ./internal/infrastructure/postgres -run TestPostgres -count=1
```

Os testes criam e removem somente schemas `jungle_it_<hex>` próprios no banco
indicado por `TEST_DATABASE_URL`. Use uma database de teste com permissão de
`CREATE SCHEMA`; não aponte para produção.
