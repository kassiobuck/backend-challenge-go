# Relatório da campanha de validação

## Ambiente e evidências gerais

Campanha reexecutada em 2026-10-17 contra PostgreSQL 16.10, Keycloak 26.3.3 e
LocalStack 4.8.1 reais no ambiente local. Três processos independentes da API
foram iniciados nas portas `18181`, `18182` e `18183`; cada um reportou
readiness `ready`, com PostgreSQL e SQS em `ok`. Requisições concorrentes dos
cenários 1, 2 e 5 foram distribuídas entre esses processos. Esta execução não
usa o relatório de uma campanha anterior como evidência: testes completos e
race foram reexecutados sem cache, e os probes HTTP e de recuperação listados
abaixo foram repetidos contra serviços reais.

Os testes de PostgreSQL criam schemas temporários isolados. Os testes SQS
usaram as filas FIFO provisionadas pelo LocalStack. A integração OIDC obteve e
validou tokens assinados pelo Keycloak local.

| # | Preparação e execução | Resultado esperado | Resultado observado | Estado |
|---|---|---|---|---|
| 1 | Criada carteira de `100.00 BRL`; 50 requisições HTTP idênticas foram disparadas concorrentemente, distribuídas pelos três processos, com a mesma chave de idempotência. Também executado `TestFinancialUseCaseConcurrentDuplicateHasOneFinancialEffect` com 50 goroutines e PostgreSQL real. | Uma transação financeira, um débito de `10.00 BRL`, sem efeito adicional nos replays. | API: 1 resposta `201`, 49 respostas `200` de replay. Saldo `90.00 BRL`, duas entradas de ledger (abertura + débito), exatamente um débito. O teste concorrente PostgreSQL confirmou saldo de 9000 unidades menores e uma única movimentação. | PASS |
| 2 | Criada outra carteira de `100.00 BRL`; duas apostas distintas de `80.00 BRL` foram enviadas simultaneamente a processos diferentes. | Uma aposta processada, outra rejeitada por saldo insuficiente; saldo `20.00 BRL` e um débito. | Respostas `201` e `422`; a rejeição persistida foi `INSUFFICIENT_FUNDS`. Saldo consultado `20.00 BRL`, duas entradas de ledger e um débito. Repetido em `TestFinancialUseCaseConcurrentDistinctBetsCannotOverdrawWallet`. | PASS |
| 3 | Em teste de integração PostgreSQL, uma transação manteve bloqueada a linha da carteira A com `SELECT ... FOR UPDATE`; outra operação debitou a carteira B enquanto o bloqueio de A continuava ativo. | A carteira B deve progredir sem aguardar um lock global. | A operação de B terminou como `PROCESSED` antes da liberação do lock de A. | PASS |
| 4 | Iniciados três processos distintos do binário da aplicação, com pools, workers e listeners próprios, usando o mesmo PostgreSQL e LocalStack. | Três instâncias independentes prontas e capazes de atender operações concorrentes. | Os três endpoints `/health/ready` retornaram `ready`; a campanha dos cenários 1, 2 e 5 distribuiu chamadas entre as três instâncias. A inicialização simultânea inicialmente expôs colisão ao criar `schema_migrations`; a correção e seu teste de regressão estão descritos abaixo. | PASS |
| 5 | Uma aposta foi confirmada por HTTP; em seguida, o mesmo provider, transação externa, chave, carteira, jogador, valor e operação foram enviados à fila FIFO real do LocalStack. | O consumidor SQS deve reconhecer a mesma operação e não aplicar um segundo débito. | A inbox registrou a mensagem como `COMPLETED`; no banco havia uma transação correspondente, saldo de 9000 unidades menores, um débito e duas entradas de ledger (abertura + aposta). HTTP e SQS referenciaram a mesma transação financeira. | PASS |
| 6 | Enviada mensagem à fila real; o teste `TestLocalStackSQSConsumerRedeliveryAfterCommit` injeta falha na confirmação/deleção depois do commit, torna a mensagem novamente visível e chama o consumidor de novo. | Redelivery segura, sem segundo efeito financeiro; confirmação só após commit durável. | A segunda entrega foi reconhecida. Saldo final `90.00 BRL`, uma movimentação da aposta e uma única inbox receipt concluída. A falha foi injetada no limite commit/delete, sem fingir que a primeira tentativa não havia confirmado o banco. | PASS |
| 7 | Eventos de outbox foram enviados ao LocalStack; o teste invalidou a lease/confirmação do primeiro publisher após a aceitação pelo SQS e iniciou outro publisher. | Evento confirmado no broker deve ser recuperável após interrupção, com identidade estável e sem perda. | O segundo publisher retomou e confirmou a outbox. O consumidor de teste observou cada `eventId` esperado uma vez na fila FIFO, graças à deduplicação do broker; a entrega permanece at-least-once e consumidores downstream ainda devem deduplicar duravelmente. | PASS |
| 8 | REFUND e ROLLBACK foram processados antes de existirem suas referências BET e WIN. O serviço foi substituído por uma nova instância; depois chegaram as referências e o resolver foi executado. | As compensações ficam pendentes sem movimentação e são processadas após a referência válida chegar. | Ambas começaram em `PENDING_REFERENCE` sem ledger entry; após BET/WIN e resolução, ambas ficaram `PROCESSED`. Saldo final `100.00 BRL`, com abertura e quatro movimentações no ledger. | PASS |
| 9 | Em processo real da API, criada REFUND pendente. O processo foi interrompido e uma nova instância ficou pronta, recebeu a aposta de referência e resolveu a pendência. A suíte de integração real também cobriu transações e outbox pendentes após reinício. | Transações e eventos pendentes no banco devem sobreviver à reinicialização e ser retomados. | Na reexecução, a REFUND persistiu como `PENDING_REFERENCE` durante a interrupção, foi resolvida após a BET e terminou `PROCESSED`; saldo `100.00 BRL`, três entradas de ledger. Os testes reais de LocalStack/PostgreSQL para retomada da outbox também passaram na suíte `go test -race ./... -count=1`. | PASS |
| 10 | Obtidos tokens reais do Keycloak para provider-a. Contra API real foram feitas chamadas sem token, com token malformado e com token assinado cuja validade de 300 segundos expirou. Também executado teste HTTP real com PostgreSQL para isolamento entre providers e spoof de `providerId`. | Requisições não autenticadas/inválidas/expiradas recebem 401; dados de outro provider não são expostos e identidade não pode ser falsificada pelo corpo. | Na reexecução: sem token `401`, token malformado `401` e token expirado `401` após 303 segundos. Token provider-a foi recusado com `403` na reconciliação privilegiada; token do cliente interno autorizado obteve `200`. Teste OIDC/HTTP real: consulta de transação alheia `404`; spoof de `providerId` `403`. | PASS |
| 11 | Contra PostgreSQL real, tentadas violações de saldo não negativo, versão, moeda/FK do ledger, unicidade e imutabilidade. Tentados `UPDATE` e `DELETE` sobre lançamento financeiro. | Constraints e triggers do banco devem rejeitar as violações independentemente da validação em Go. | `TestPostgresConstraintsAndAppendOnlyLedger` passou: saldo negativo, escrita sem versão, incompatibilidades de moeda/FK e duplicidade falharam; `UPDATE` e `DELETE` do ledger também foram rejeitados. | PASS |
| 12 | Testados cancelamento/espera do consumidor, publisher e resolver via Fx; testado shutdown HTTP com liberação do listener. Também houve interrupção e reinicialização de processos da API nos cenários 9 e de outbox. | Workers devem encerrar dentro do limite configurado e recuperar trabalho durável após reinício. | Testes Fx passaram para os três workers e servidor HTTP. A recuperação de referência e outbox após parada/reinício de processo real passou. A parada manual para simular crash foi abrupta; o comportamento gracioso é validado pelos hooks Fx, não por envio de sinal de sistema operacional ao processo externo. | PASS, com limite de método |
| 13 | Após a recuperação da REFUND pendente, chamada a reconciliação HTTP real da carteira `c94702c8-b8f1-48d0-92a0-119882d950a9`, com token do cliente interno; reconciliador comparou saldo persistido e soma do ledger. | Saldo reconstruído do ledger deve coincidir com saldo armazenado. | API respondeu `consistent=true`, saldo armazenado `100.00 BRL`, calculado `100.00 BRL`, diferença `0.00 BRL` e três entradas verificadas. | PASS |

## Falha encontrada e correções

Ao iniciar três APIs simultaneamente em um schema PostgreSQL ainda sem
`schema_migrations`, duas inicializações falharam com duplicidade em
`pg_type_typname_nsp_index`. A tabela era criada antes da aquisição do advisory
lock. `MigrationRunner` agora cria a tabela dentro de uma transação protegida
pelo mesmo advisory lock usado nas migrations. O teste
`TestPostgresConcurrentMigrationStartupSerializesInitialSchemaCreation`
remove a tabela somente no schema temporário isolado e executa três runners
simultâneos; todos concluem e as três versões aparecem uma única vez.

O healthcheck original do Keycloak chamava `curl`, que não existe na imagem
local, e deixava o container como `unhealthy`. `compose.yaml` agora usa Bash e
`/dev/tcp` para consultar `/health/ready`; `docker compose config -q` passou e a
mesma sonda executada no container respondeu `HTTP/1.0 200 OK`. O container
Keycloak que já estava em execução não foi recriado: a recriação poderia
descartar alterações locais não persistidas do realm. Portanto, a configuração
corrigida está pronta para a próxima criação/recriação, mas o status de health
do container atual ainda reflete a sonda antiga. A descoberta OIDC real
retornou HTTP 200 e os testes de integração reais com Keycloak passaram.

## Comandos de validação finais

Executados nesta reexecução com PostgreSQL, LocalStack e Keycloak locais
configurados:

```text
go test ./... -count=1                PASS
go test -race ./... -count=1          PASS
go vet ./...                          PASS
docker compose config -q              PASS
```

`gofmt -l` foi executado sobre os 65 arquivos `.go` enumerados em `cmd`,
`internal` e `migrations`; não listou arquivos.

Os testes de integração de `go test` e `go test -race` foram executados com as
variáveis de banco, filas LocalStack e realm Keycloak real definidas conforme
`README.md`; os testes não foram substituídos por mocks nos cenários
PostgreSQL/LocalStack/IdP. Stubs são usados somente nos testes unitários de
lifecycle e em pontos deliberadamente injetados de falha.

## Limites observados

- A garantia de entrega SQS/outbox é at-least-once. FIFO deduplica dentro da
  janela do serviço; consumidores externos ainda precisam de inbox/idempotência
  durável.
- O teste de shutdown Fx cobre cancelamento e espera dos workers; a interrupção
  de processo usada para testar persistência foi deliberadamente abrupta, não
  um sinal graceful enviado ao processo externo.
- A imagem Keycloak já em execução não recebeu o healthcheck atualizado, pois
  isso requer recriação do container. O comando de healthcheck atualizado foi
  validado diretamente dentro do container.
