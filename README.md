# Backend Challenge Go — carteira e processamento de apostas

Serviço Go para operações financeiras de provedores de jogos. A aplicação usa
Uber Fx, PostgreSQL, HTTP/OIDC e AWS SQS (LocalStack no desenvolvimento). O
ledger, a inbox e a outbox são persistidos no PostgreSQL.

> **Estado da auditoria:** a implementação tem evidência para as garantias
> financeiras centrais e os testes listados abaixo, mas ainda não satisfaz
> todos os requisitos do README original. Em particular, não há métricas
> exportadas, expiração/máximo de tentativas para referências ausentes, e o
> consumidor SQS usa um comando plano em vez do envelope original. O evento de
> saldo tem divergência nominal no payload; política IAM de produção também não
> é provisionada neste repositório. Consulte
> [ARCHITECTURE.md](./ARCHITECTURE.md) e
> [VALIDATION_REPORT.md](./VALIDATION_REPORT.md) antes de considerar a entrega
> aprovada.

## Pré-requisitos

- Docker Desktop e Docker Compose v2.
- Go 1.24.4 (versão declarada em `go.mod` e no Dockerfile).
- PowerShell 5.1+ nos exemplos abaixo. Para comandos POSIX, troque `Copy-Item`
  por `cp`, `;` por `&&` quando adequado e use os nomes de variáveis do shell.

## Checkout limpo: configurar e iniciar dependências

Na raiz do projeto:

```powershell
Copy-Item .env.example .env
docker compose up --build -d postgres keycloak localstack
```

O Compose usa PostgreSQL 16.10, Keycloak 26.3.3 e LocalStack 4.8.1. O realm
`jungle-local`, a audience `jungle-api` e os clientes locais `internal-service`,
`provider-a` e `provider-b` são importados de
[`deploy/keycloak/jungle-local-realm.json`](./deploy/keycloak/jungle-local-realm.json).
O hook do LocalStack provisiona repetivelmente:

- `wager-transactions.fifo` — comandos de entrada;
- `wager-transactions-dlq.fifo` — dead-letter queue; redrive após cinco
  recebimentos;
- `wager-events.fifo` — eventos publicados pela outbox.

Espere pelos health checks:

```powershell
docker compose ps
Invoke-RestMethod http://localhost:8081/realms/jungle-local/.well-known/openid-configuration
```

Os dados do PostgreSQL ficam no volume nomeado `postgres-data`. As filas são
provisionadas quando o LocalStack inicia. As credenciais constantes no realm e
no `.env.example` são **somente valores locais de demonstração**; não as use em
ambientes compartilhados ou produção. Não versione `.env`.

## Aplicação e migrations

A aplicação aplica migrations pendentes automaticamente no startup, antes de
aceitar tráfego, sob advisory lock PostgreSQL. Cada migration `.up.sql` é
aplicada em transação e registrada com checksum. Os pares `.down.sql` são
reversíveis e testados em schemas de integração isolados. O repositório não
fornece um comando CLI de rollback operacional; `MigrationRunner.Down` é uma
API interna e seu uso não deve ser improvisado contra dados que precisem ser
preservados. Consulte [migrations/README.md](./migrations/README.md).

Para provar aplicação e reversão em um schema descartável de integração:

```powershell
$env:TEST_DATABASE_URL = "postgres://jungle:jungle-local-password@localhost:5432/jungle?sslmode=disable"
go test ./internal/infrastructure/postgres -run TestPostgresMigrationsUpDownAndIdempotency -count=1
```

O teste cria e remove apenas seu schema isolado; não é uma ferramenta para
reverter o schema de uma instalação em uso.

Para executar a API em container depois das dependências:

```powershell
docker compose up --build -d app
Invoke-RestMethod http://localhost:8080/health/live
Invoke-RestMethod http://localhost:8080/health/ready
```

Alternativamente, iniciar tudo em primeiro plano:

```powershell
docker compose up --build
```

Endpoints locais: API `http://localhost:8080`, Keycloak
`http://localhost:8081`, LocalStack `http://localhost:4566` e PostgreSQL
`localhost:5432`. `GET /health/live` é liveness; `GET /health/ready` consulta
PostgreSQL e SQS e devolve 503 quando uma dependência não está disponível. As
duas rotas são públicas. A imagem da API instala `curl` para sua sonda Docker.

Para parar sem remover o volume:

```powershell
docker compose stop
```

`docker compose down` remove containers e rede, mas preserva o volume nomeado.
**Não** use `docker compose down -v` se quiser manter os dados. Fx interrompe o
servidor e cancela/aguarda os workers até `SHUTDOWN_TIMEOUT`; uma mensagem SQS
em andamento cujo tratamento não confirmou pode voltar a ficar visível quando
expirar seu visibility timeout de 30 segundos.

## Carregar variáveis no PowerShell

O Go não carrega `.env` automaticamente. O Compose faz interpolação do arquivo,
mas, para executar a aplicação ou os testes diretamente no host, importe as
variáveis para o processo atual:

```powershell
Get-Content .env | ForEach-Object {
    if ($_ -match '^\s*([^#=]+)=(.*)$') {
        Set-Item -Path "Env:$($matches[1].Trim())" -Value $matches[2].Trim()
    }
}
```

O arquivo de exemplo usa valores sem espaços nem expansão de shell. Para outra
configuração, edite `.env` localmente; `DATABASE_URL`, credenciais AWS e URLs
SQS são obrigatórias para iniciar a aplicação. `HTTP_ADDR`, `AWS_REGION`,
`STARTUP_TIMEOUT` e `SHUTDOWN_TIMEOUT` têm defaults validados.

## Chamadas HTTP autenticadas

Tokens de serviço são obtidos via `client_credentials`. O `provider_id` e as
permissões vêm dos claims assinados pelo Keycloak. Operações de carteira e
reconciliação exigem o cliente interno; operações e consultas de provedor
exigem o token daquele provedor. O `providerId` do corpo é validado contra a
identidade autenticada e não concede autorização.

Obtenha tokens locais:

```powershell
$providerToken = (Invoke-RestMethod -Method Post `
  -Uri "$env:OIDC_ISSUER_URL/protocol/openid-connect/token" `
  -ContentType "application/x-www-form-urlencoded" `
  -Body @{ grant_type = "client_credentials"; client_id = "provider-a"; client_secret = "provider-a-local-secret" }).access_token

$internalToken = (Invoke-RestMethod -Method Post `
  -Uri "$env:OIDC_ISSUER_URL/protocol/openid-connect/token" `
  -ContentType "application/x-www-form-urlencoded" `
  -Body @{ grant_type = "client_credentials"; client_id = "internal-service"; client_secret = "internal-local-secret" }).access_token
```

Abra carteira com o token interno:

```powershell
$playerId = "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1"
$wallet = Invoke-RestMethod -Method Post -Uri "http://localhost:8080/wallets" `
  -Headers @{ Authorization = "Bearer $internalToken" } `
  -ContentType "application/json" `
  -Body (@{ playerId = $playerId; initialBalance = @{ amount = "100.00"; currency = "BRL" } } | ConvertTo-Json -Compress)
$wallet | ConvertTo-Json -Depth 8
$walletId = $wallet.id
```

Envie uma aposta e preserve a chave para retries idempotentes:

```powershell
$idempotencyKey = "provider-a:bet-example-001"
$wagerBody = @{
    providerId = "provider-a"
    externalTransactionId = "bet-example-001"
    playerId = $playerId
    walletId = $walletId
    roundId = "round-example-001"
    gameId = "game-example"
    kind = "BET"
    money = @{ amount = "25.00"; currency = "BRL" }
} | ConvertTo-Json -Compress

Invoke-RestMethod -Method Post -Uri "http://localhost:8080/wagering/transactions" `
  -Headers @{ Authorization = "Bearer $providerToken"; "Idempotency-Key" = $idempotencyKey } `
  -ContentType "application/json" -Body $wagerBody
```

Primeiro processamento bem-sucedido retorna 201; replay retorna 200 com
`idempotentReplay=true` e o resultado financeiro original. Chave reutilizada
com conteúdo diferente ou ID externo reapresentado com outra chave retorna
409. Rejeição de negócio terminal retorna 422; referência ainda ausente retorna
202. Para as mesmas chamadas em um novo terminal/sessão, reobtenha os tokens.

Consulte carteira/ledger e transação usando suas permissões; reconciliação é
restrita ao serviço interno:

```powershell
Invoke-RestMethod "http://localhost:8080/wallets/$walletId" `
  -Headers @{ Authorization = "Bearer $internalToken" }
Invoke-RestMethod "http://localhost:8080/wallets/$walletId/ledger?limit=50" `
  -Headers @{ Authorization = "Bearer $internalToken" }
Invoke-RestMethod -Method Post -Uri "http://localhost:8080/wallets/$walletId/reconciliation" `
  -Headers @{ Authorization = "Bearer $internalToken" }
Invoke-RestMethod "http://localhost:8080/providers/provider-a/wagering/transactions/bet-example-001" `
  -Headers @{ Authorization = "Bearer $providerToken" }
```

`GET /wagering/transactions/{transactionId}` requer `wager:internal:read`.
`GET /providers/{providerId}/wagering/transactions/{externalTransactionId}`
oculta recursos de outro provedor como 404. Rotas adicionais:
`GET /wallets/{walletId}`, `GET /wallets/{walletId}/ledger`,
`GET /wagering/transactions/{transactionId}`.

## SQS e contratos atuais

O publisher envia os eventos da outbox para `wager-events.fifo`, com
`MessageGroupId=aggregateId` e `MessageDeduplicationId=eventId`. A outbox é
confirmada somente depois que o SQS aceita o envio; uma falha no intervalo pode
republicar o mesmo `eventId` (at-least-once). Consumidores downstream precisam
de idempotência durável.

**Desvios conhecidos do contrato do desafio:** o consumidor atual decodifica um
`WagerCommand` JSON plano, não o envelope
`{messageId,type,occurredAt,data}` ilustrado pelo README original. A inbox usa
o `MessageId` atribuído pelo SQS como identidade e verifica o hash canônico dos
campos financeiros na redelivery; ela não usa o `messageId` do envelope. O
comando real aceito pelo código tem este formato:

```json
{
  "providerId": "provider-a",
  "externalTransactionId": "bet-from-sqs-001",
  "idempotencyKey": "provider-a:bet-from-sqs-001",
  "playerId": "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
  "walletId": "PREENCHA_COM_O_ID_DA_CARTEIRA",
  "roundId": "round-sqs-001",
  "gameId": "game-example",
  "kind": "BET",
  "money": { "amount": "5.00", "currency": "BRL" }
}
```

O contrato de eventos também difere nominalmente: o envelope usa
`schemaVersion` em vez de `version`, e `WalletBalanceChanged` serializa o valor
como `amount` em vez de `money`. Credenciais AWS são configuráveis, mas
políticas IAM least-privilege para uma conta AWS não são incluídas; o Compose
usa somente credenciais fictícias do LocalStack.

Com a aplicação em execução, a mensagem pode ser publicada localmente usando
AWS CLI incluído no container LocalStack. Substitua o ID pelo retorno de
`POST /wallets`:

```powershell
$message = '{"providerId":"provider-a","externalTransactionId":"bet-from-sqs-001","idempotencyKey":"provider-a:bet-from-sqs-001","playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","walletId":"PREENCHA_COM_O_ID_DA_CARTEIRA","roundId":"round-sqs-001","gameId":"game-example","kind":"BET","money":{"amount":"5.00","currency":"BRL"}}'
$encoded = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($message))
$tempFile = "wager-command-$([guid]::NewGuid().ToString('N')).json"
docker compose exec -T localstack sh -c "printf '%s' '$encoded' | base64 -d > '/tmp/$tempFile'"
docker compose exec -T localstack awslocal sqs send-message `
  --queue-url http://localhost:4566/000000000000/wager-transactions.fifo `
  --message-body "file:///tmp/$tempFile" --message-group-id provider-a `
  --message-deduplication-id bet-from-sqs-001
docker compose exec -T localstack rm -f "/tmp/$tempFile"
```

O arquivo é criado no container para preservar as aspas do JSON em chamadas
PowerShell para a CLI nativa; o nome é único e apenas esse arquivo temporário é
removido ao final.

Mensagens malformadas/permanentemente inválidas são copiadas para DLQ antes da
remoção da fila original. Falhas transitórias recebem visibility delay com
backoff exponencial; o redrive configurado no broker envia mensagens após cinco
recebimentos para DLQ. O comando direto usa credenciais de desenvolvimento
LocalStack, não é configuração para AWS real.

## Testes e validação

Primeiro inicie as dependências reais, sem a API/consumidor competir pelas filas:

```powershell
docker compose up --build -d postgres keycloak localstack
```

Carregue `.env` com o bloco acima e configure a integração:

```powershell
$env:TEST_DATABASE_URL = $env:DATABASE_URL
$env:TEST_SQS_ENDPOINT = $env:AWS_ENDPOINT_URL
$env:TEST_SQS_INPUT_QUEUE_URL = $env:SQS_QUEUE_URL
$env:TEST_SQS_DLQ_URL = $env:SQS_DLQ_URL
$env:TEST_SQS_OUTPUT_QUEUE_URL = $env:SQS_OUTPUT_QUEUE_URL
$env:TEST_OIDC_ISSUER = $env:OIDC_ISSUER_URL
$env:TEST_OIDC_AUDIENCE = $env:OIDC_AUDIENCE
$env:TEST_OIDC_CLIENT_ID = "provider-a"
$env:TEST_OIDC_CLIENT_SECRET = "provider-a-local-secret"
$env:TEST_OIDC_PROVIDER_ID = "provider-a"
$env:TEST_OIDC_INTERNAL_CLIENT_ID = "internal-service"
$env:TEST_OIDC_INTERNAL_CLIENT_SECRET = "internal-local-secret"
$env:TEST_OIDC_PROVIDER_A_CLIENT_ID = "provider-a"
$env:TEST_OIDC_PROVIDER_A_CLIENT_SECRET = "provider-a-local-secret"
$env:TEST_OIDC_INTERNAL_URL = ""
```

Execute da raiz:

```powershell
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
$goFiles = Get-ChildItem cmd,internal,migrations -Recurse -Filter *.go | ForEach-Object { $_.FullName }
gofmt -l $goFiles
docker compose config -q
```

Com as variáveis de integração definidas, testes PostgreSQL, LocalStack e
Keycloak usam os serviços reais. Sem elas, testes de integração correspondentes
podem ser ignorados; um `go test` verde sem essas variáveis não demonstra as
garantias desses sistemas. Os cenários e últimos resultados observados estão
em [VALIDATION_REPORT.md](./VALIDATION_REPORT.md). Os testes criam schemas
PostgreSQL isolados; a remoção desses schemas é limitada ao próprio teste.

Para iniciar três instâncias de API isoladas em portas distintas, configure
`HTTP_ADDR` individualmente e use o mesmo banco/filas. Não execute a bateria de
testes que consome as filas ao mesmo tempo que esses workers. O relatório de
validação registra a execução distribuída em três processos, os comandos e
resultados. `go test -race` e os testes com serviços reais são necessários para
evidência de aceite.

## Configuração e limites importantes

Veja [`.env.example`](./.env.example) para todas as variáveis e valores locais.
O projeto não expõe endpoint de métricas atualmente; divergências de
reconciliação são reportadas em resposta/log, mas a métrica exigida no README
original não está implementada. A resolução de referências pendentes é
persistente e repetida a cada cinco segundos, mas ainda não tem limite de
tentativas/TTL nem rejeição final por expiração. Esses itens, e o desvio do
envelope SQS descrito acima, precisam ser corrigidos antes de afirmar
conformidade completa.

## Requisitos do desafio

O desafio original está em
[junglegaming/backend-challenge-go](https://github.com/kassiobuck/backend-challenge-go).
As decisões da implementação e a auditoria requisito-a-requisito estão em
[ARCHITECTURE.md](./ARCHITECTURE.md). A entrega **não está aprovada integralmente**
enquanto os gaps ali identificados não forem resolvidos e verificados.
