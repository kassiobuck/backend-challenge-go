# Arquitetura implementada e auditoria final

## 1. Escopo e situação

Este documento registra a arquitetura implementada e a confronta com o desafio descrito no
[README do desafio](https://github.com/kassiobuck/backend-challenge-go/blob/bdd9e70b65db9b374ee070bd3a4768661a888997/README.md).
O código contém bootstrap Fx, domínio financeiro, ports da aplicação,
adaptadores pgx/HTTP/OIDC/SQS, migrations, health checks e workers de inbox,
outbox e referências. A auditoria não toma etapas anteriores como prova: as
afirmações abaixo se referem ao código, testes e
[campanha de validação](./VALIDATION_REPORT.md).

**Conclusão:** não declarar a entrega aprovada integralmente. Embora as
garantias monetárias centrais e os cenários concorrentes documentados tenham
evidências, permanecem gaps explícitos:

1. Nenhuma métrica é exportada; a reconciliação só registra divergências em log
   e resposta.
2. Pendências de referência são reagendadas a cada cinco segundos sem máximo de
   tentativas/TTL nem rejeição após expiração.
3. SQS aceita um comando JSON plano, não o envelope
   `{messageId,type,occurredAt,data}` do desafio; a inbox usa `MessageId` do
   broker, não o `messageId` do envelope.
4. `MigrationRunner.Down` é coberto por teste, mas não existe comando CLI para
   rollback operacional.
5. O shutdown gracioso é coberto pelos hooks/testes Fx, mas a campanha não
   enviou SIGTERM a um processo externo real.
6. Não há política IAM de least privilege provisionada para AWS; no Compose,
   as credenciais do LocalStack são apenas valores de desenvolvimento.

### Convenções: requisito e decisão

- **Requisito explícito** identifica comportamento ou tecnologia exigidos pelo
  desafio. Esses requisitos não são opcionais nesta proposta.
- **Decisão implementada** registra a alternativa escolhida no código.
- **Fora do escopo inicial** identifica uma opção permitida, mas não necessária
  para satisfazer os critérios obrigatórios.

## 2. Objetivos e invariantes arquiteturais

1. HTTP e SQS chamam o mesmo caso de uso e a mesma política de idempotência.
2. PostgreSQL é a autoridade para unicidade, concorrência financeira,
   idempotência, ledger, inbox e outbox.
3. Cada operação financeira efetiva é atômica no banco: estado da transação,
   saldo/versão, ledger, inbox quando aplicável e eventos de outbox são
   confirmados juntos.
4. Não se publica evento antes do commit que o originou.
5. O domínio não importa HTTP, SQS, PostgreSQL, Fx ou bibliotecas de infraestrutura.
6. Nenhum lock em memória ou cache local é garantia de consistência.
7. Entrada externa inválida não é normalizada silenciosamente nem arredondada.
8. Qualquer estado pendente confirmado tem um caminho persistente de retomada.

## 3. Limites e organização de pacotes

### 3.1 Limites

| Camada | Responsabilidade | Pode depender de |
| --- | --- | --- |
| Domínio | Regras monetárias, entidades, invariantes, transições e erros de negócio | Biblioteca padrão Go e outros pacotes de domínio |
| Aplicação | Casos de uso, coordenação de repositórios, transações, autorização contextual e contratos de portas | Domínio e interfaces de aplicação |
| Infraestrutura | PostgreSQL, migrations, SQS, OIDC/JWKS, logs, métricas e configuração de adaptadores | Aplicação, domínio e bibliotecas externas |
| Interfaces externas | HTTP/JSON, consumidor SQS, publisher SQS e mapeamento de protocolo | Aplicação e DTOs próprios da interface |
| Composição | Construção e ciclo de vida com Fx | Todas as implementações concretas; não é importada pelo domínio |

O sentido das dependências aponta para dentro: adaptadores traduzem protocolos
externos para contratos da aplicação; a aplicação não conhece os protocolos.
Tipos de request/response HTTP não são tipos de domínio.

### 3.2 Organização real dos pacotes

```text
cmd/
  api/
    main.go                    # Fx.New(...).Run()
internal/
  adapter/
    httpapi/                    # rotas, OIDC middleware, DTOs e erros HTTP
    sqsin/                      # decodificação de comando e consumer/inbox
    sqsout/                     # publisher da outbox
  app/                          # composição Uber Fx
  application/
    financial.go                # abertura, processamento, idempotência e referências
    queries.go                  # consultas e reconciliação
    port/                       # interfaces de aplicação e repositórios
  config/                       # leitura e validação do ambiente
  domain/
    money/ wallet/ wager/       # value object, agregado e máquina de estados
    ledger/ event/ identity/    # lançamentos, eventos e identificadores
  health/                       # liveness, readiness e probes
  infrastructure/
    postgres/                   # pgx, UnitOfWork, repositórios e migrations
    sqs/                        # AWS SDK e probes de fila
    oidc/                       # validação JWT/JWKS e claims
  messaging/                    # contratos tipados e backoff
  worker/                       # resolução de referências
migrations/
  *.up.sql / *.down.sql         # pares versionados embutidos no binário
deploy/
  keycloak/                     # realm e clientes locais
  localstack/                   # imagem e hook de provisionamento SQS
compose.yaml
```

`internal/` impede que detalhes da aplicação sejam importados por outros módulos
Go. Os workers pertencem à aplicação de execução, não ao domínio. `adapter/sqsout`
define a publicação no destino; `infrastructure/sqs` contém a construção do
cliente e detalhes AWS comuns. Evitar criar interfaces sem consumidor concreto:
as portas abaixo existem porque a aplicação precisa persistir ou publicar por
adaptadores intercambiáveis.

## 4. Modelo de domínio

### 4.1 Identificadores e tempo

**Decisão implementada:** IDs internos são UUIDs gerados pela aplicação e
validados como UUIDs; timestamps são instantes UTC. No código, IDs são strings
validadas, não tipos fortes distintos para cada entidade.

IDs fornecidos por terceiros (`providerId`, `externalTransactionId`,
`Idempotency-Key`) são strings opacas sujeitas a tamanho e charset documentados;
não são convertidos silenciosamente para IDs internos.

### 4.2 `Money` e `Currency`

**Requisito explícito:** precisão exata, escala fixa de duas casas, representação
serializada como decimal string e moeda ISO 4217; nunca usar ponto flutuante.

**Decisão implementada:** armazenar `Money` como `int64` em unidades mínimas
(centavos) e um `Currency` validado. `Money` tem campos não exportados, sem
métodos mutadores, e valor zero da linguagem Go é inválido. Toda construção passa
por um construtor que valida moeda e unidade.

- O domínio aceita BRL, USD e EUR, moedas ISO 4217 com duas casas decimais,
  para validar incompatibilidade de moeda. Os cenários financeiros principais
  continuam em BRL; outras moedas exigem suporte explícito antes de serem
  aceitas por interfaces externas.
- `Parse` exige exatamente dois dígitos fracionários, dígitos ASCII e formato
  decimal simples (`-?[0-9]+\\.[0-9]{2}` para uso interno). Não aceita espaços,
  separador de milhar, notação científica, `NaN` ou `Infinity`.
- O parser externo rejeita negativos; o parser interno pode produzir valores
  negativos necessários para diferenças. Valores de entrada financeira externa
  são validados por regra específica do tipo de operação.
- `String`/JSON produzem duas casas e moeda, por exemplo
  `{"amount":"25.00","currency":"BRL"}`. A serialização nunca passa por `float`.
- Soma, subtração e negação verificam overflow antes da operação. Parsing
  verifica o limite antes de multiplicar unidades inteiras por 100 e somar
  centavos. `MinInt64` não pode ser negado. Qualquer overflow retorna erro
  classificável, não satura nem faz wrap.
- Comparação e aritmética exigem moedas iguais; moeda incompatível retorna
  `ErrCurrencyMismatch`.
- O máximo representável é `math.MaxInt64` centavos positivos e
  `math.MinInt64` negativos para cálculos internos. Limites menores podem ser
  impostos por regra de negócio/configuração, mas devem ser documentados e
  testados.
- No PostgreSQL, persistir centavos como `BIGINT` e moeda em coluna `CHAR(3)` ou
  `VARCHAR(3)`, com checks. A API retorna string decimal.

`Money` deve oferecer construtores/parsers, `Zero(currency)`, `Add`, `Sub`,
`Negate`, `Compare`, `IsZero`, `IsPositive` e serialização. Operações retornam
erro quando a entrada é inválida, a moeda diverge ou há overflow.

### 4.3 Agregado `Wallet`

`Wallet` é a raiz do agregado financeiro e contém:

- `id`, `playerId`, `currency`;
- `balance: Money`, `version`;
- `createdAt`, `updatedAt`.

Campos mutáveis permanecem encapsulados. Construtores separados:

- `Open(...)`: valida identidade, moeda e saldo inicial não negativo; versão
  inicial sempre `1`;
- `Rehydrate(...)`: valida os valores persistidos sem reaplicar crédito,
  transições ou eventos.

`Debit(money)` exige valor positivo, moeda igual e saldo suficiente.
`Credit(money)` exige valor positivo e moeda igual. Ambos validam overflow e
alteram saldo/version no agregado; retornam a mudança contendo saldo anterior,
posterior e nova versão para a aplicação persistir saldo e lançamento juntos.
Saldo zero inicial é válido. Uma criação de carteira, mesmo com saldo zero,
começa com versão `1`; saldo inicial zero não gera `OPENING`, ledger nem eventos
financeiros.

Unicidade `(playerId, currency)` é também constraint do banco. O agregado não
pode ser usado como substituto dessa garantia entre processos.

### 4.4 `WagerTransaction`

Campos persistidos para operações externas:

`transactionId`, `providerId`, `externalTransactionId`, `idempotencyKey`,
`payloadHash`, `walletId`, `playerId`, `roundId`, `gameId`, `kind`, `money`,
`referenceExternalTransactionId`, referência interna resolvida quando
aplicável, `status`, `failureCode`, resultado financeiro original e timestamps.

Operações internas `OPENING` compartilham o registro financeiro e estado, mas não
têm campos externos aplicáveis; schema e constraints devem distinguir as duas
origens. A identidade de uma abertura é interna e estável. Não é permitido
receber `OPENING` por HTTP ou SQS.

#### Máquina de estados implementada

```text
                 +----------------------+
                 |                      v
PENDING ------> PROCESSED (terminal)  PENDING_REFERENCE
   |             REJECTED (terminal)      |   |   |
   +-----------> FAILED   (terminal)       +---+---+--> PROCESSED / REJECTED / FAILED
```

Transições permitidas:

| Origem | Destino | Condição |
| --- | --- | --- |
| Nova transação | `PENDING` | Estado inicial validado |
| `PENDING` | `PROCESSED` | Regras concluídas e persistidas com o efeito financeiro |
| `PENDING` | `REJECTED` | Regra de negócio terminal recusou a operação |
| `PENDING` | `PENDING_REFERENCE` | Operação depende de referência ainda indisponível |
| `PENDING` | `FAILED` | Falha permanente de infraestrutura explicitamente registrada |
| `PENDING_REFERENCE` | `PROCESSED` | Referência elegível foi resolvida e operação aplicada |
| `PENDING_REFERENCE` | `REJECTED` | Referência inelegível ou regra de negócio recusou |
| `PENDING_REFERENCE` | `FAILED` | Falha permanente registrada conforme política de falhas |

`PROCESSED`, `REJECTED` e `FAILED` são terminais. Falha transitória de
infraestrutura não avança estado: a transação SQL é revertida e o caller/worker
repete a operação. `FAILED` é reservado a falha permanente registrada, não a
indisponibilidade temporária nem a rejeição de negócio.

Embora o domínio aceite a transição de `PENDING_REFERENCE` para `REJECTED`, o
worker atual não expira referências ausentes: reagenda a consulta a cada cinco
segundos sem contador ou TTL. Portanto, a linha da máquina de estados descreve
uma transição possível do domínio, não uma garantia implementada de expiração.

`WagerTransaction.Process` recebe os saldos anterior e posterior, valida a
equação contra direção e valor da operação (inclusive saldo inalterado para
`LOSS`) e só então conclui a transição. O caso de uso deve fornecer a mudança
real produzida pela carteira; o ledger persiste a mesma equação dentro da
transação SQL.

**Interpretação importante:** o desafio descreve transição “para processamento”,
mas enumera `PENDING`, `PENDING_REFERENCE`, `PROCESSED`, `REJECTED` e `FAILED`,
sem estado `PROCESSING`. A proposta não adiciona status de domínio
`PROCESSING`. Operações sem dependências são concluídas na mesma transação SQL;
workers reivindicam linhas com `FOR UPDATE SKIP LOCKED` e processam antes de
liberar a transação. Se futuramente uma operação exigir claim durável entre
transações, deve-se revisitar esta decisão antes de adicionar estado ou lease de
processamento ao modelo.

`PENDING` confirmado fora da transação da operação só pode ser habilitado com
um worker durável que o retome após lease expirar/reinício. O fluxo inicial evita
commit intermediário de aceite para operações independentes. `PENDING_REFERENCE`
é a pendência assíncrona persistida exigida pelo desafio.

### 4.5 `WalletLedgerEntry`

Lançamento imutável:

`id`, `walletId`, `transactionId`, `direction` (`DEBIT`/`CREDIT`), `money`,
`balanceBefore`, `balanceAfter`, `createdAt`.

Construtor valida valor positivo, moedas iguais e a equação de saldo
correspondente à direção. `LOSS` e rejeições não geram lançamento. O banco impõe
unicidade `(walletId, transactionId)`, chaves estrangeiras, checks e proteção
contra `UPDATE`/`DELETE` por privilégios da role e trigger de imutabilidade.
Escrita de ledger é somente por inserção e dentro da transação financeira.

### 4.6 Eventos

Domínio/aplicação produzem valores de evento tipados; o domínio não publica
mensagens. O contrato exigido pelo desafio descreve o envelope:

`eventId`, `eventType`, `aggregateId`, `correlationId`, `causationId` opcional,
`occurredAt` UTC RFC 3339, `version`, `data` tipado. A implementação serializa
`schemaVersion` em vez de `version` e sempre inclui `causationId`.

O payload `WalletBalanceChanged` contém `walletId`, `transactionId`, `direction`,
`money`, `balanceBefore`, `balanceAfter`, `walletVersion`. No JSON real o valor
é serializado como `amount`, não `money`; isso é um desvio de contrato ainda
não validado com consumidores. Evento é snapshot imutável; retry não reconstrói
seu payload a partir do estado atual.

Eventos e gatilhos:

| Evento | Gatilho |
| --- | --- |
| `WagerTransactionProcessed` | Toda operação processada, inclusive `LOSS` |
| `WagerTransactionRejected` | Rejeição definitiva de negócio |
| `WalletBalanceChanged` | Mudança efetiva de saldo |
| `WagerTransactionPendingReference` | Persistência da espera por referência |

Um evento de abertura positiva emite `WagerTransactionProcessed` e
`WalletBalanceChanged` no commit da carteira; abertura com saldo zero não gera
eventos financeiros.

## 5. Contratos da aplicação e erros

### 5.1 Portas de persistência

Interfaces ficam em `internal/application/port`, porque são necessidades da
aplicação. Repositórios oferecem operações de domínio, não CRUD genérico.

```go
type UnitOfWork interface {
    Within(ctx context.Context, fn func(Repositories) error) error
}

type Repositories interface {
    Wallets() WalletRepository
    Wagers() WagerRepository
    Ledger() LedgerRepository
    Inbox() InboxRepository
    Outbox() OutboxRepository
}
```

O `Repositories` recebido no callback está vinculado à mesma transação SQL; não
pode escapar do callback. `Within` faz rollback se callback, contexto ou commit
falhar e devolve erro observável. Interfaces consultivas de health check,
publisher de evento e relógio/gerador de IDs são separadas apenas quando há um
consumidor real que justifique a porta.

Contratos essenciais:

- `WalletRepository`: `FindByID`, `FindByPlayerCurrency`, `Create`,
  `GetForUpdate`, `SaveBalance(expectedVersion, balance, newVersion)`.
  `SaveBalance` deve verificar versão e número de linhas afetadas.
- `WagerRepository`: buscar por ID, `(providerId, externalTransactionId)` e
  `(providerId, idempotencyKey)`; inserir de forma concorrente segura; atualizar
  estado/result snapshot sob transação. Uma colisão não pode ser interpretada
  sem reler e comparar o hash.
- `LedgerRepository`: inserir lançamento e consultar páginas ordenadas por
  cursor estável; não expõe update/delete.
- `InboxRepository`: registrar `(consumerName, messageId, payloadHash)`, detectar
  replay/hash divergente e marcar conclusão durável.
- `OutboxRepository`: acrescentar snapshot de evento e reivindicar registros
  vencidos/pending com claim concorrente.
- `UnitOfWork`: única fronteira para operações que precisam de atomicidade entre
  os repositórios.

Leituras que exigem snapshot consistente (reconciliação) executam em uma
transação/read snapshot explícito. Métodos de escrita não podem abrir transações
ocultas fora do `UnitOfWork`.

### 5.2 Casos de uso e contratos

DTOs externos não entram no domínio. A aplicação recebe comandos tipados e
retorna resultados tipados:

| Caso de uso | Entrada | Saída/efeito |
| --- | --- | --- |
| `OpenWallet` | `playerId`, `initialBalance`, principal interno | Wallet criada; abertura/ledger/outbox somente para saldo positivo |
| `ProcessWager` | dados de negócio, `idempotencyKey`, origem e contexto de correlação | `transactionId`, status, saldo do processamento/replay, `idempotentReplay`, `failureCode` quando terminal |
| `GetWallet` | `walletId`, principal autorizado | Snapshot da carteira |
| `ListWalletLedger` | `walletId`, cursor opaco opcional, limite | Página ordenada estável e próximo cursor |
| `GetWagerTransaction` | `transactionId`, principal autorizado | Status, código de falha e resultado persistido |
| `GetProviderWagerTransaction` | provider autenticado, external transaction ID | Resultado visível somente ao próprio provider |
| `ReconcileWallet` | `walletId`, principal interno | Saldo armazenado, saldo calculado, diferença, consistência e quantidade de lançamentos |

`ProcessWager` é único para HTTP e SQS. Origem e metadados de transporte ficam
fora do hash de negócio. Uma operação financeira terminal repetida retorna o
resultado persistido; uma colisão incompatível retorna conflito.

Status HTTP propostos: `201` para abertura; `200` para operação processada ou
replay; `202` para pendência por referência; `400` para JSON/campos/formato
inválidos; `401`/`403` para autenticação/autorização; `404` para recurso não
visível/encontrado conforme política anti-enumeração; `409` para conflito de
idempotência, transação externa ou carteira duplicada; `422` para rejeição de
negócio terminal; `503` para dependência/falha transitória. Corpos de erro
contêm `code` estável e mensagem segura; detalhes financeiros não são incluídos
em erros não autorizados. O README final deverá fixar exemplos e regras de
exposição.

### 5.3 Erros classificáveis

Erros de domínio são sentinelas ou tipos próprios, comparáveis com
`errors.Is`/`errors.As`; não dependem de texto de erro.

| Código/classe proposta | Significado | Persistência/resultado |
| --- | --- | --- |
| `ErrInvalidMoney`, `ErrInvalidCurrency` | Valor, escala ou moeda inválida | Rejeitar entrada; sem efeito financeiro |
| `ErrCurrencyMismatch` | Moedas incompatíveis | Rejeição de domínio |
| `ErrOverflow` | Overflow no parsing ou cálculo exato | Rejeitar; nunca truncar |
| `ErrInsufficientFunds` | Aposta excede saldo | `REJECTED`, `INSUFFICIENT_FUNDS` |
| `ErrReversalInsufficientFunds` | Reversão exigiria saldo negativo | `REJECTED`, código próprio |
| `ErrInvalidTransition` | Transição de estado não permitida | Erro de domínio; não alterar terminal |
| `ErrReferenceNotEligible` | Referência não processada/incompatível | `REJECTED`, código documentado |
| `ErrReferencePending` | Referência ainda não disponível/final | `PENDING_REFERENCE` |
| `ErrReversalAlreadyApplied` | Reversão duplicada/incompatível | `REJECTED`, código documentado |
| `ErrIdempotencyConflict` | Mesma chave com hash diferente | HTTP `409`; sem novo efeito |
| `ErrExternalTransactionConflict` | Mesmo ID externo com chave/conteúdo divergente | HTTP `409`; sem novo efeito |
| `ErrWalletAlreadyExists` | `(playerId,currency)` duplicado | HTTP `409` |
| `ErrNotFound` | Recurso inexistente ou fora do escopo visível | HTTP `404` conforme política |
| Erro transitório de infraestrutura | DB/SQS/timeout indisponível | Rollback/retry; HTTP `503` ou retry SQS |
| Erro permanente de infraestrutura | Falha não recuperável conforme política | `FAILED` quando registrada; caso contrário DLQ |

`failureCode` persistido é enum/string estável, separado da mensagem localizada.
Erros internos preservam causa com `%w` e não são convertidos em rejeição de
negócio por fallback silencioso.

## 6. Estratégia transacional, concorrência e idempotência

### 6.1 Uma transação SQL por operação

PostgreSQL é a fonte de verdade. A unidade de trabalho contém, conforme o fluxo:

1. Inserção ou resolução concorrente da identidade idempotente.
2. Lock da carteira alvo por `SELECT ... FOR UPDATE`, se houver efeito nela.
3. Resolução/lock da referência e verificação de reversões duplicadas.
4. Validação e transição de domínio.
5. Atualização condicional de saldo/versão.
6. Inserção do ledger, quando houver movimento.
7. Persistência do resultado original da transação.
8. Registro/conclusão da inbox, para entrada SQS.
9. Inserção dos snapshots de evento na outbox.
10. Commit único.

Qualquer falha antes do commit reverte todos os passos. Não se faz chamada de
rede para SQS nem IdP dentro da transação SQL. Deadlocks/conflitos serializáveis,
se ocorrerem, são classificados como retry limitado; não são ignorados.

### 6.2 Coordenação por carteira

**Decisão implementada:** locking pessimista por linha de carteira via
`SELECT ... FOR UPDATE`, seguido de atualização de saldo condicionada pela
versão atual (`WHERE id = ? AND version = ?`) e constraints de não negatividade.
Isso localiza contenção na carteira que está sendo alterada; carteiras distintas
podem progredir em paralelo.

Ordem de lock definida para operações financeiras: identidade/idempotência
concorrente; carteira; referência da transação; gravações dependentes. Todas as
rotinas devem respeitar a mesma ordem. A unique constraint da referência de
reversão é a garantia final contra duplicidade, mesmo em corrida entre processos.

### 6.3 Idempotência

Constraints do banco:

- unique `(provider_id, idempotency_key)`;
- unique `(provider_id, external_transaction_id)`;
- unique `(wallet_id, transaction_id)` no ledger;
- unique `(consumer_name, message_id)` na inbox;
- unique de uma reversão bem-sucedida por `reference_transaction_id` na tabela
  de reversões; a tabela só recebe inserção quando a operação é concluída.

O hash é SHA-256 de JSON canônico UTF-8 contendo os campos de negócio em ordem
estável: `providerId`, `externalTransactionId`, `playerId`, `walletId`,
`roundId`, `gameId`, `kind`, `money.amount`, `money.currency` e
`referenceExternalTransactionId` quando presente. Campos opcionais ausentes são
representados de modo canônico consistente; moeda é uppercase; `amount` mantém
duas casas exatas. `idempotencyKey`, `messageId`, timestamp do envelope e
correlation/transport metadata ficam excluídos. HTTP e SQS chamam o mesmo
canonicalizador.

Com uma inserção concorrente, a aplicação consulta as duas identidades únicas:

- chave e hash iguais: devolve status e snapshot de resultado persistidos;
- chave igual e hash diferente: conflito;
- ID externo igual com outra chave: conflito, sem reaplicação;
- dois conflitos simultâneos inconsistentes: erro de conflito explícito, nunca
  escolher um resultado arbitrariamente.

O resultado original inclui o saldo observado ao concluir a operação. Consultar
outra vez a carteira atual não substitui esse snapshot durante replay.

### 6.4 Política de reversões implementada

`REFUND` e `ROLLBACK` são operações integrais alternativas sobre uma mesma
operação elegível; no máximo
uma delas pode ser bem-sucedida para a referência original. Esta opção evita que
uma aposta receba devolução e depois crédito compensatório duplicado. Uma tabela
`wager_reversals` registra somente reversões concluídas, com
`reference_transaction_id` como chave única e `reversal_transaction_id` único.
Esta é a política efetivamente codificada e coberta por constraints/testes.

`REFUND` referencia `BET`; `ROLLBACK` referencia `BET`, `WIN` ou `REFUND`
processada. Valor, provedor, jogador, carteira, moeda e rodada precisam ser
iguais. A operação da referência deve estar em estado elegível. Uma referência
inexistente é `PENDING_REFERENCE`; referência terminal inelegível é rejeitada.
Reversão que não possa debitar sem saldo negativo é rejeitada com código
distinto da aposta insuficiente.

## 7. Modelo PostgreSQL: ledger, inbox, outbox e referências

### 7.1 Tabelas conceituais

| Tabela | Papel e garantias principais |
| --- | --- |
| `wallets` | Identidade, jogador, moeda, saldo em centavos, versão, timestamps; unique `(player_id,currency)`, `balance >= 0`, versão positiva |
| `wager_transactions` | Operações internas/externas, chaves, hash, estado, referência, código de falha e resultado original; constraints por origem, status e IDs externos |
| `wallet_ledger_entries` | Registro append-only, valor em centavos, direção, saldos antes/depois e versão resultante da carteira; unique `(wallet_id,transaction_id)`/`(wallet_id,wallet_version)` e proteção DB contra update/delete |
| `inbox_messages` | `consumer_name`, `message_id`, hash, recebimento e conclusão; unique `(consumer_name,message_id)` |
| `outbox_events` | `event_id`, aggregate/type, payload snapshot, ocorrência, attempts, availability, lease e published timestamp |
| `wager_reversals` | Reversões bem-sucedidas; `reference_transaction_id` único impõe a exclusividade proposta entre `REFUND` e `ROLLBACK`, `reversal_transaction_id` também é único |

Referências são resolvidas por `(provider_id, reference_external_transaction_id)`.
Índices devem acompanhar as consultas de transação externa, chave de
idempotência, estados pendentes, wallet/cursor e outbox disponível. Migrations
up/down são versionadas. Checks, FKs, índices únicos, triggers/permissões e
transações são parte da garantia, não apenas validações Go.

**Limite transacional PostgreSQL:** o `UnitOfWork` abre uma única `pgx.Tx` por
operação e fornece nela os repositórios de carteira, wager, ledger, idempotência,
inbox, outbox e reversões. Commit ocorre somente após o callback e a verificação
do contexto; qualquer erro cancela a unidade toda. Uma operação financeira
deve, nessa fronteira, gravar estado de transação, saldo/versionamento, ledger,
reserva de reversão, inbox e snapshots de outbox, conforme aplicável. Não há
chamada de rede dentro dessa transação.

Para saldo concorrente, a ordem é primeiro resolver a identidade
idempotente, depois `WalletRepository.LockByID` (`SELECT ... FOR UPDATE`),
depois resolver/lockar a transação de referência e por fim persistir o saldo com
`SaveBalance(expectedVersion, ...)`. A atualização por versão é uma segunda
barreira otimista; zero linhas atualizadas vira conflito, não sucesso. Todos os
processos usam PostgreSQL, sem lock local como garantia. A disputa de outbox usa
uma CTE com `FOR UPDATE SKIP LOCKED`, lease persistido e marcação condicionada
ao owner; claims e publicação ficam separados para que SQS nunca seja chamado
antes do commit da reserva.

Cada `ClaimBatch` gera um token aleatório por lote, distinto do identificador
estável do worker. `MarkPublished` e `Reschedule` recebem o token devolvido no
evento reclamado, impedindo que uma tentativa antiga do mesmo worker confirme
uma lease posterior.

O runner aplica cada arquivo `.up.sql` integralmente em transação, registra a
versão no mesmo commit e serializa instâncias com advisory lock. A tabela de
versões armazena checksum SHA-256 do par up/down para detectar alteração de
migration já aplicada. `.down.sql` reverte a versão mais recente dentro de
transação equivalente. O ledger tem
trigger de rejeição para `UPDATE`/`DELETE`; estado/identidade da requisição
financeira de uma transação também não pode ser reescrito depois de inserido.
Cada lançamento inclui a versão resultante da carteira. Triggers imediatos
validam seu vínculo com transação processada, direção, valor, saldo e versão;
constraint triggers adiados verificam no commit a continuidade das versões e
que o saldo da carteira coincide com o lançamento mais recente. Carteira
positiva exige `OPENING` e lançamento de abertura no mesmo commit; carteira
inicial de saldo zero não exige lançamento.

O cursor do ledger é opaco para HTTP e codifica um par ordenável estável
(`created_at`, `id`); paginação usa ordenação determinística descendente e
consulta keyset, não `OFFSET`.

### 7.2 Inbox SQS

Inbox é escrita na mesma transação da operação, ledger e outbox. O consumidor
atual recebe um `WagerCommand` JSON plano e estrito, não o envelope tipado do
desafio. Sua chave inbox é `MessageId` do SQS; o hash é calculado sobre o comando
financeiro canônico compartilhado com HTTP. Receber o mesmo ID de SQS concluído
com hash igual é reentrega: não repete efeito e permite ACK. Mesmo ID com hash
diferente é violação de identidade: não se processa como nova operação e a
mensagem é copiada para DLQ antes de ser removida da fila original. O
`messageId` fornecido no envelope do desafio não é validado nem usado.

Uma referência ausente pode ser concluída na inbox quando `PENDING_REFERENCE`
foi persistido; worker de referências passa a ser responsável pela continuidade.
A mensagem só é removida da fila depois do commit da transação que torna durável
o resultado ou a pendência.

### 7.3 Outbox e entrega

O worker seleciona lotes com `FOR UPDATE SKIP LOCKED`, reivindica com lease e
confirma essa reivindicação numa transação curta. Fora da transação, publica o
snapshot no destino; depois marca publicação. Falhas atualizam tentativas e
próximo horário com backoff exponencial limitado. Leases expirados permitem
retomada por outra instância. A publicação SQS e a confirmação SQL não podem ser
atômicas: uma queda entre elas pode enviar novamente o evento, sempre com o
mesmo `eventId`; não se marca como publicado sem aceitação do broker.

**Destino implementado:** fila SQS FIFO `wager-events.fifo`, provisionada localmente
junto das filas de entrada. `MessageGroupId` é `aggregateId` (wallet ID para
eventos financeiros da carteira); `MessageDeduplicationId` é `eventId`. A
deduplicação temporal do FIFO não é garantia de exactly-once. Se ocorrer crash
depois da publicação e antes de marcar outbox, o evento pode reaparecer com o
mesmo `eventId`; consumidores deduplicam pelo ID.

## 8. Aplicação das operações

### 8.1 Abertura

Em uma transação:

- criar carteira com saldo e versão `1`;
- se saldo positivo, criar `OPENING` interna em `PROCESSED`, inserir lançamento
  de crédito e outbox de `WagerTransactionProcessed` e
  `WalletBalanceChanged`;
- se saldo zero, criar apenas carteira;
- colisão `(playerId,currency)` retorna conflito, sem segunda carteira/crédito.

### 8.2 Operações externas

Antes da transação, HTTP valida autenticação/escopo, formato/limites e DTO; SQS
decodifica o comando JSON plano e usa o `MessageId` atribuído pelo broker.
Ambos constroem o mesmo comando de negócio e passam pelo caso de uso
compartilhado. O envelope de entrada ilustrado no README original não é aceito.

A chave é única por provedor e seu hash é SHA-256 de uma representação JSON
canônica do comando de negócio (IDs normalizados, tipo canônico, moeda e unidades
menores); a chave e os metadados de transporte não entram no hash. Repetir a chave
com o mesmo hash retorna a transação e o resultado persistidos, sem nova
movimentação. Reutilizá-la com outro hash é conflito. Reutilizar o identificador
externo com outra chave também é conflito, inclusive quando o conteúdo coincide:
isso evita confirmar uma chave que não pode ser persistida como registro de
idempotência da transação original.

Regras:

| Tipo | Efeito | Resultado persistido |
| --- | --- | --- |
| `BET` | Débito positivo, saldo suficiente | Processada + ledger + eventos de sucesso |
| `WIN` | Crédito positivo | Processada + ledger + eventos de sucesso |
| `LOSS` | Sem alteração; money zero na moeda da carteira | Processada + sem ledger/versão + evento de sucesso |
| `REFUND` | Crédito integral de `BET` elegível | Processada + ledger + eventos de sucesso |
| `ROLLBACK` | Movimento oposto integral de referência elegível | Processada + ledger + eventos de sucesso |

Rejeição de domínio é terminal e persistida como `REJECTED`, com `failureCode` e
evento de rejeição na mesma transação. Falha temporária de banco desfaz a
transação inteira; falha de transporte HTTP retorna indisponibilidade e SQS
retenta sem ACK.

### 8.3 Referências pendentes

Ao não encontrar referência, persistir a transação em `PENDING_REFERENCE`; para
origem SQS, inbox e evento `WagerTransactionPendingReference` são confirmados
juntos. O worker consulta lotes de até 100 referências vencidas a cada segundo,
usando `FOR UPDATE SKIP LOCKED`, e tenta aplicar a operação na mesma transação
SQL quando a referência aparece. Referências ainda ausentes ou não terminais
recebem uma próxima tentativa durável cinco segundos depois, evitando que as
primeiras pendências sem referência monopolizem lotes futuros.

O estado pendente não tem TTL nem limite de tentativas nesta implementação:
permanece durável até a referência ficar disponível ou até a operação ser
resolvida por regra de negócio. A rejeição por expiração ainda não existe,
apesar de ser exigida pelo README original. Uma referência encontrada em
`PENDING`/`PENDING_REFERENCE` mantém a operação pendente; referência
`REJECTED`/`FAILED` ou incompatível causa rejeição estável. Um limite/expiração
de negócio precisa ser implementada com a política de máximo de tentativas/TTL
exigida pelo desafio.

## 9. Interfaces externas e segurança

### 9.1 HTTP

As rotas requeridas são:

```text
POST /wallets
GET  /wallets/{walletId}
GET  /wallets/{walletId}/ledger?cursor=...&limit=50
POST /wallets/{walletId}/reconciliation
POST /wagering/transactions
GET  /wagering/transactions/{transactionId}
GET  /providers/{providerId}/wagering/transactions/{externalTransactionId}
GET  /health/live
GET  /health/ready
```

Health é público. Operações e dados de carteira são internos; endpoints de
provedor são autenticados e limitados ao provedor do principal. Limites HTTP,
timeouts, body size, JSON estrito e validação de query são definidos na camada
adapter. Respostas não expõem payloads completos nem dados de outro provedor.

As permissões OIDC são explícitas: `wallet:write` abre carteiras, `wallet:read`
consulta carteira, `ledger:read` consulta lançamentos,
`reconciliation:write` executa reconciliação, `wager:internal:read` consulta
transações pelo ID interno, `wager:write` registra operações de provedor e
`wager:read` consulta operações do provedor autenticado. O POST de operação
exige `Idempotency-Key`; `providerId` no corpo é apenas uma asserção e deve
corresponder ao claim assinado `provider_id`. O caso de uso recebe sempre o
valor do claim, nunca o valor do corpo.

Erros HTTP usam `{ "code": "...", "message": "..." }`: `400 invalid_input`,
`401 unauthenticated`, `403 forbidden`, `404 not_found`, `409 conflict`,
`422` para rejeições de negócio, `202 pending`, `503 transient_failure` e `500
internal_error`. Uma operação processada retorna `201` na primeira execução e
`200` em replay, com o saldo persistido do resultado original e
`idempotentReplay`. Pendências retornam `202`; rejeições duráveis retornam
`422`. Cursor do ledger é base64url de envelope versionado vinculado à carteira
e usa ordenação estável `(created_at DESC, id DESC)`.

### 9.2 OAuth 2.0/OIDC

**Decisão implementada:** Keycloak no Compose, com  JWT Bearer e fluxo
`client_credentials` para chamadas serviço a serviço. O adapter OIDC valida
assinatura usando JWKS, `issuer`, `audience`, `exp` e `nbf`; falha fechada em
token ausente/inválido/expirado. Cache JWKS é limitado e renovável.

- Tokens de serviço carregam scopes/roles separados para operações internas de
  carteira e acesso de provedor.
- O realm local provisiona clients de `client_credentials` para serviço interno
  e provedores de exemplo, com audience `jungle-api`, permissões e claim
  `provider_id` inseridos pelo próprio IdP; os segredos incluídos são somente
  valores locais de desenvolvimento.
- `providerId` autorizado deriva de claim confiável do principal validado. Se o
  corpo incluir `providerId`, deve corresponder ao claim; nunca substitui a
  identidade.
- Consulta e replay aplicam a mesma autorização que a operação original.
- Cliente interno pode abrir/consultar carteiras e reconciliar; tokens de
  provedor não podem fazê-lo.
- Liveness permanece pública. Readiness depende de PostgreSQL e SQS; falhas de
  autenticação são tratadas fail-closed. A disponibilidade do IdP/JWKS e a
  política de cache de chave devem ser documentadas e testadas.

O cliente SQS é configurado com credenciais AWS. LocalStack usa credenciais
fictícias; nenhuma política IAM least-privilege para ambiente AWS é versionada
neste repositório e deve ser fornecida pelo deployment. O consumidor valida os
campos e as regras de domínio, mas não autentica tokens OIDC de produtores SQS.

## 10. Uber Fx, composição e ciclo de vida

**Decisão implementada:** composição Fx em um `fx.Module` central
(`internal/app.Module`), não em submódulos por domínio. `fx.Provide` constrói
configuração, pool/UnitOfWork, casos de uso, OIDC, SQS, probes, consumer,
publisher, resolver e servidor; `fx.Invoke` registra migrations, startup
checks e hooks Fx para cada worker/servidor.

`cmd/api` cria logger JSON `slog` e chama `fx.App.Run`. Domínio e aplicação não
importam Fx. O pool PostgreSQL fecha no lifecycle depois de HTTP e workers.
Métricas/exporter não estão implementados.

Ordem de start: configuração validada, dependências/pool e verificação, depois
consumidor/workers e servidor HTTP. Ordem de stop interrompe primeiro novas
entradas HTTP e polling SQS; aguarda
trabalho em andamento com deadline; uma mensagem sem commit reaparece após o
visibility timeout de 30 segundos; aguarda workers terminarem; por fim fecha o
pool PostgreSQL. Erro de start aborta a inicialização. Erros dos loops dos
workers são registrados e recebem retry/backoff; não há métrica nem política
automática para solicitar reinício do processo.

Cada worker possui contexto cancelável e sinal de término aguardável. Shutdown
forçado após timeout é logado e não confirma trabalho sem commit. O lifecycle Fx
não deve executar loop bloqueante dentro de `OnStart`; inicia goroutine
controlada e observa seu fim em `OnStop`.

## 11. Testes e critérios de aceite arquiteturais

### Testes unitários sem infraestrutura

- `Money`: strings válidas/inválidas, duas casas, limite positivo/negativo,
  overflow de parsing/soma/subtração/negação, moeda incompatível, JSON sem float.
- `Wallet`: abertura zero/positiva, versão, débito/crédito, saldo insuficiente,
  moeda, overflow e reidratação sem efeitos.
- `WagerTransaction`: cada transição, terminais, origem interna `OPENING`,
  códigos de falha e transições de referência.
- Tipos de operação: `BET`, `WIN`, `LOSS`, `REFUND`, `ROLLBACK`, regras de zero,
  referência e elegibilidade.
- Canonicalizador: ordem de campos invariável, metadados de transporte excluídos,
  normalização monetária definida e hashes equivalentes HTTP/SQS.
- Ledgers e eventos: direção/saldo, imutabilidade do tipo, envelope e payload.

### Testes de integração reais

Usar PostgreSQL, Keycloak e LocalStack/MiniStack em containers. Mocks podem
servir para testes unitários de portas, mas não substituem a verificação real de
constraints, OIDC, SQS, migrations, visibilidade, DLQ ou outbox.

- Aplicar e reverter migrations; provar unicidade, checks, FK e trigger/permissão
  de ledger.
- Confirmar atomicidade: falhar cada etapa entre saldo, ledger, inbox e outbox e
  verificar rollback total.
- Verificar abertura positiva e zero; versões, eventos e saldo.
- Exercitar transação repetida, chave/payload conflitante e external ID
  conflitante; confirmar snapshot de replay.
- Autenticar tokens ausentes, inválidos, expirados, dois providers e serviço
  interno; validar isolamento em consulta e replay.
- Rodar concorrência exigida com três processos/conexões independentes:
  50 duplicatas da mesma operação; disputa 2×80 sobre 100; carteiras distintas
  simultâneas; cruzamento HTTP/SQS.
- Exercitar inbox com hash igual/divergente, ACK somente depois do commit,
  visibilidade, retry, mensagem inválida e DLQ. O contrato original de envelope
  SQS também precisa de teste depois de implementado.
- Exercitar referência antecipada, chegada posterior e retomada após reinício.
  Expiração/rejeição por TTL permanece sem implementação e sem evidência.
- Interromper consumer depois do commit e antes do ACK; comprovar reentrega sem
  segundo efeito.
- Executar dois publishers concorrentes; interromper após claim, após publicar e
  antes de confirmar; comprovar recuperação e `eventId` preservado.
- Reiniciar aplicação e verificar idempotência, pendências, saldo e ledger.
- Fx: início com configuração válida/inválida, start com dependência ausente,
  shutdown normal e timeout com workers encerrados/liberados.
- Reconciliação: saldo reconstruído pelo ledger, divergência reportada sem
  alteração.

### Comandos finais

O checkout de entrega deve documentar e executar:

```sh
docker compose up --build
go test ./...
go test -race ./...
go vet ./...
```

Além disso, documentar preparação das dependências de integração, execução com
três instâncias e simulação de falhas. Os testes de carga, tracing e partidas
dobradas são diferenciais e não substituem nenhum teste acima.

## 12. ADRs (Architecture Decision Records)

Os registros abaixo documentam decisões arquiteturais; o estado indica se a
decisão está implementada ou ainda proposta. Devem ser atualizados se uma
implementação precisar divergir; qualquer desvio que afete invariantes
financeiras requer justificativa e testes equivalentes.

### ADR-001 — Domínio independente e arquitetura em camadas

**Estado:** Implementado.

**Contexto:** Regras financeiras precisam ser testáveis sem depender de
transporte, broker, banco ou framework de composição.

**Alternativas:** (a) arquitetura em camadas com dependências para dentro;
(b) handlers chamando diretamente SQL/SQS; (c) domínio importando Fx/adaptadores.

**Decisão:** Pacotes de domínio puros; casos de uso na aplicação; portas da
aplicação; adaptadores HTTP/SQS/PostgreSQL/OIDC; Fx somente na composição.

**Consequências:** Mais tipos/contratos entre camadas, mas regras reaproveitáveis
por HTTP e SQS e testes unitários isolados. Evitar interfaces genéricas sem uso.

### ADR-002 — Dinheiro em centavos `int64`, escala fixa e parsing estrito

**Estado:** Implementado.

**Contexto:** Contrato exige precisão exata, duas casas decimais e proteção de
overflow.

**Alternativas:** `float64`; decimal arbitrary precision; unidades mínimas em
inteiro.

**Decisão:** `Money` imutável de centavos `int64`, BRL/USD/EUR no domínio
inicial, duas casas obrigatórias em entradas externas e checagem explícita de
overflow. Os fluxos financeiros principais permanecem em BRL.

**Consequências:** Aritmética eficiente e simples de persistir como `BIGINT`;
limite explícito de magnitude. Nenhum arredondamento é permitido; moedas além
das três listadas precisam de decisão sobre código e escala antes de suporte.

### ADR-003 — PostgreSQL como autoridade e lock por linha da carteira

**Estado:** Implementado.

**Contexto:** Várias instâncias processam a mesma carteira; locks locais não
oferecem garantia distribuída.

**Alternativas:** lock em memória; lock global; controle otimista puro; lock
pessimista por linha.

**Decisão:** `SELECT FOR UPDATE` por carteira e atualização condicionada por
versão, constraints no banco e transação SQL curta. Ordem de locks documentada.

**Consequências:** Serializa operações na mesma carteira e permite paralelismo
entre carteiras distintas. Requer teste com múltiplas conexões/processos e
tratamento de deadlocks/retries.

### ADR-004 — Idempotência persistida e hash de payload canônico

**Estado:** Implementado.

**Contexto:** HTTP e SQS têm entrega repetida; FIFO/cache não é autoridade
durável; replay precisa preservar saldo original.

**Alternativas:** cache local; confiar apenas em dedupe SQS; unicidade persistida
com hash; idempotência somente por external ID.

**Decisão:** Constraints únicas no banco para chave e identidade externa,
canonicalização compartilhada com SHA-256 e snapshot do resultado financeiro.

**Consequências:** Comportamento uniforme após restart e entre transportes; exige
contrato versionado de canonicalização e resposta explícita para colisões.

### ADR-005 — `PENDING_REFERENCE` durável e sem estado `PROCESSING`

**Estado:** Parcial. Não há estado `PROCESSING`, e referências são duráveis,
mas o resolver ainda não aplica máximo de tentativas/TTL nem rejeita pendências
expiradas.

**Contexto:** O README exige referências que podem chegar depois e lista os
estados sem enumerar `PROCESSING`, embora mencione transição para processamento.

**Alternativas:** adicionar status `PROCESSING`; usar claim/lease fora do estado
de domínio; evitar commit intermediário para operações sem dependências.

**Decisão:** Não adicionar `PROCESSING` ao estado de domínio. Operações
independentes processam em uma transação; workers usam locks de linha e
`SKIP LOCKED`; ausência de referência persiste como `PENDING_REFERENCE` com reagendamento persistente a cada cinco segundos; TTL e
limite de tentativas não estão implementados.

**Consequências:** Evita estado adicional não definido e simplifica recuperação
transacional. Se houver `PENDING` commitado entre requisição e conclusão, será
obrigatório adicionar mecanismo durável de retomada antes de habilitá-lo.

### ADR-006 — Ledger append-only e reconciliação não mutável

**Estado:** Implementado, exceto a métrica de divergência exigida.

**Contexto:** Ledger é evidência auditável; correções não podem apagar história.

**Alternativas:** ledger editável; apenas saldo agregado; lançamentos
append-only com saldo materializado.

**Decisão:** Manter saldo materializado em `wallets` e lançamentos imutáveis em
`wallet_ledger_entries`; proteger no banco por constraints, trigger e
permissões. Reconciliar em snapshot consistente sem corrigir automaticamente.

**Consequências:** Consultas comuns são eficientes e divergências detectáveis;
é necessário manter trigger/role na migration e testar constraints no banco real.

### ADR-007 — Inbox/outbox transacionais e entrega externa at-least-once

**Estado:** Implementado.

**Contexto:** Não se pode publicar antes do commit nem perder evento confirmado;
consumer/publisher podem cair entre passos.

**Alternativas:** dual-write direto; publicar dentro da transação SQL; inbox/outbox
persistentes.

**Decisão:** Persistir inbox e efeitos de entrada na mesma transação; inserir
outbox junto com o efeito; publishers claimam com lease, publicam fora da
transação e marcam depois; reenvios mantêm `eventId`.

**Consequências:** Entrega é at-least-once e consumidores deduplicam por
`eventId`; outbox exige monitoramento de atraso, retry e retomada de leases.

### ADR-008 — Keycloak OIDC e autorização por identidade validada

**Estado:** Implementado.

**Contexto:** Autenticação/autorização externa é obrigatória; o corpo não pode
autorizar o próprio `providerId`.

**Alternativas:** tokens emitidos pela aplicação; identidade externa; API key
sem claims de autorização.

**Decisão:** Keycloak local, JWT/OIDC `client_credentials`, validação de
assinatura/JWKS/issuer/audience/expiração; claim confiável determina provider;
scope interno separado de scope de provedor.

**Consequências:** Provisionamento de realm/clients e testes reais são
necessários. Rotação/cache de JWKS deve falhar de forma segura. HTTP não é a
única barreira: SQS também depende de credenciais e política de produtor.

### ADR-009 — SQS FIFO com inbox; fila de eventos de saída FIFO

**Estado:** Parcial. FIFO, inbox/outbox e ACK pós-commit estão implementados,
mas o formato do comando e a identidade inbox divergem do envelope original.

**Contexto:** SQS local é obrigatório; entrega at-least-once e reordenação de
referências são cenários do desafio.

**Alternativas:** SQS Standard; FIFO sem inbox; FIFO com inbox/outbox persistentes.

**Decisão:** Filas FIFO de entrada e DLQ provisionadas; inbox por consumidor e
`messageId`; ACK após commit; outbox publica em `wager-events.fifo` com
`MessageGroupId=aggregateId` e `MessageDeduplicationId=eventId`.

**Consequências:** Ordenação por agregado ajuda, mas dedupe temporal não substitui
constraints nem idempotência durável. A fila de entrada aplica visibility,
backoff para falhas transitórias e redrive após cinco recebimentos; mensagens
inválidas são encaminhadas explicitamente à DLQ. Os workers Fx cancelam polling
e aguardam o término com `SHUTDOWN_TIMEOUT`.

### ADR-010 — Fx compõe lifecycle, não regras de domínio

**Estado:** Implementado, com validação de shutdown descrita nos limites.

**Contexto:** Fx é obrigatório para composição e gestão de recursos.

**Alternativas:** singleton global/manual; Fx em todos os pacotes; módulos Fx na
raiz de composição.

**Decisão:** Módulos Fx por infraestrutura/interface/worker; constructors
constroem dependências; lifecycle inicia e para servidores/workers/recursos.
Domínio e casos de uso não importam Fx.

**Consequências:** Inicialização e shutdown observáveis/testáveis; composição
deve validar configuração/dependências antes de aceitar tráfego e respeitar
ordem de fechamento.

### ADR-011 — Testes com infraestrutura real para garantias sistêmicas

**Estado:** Parcial. Testes reais estão disponíveis e executados; há cenários do
README original sem prova automatizada de contrato completo, expiração e métricas.

**Contexto:** Mocks não demonstram locks, constraints, OIDC, FIFO, visibility,
redrive ou recuperação real.

**Alternativas:** apenas unit tests; mocks para todos os adaptadores; unit tests
mais integração em containers.

**Decisão:** Unit tests para domínio e canonicalizador; integração com
PostgreSQL, Keycloak e LocalStack/MiniStack reais; concorrência com ao menos três
processos; execução `go test -race`.

**Consequências:** Testes levam mais tempo e precisam de ambiente reprodutível,
mas validam as garantias de aceite; separar comandos unitários e de integração
sem substituir integração real.

## 13. Decisões fechadas e gaps pendentes

As decisões do escopo atual foram tomadas: reversões de tipos diferentes são
mutuamente exclusivas para a mesma referência; a API persiste BRL/USD/EUR;
eventos saem por `wager-events.fifo` agrupados pelo agregado; recursos de outro
provedor são ocultados com 404; operações sem referência não confirmam um
estado `PENDING` intermediário. Isso não elimina os gaps de implementação abaixo.

1. **Referências pendentes (requisito não atendido):** `ResolvePendingReferences`
   reagenda a cada cinco segundos; não mantém contagem/TTL e não fecha com
   `REFERENCE_NOT_FOUND` ou evento de rejeição após expiração.
2. **Contrato de entrada SQS (requisito não atendido):** decodifica comando plano;
   não valida envelope `messageId`, `type`, `occurredAt`, `data`, nem usa
   `messageId` do envelope como chave inbox. A identidade atualmente é o
   `MessageId` atribuído pelo SQS.
3. **Contrato de saída (desvio nominal):** evento inclui `schemaVersion` e
   `WalletBalanceChangedData` serializa valor como `amount`, enquanto o README
   original denomina esses campos `version` e `money`. Confirmar compatibilidade
   com consumidores antes de declarar o contrato conforme.
4. **Observabilidade (requisito explícito não atendido):** não existe endpoint,
   registry ou exporter de métricas. Reconciliação não emite métrica e logs de
   request HTTP não incluem correlation/provider/transaction/wallet IDs.
5. **Migrations:** pares up/down existem e `Down` é exercitado em teste, mas não
   há CLI de rollback; manutenção operacional ainda precisa de comando suportado.
6. **Shutdown:** testes Fx cobrem cancelamento e espera dos workers; ainda falta
   teste de processo real encerrado por SIGTERM e medida do tempo de término.
7. **Autorização SQS em AWS:** credenciais são configuráveis e LocalStack é
   usado nos testes; roles/policies least-privilege de produção não são
   provisionadas nem validadas aqui.

## 14. Matriz de auditoria final e rastreabilidade

| Requisito do README original | Estado nesta auditoria | Evidência ou gap |
| --- | --- | --- |
| Go/Modules, Fx, PostgreSQL, SQS, Compose, migrations up/down | Atendido com ressalva | `go.mod`, `internal/app`, `compose.yaml`, pares SQL e `TestPostgresMigrationsUpDownAndIdempotency`; não existe CLI Down. |
| Money exato, escala, rejeição de notação científica e overflow | Atendido | `domain/money` usa `int64` centavos, currency, guards e testes unitários/race. |
| Saldo não negativo, locks por carteira e sem lost update | Atendido | Constraints/triggers, `SELECT FOR UPDATE`, versionamento; testes PG concorrentes e campanha de três processos. |
| Ledger append-only e reconciliação | Atendido com ressalva | Constraints/triggers e testes de UPDATE/DELETE; endpoint reconstrói saldo. Métrica de divergência não existe. |
| Idempotência persistente, conflito e replay do saldo original | Atendido | Índices únicos/resultado em PostgreSQL; `TestFinancialUseCaseIdempotencyAndExternalReplay` inclusive após recriar serviço. |
| Atomicidade financeira, inbox e outbox | Atendido | `UnitOfWork` pgx, commit único e testes de rollback/redelivery; publicação ocorre fora da transação e depois do commit. |
| Autenticação, autorização interna e isolamento entre providers | Atendido | OIDC Keycloak real, permissões dos claims, testes de token e isolamento; evidência resumida no relatório. |
| SQS at-least-once, retry, redrive e DLQ | Parcial | Código implementa retry de transporte e DLQ, incluindo teste LocalStack; envelope/identidade do desafio não atendidos e política IAM AWS não provisionada. |
| Referências antecipadas e retomada após reinício | Parcial | Estado e agenda persistem e resolução após chegada tem teste real; sem TTL/máximo nem rejeição final. |
| Eventos tipados e contrato de envelope | Parcial | Eventos/outbox persistentes e publisher testado; nomes `schemaVersion`/`amount` diferem de `version`/`money` no contrato descrito. |
| Fx lifecycle e liberação de recursos | Parcialmente validado | Hooks para HTTP/consumer/publisher/resolver e testes de lifecycle; sem prova SIGTERM de processo externo nesta campanha. |
| Logs estruturados, métricas e health checks | Parcial | `slog` JSON e liveness/readiness implementados; métricas ausentes e logs de request sem IDs de correlação. |
| Docker/IdP/filas reproduzíveis e ausência de segredos reais | Atendido para perfil local | Realm import e filas provisionadas pelo Compose; `.env.example` contém credenciais fictícias locais. O workspace fornecido não é repositório Git, então a auditoria local não prova o conteúdo de um commit remoto. |
| Documentação checkout limpo e comandos reproduzíveis | Atendido com ressalvas documentadas | `README.md` contém pré-requisitos, Compose, tokens, chamadas e testes; CLI rollback não está disponível. |
| Diferenciais: tracing, teste de carga, partidas dobradas | Não implementados | São opcionais e não impedem o aceite obrigatório. |
