# Webhook Tester

Um servidor local que captura qualquer request de webhook, responde com mocks
canned e mostra **cada endpoint na sua própria tela**. Binário único, sem
depender de serviço público nem de limite de requisições.

## Rodar

```bash
./dev
```

Aponte o webhook para `http://<seu-ip>:8889/qualquer/coisa` (qualquer path,
qualquer método) e navegue pelas telas.

### Teclas

Os dois painéis são listas verticais, então as setas `↑` `↓` dirigem o painel
que está com o foco, e `←` `→` movem o foco entre eles. O painel focado fica com
o título e o marcador `▸` acesos; o outro, apagados. O foco começa nos endpoints.

| tecla | ação |
|---|---|
| `↑` `↓` / `k` `j` | navega no painel focado: troca de endpoint ou anda nas requests |
| `←` `→` / `h` `l` | move o foco entre endpoints e requests |
| `tab` | alterna o foco |
| `space` | pausa — o que chegar fica na fila até você soltar |
| `/` | busca em método, path, headers e corpo · `esc` limpa |
| `s` | força o status do endpoint: 500 → 429 → 200 → normal |
| `c` | limpa as requests da tela |
| `q` | sai |

Com o foco nas requests, `pgup`/`pgdn` rolam o dump do painel de detalhe.

A tela `All` agrega tudo. Path que nenhum mock atende cai em `(unmatched)`, que
é criada sozinha. O contador ao lado do nome vira badge amarelo `3•` quando
chegou coisa numa tela que você não está olhando.

## mocks.json

Lido a cada request — editar não exige restart. O primeiro mock que casa vence,
e `*` casa qualquer coisa, inclusive `/`.

```json
[
  {
    "method": "POST",
    "path": "/meta/*/messages",
    "status": 200,
    "delayMs": 1500,
    "body": { "messages": [{ "id": "wamid.TESTE1" }] }
  },
  {
    "method": "POST",
    "path": "/bilhetis/tickets",
    "sequence": [
      { "status": 500, "body": { "error": "boom" } },
      { "status": 200, "body": { "id": "tkt-000001" } }
    ]
  }
]
```

- `status` — o que responder. Sem ele, sorteia de `STATUS`.
- `delayMs` — segura a resposta, para testar timeout do cliente.
- `body` — resposta fixa.
- `sequence` — cicla uma entrada por chamada, para testar retry. Substitui
  `status` e `body` quando presente.

Sem mock nenhum, a resposta é
`{"status":"received","method":"...","path":"..."}`.

## Variáveis de ambiente

| variável | padrão | o que faz |
|---|---|---|
| `PORT` | `8889` | porta de escuta |
| `MOCKS_FILE` | `mocks.json` | de onde vêm os mocks |
| `LOG_FILE` | `webhook.jsonl` | uma linha JSON por request, vazio desliga |
| `STATUS` | `[200]` | status sorteados quando o mock não define um |
| `SECRET` | vazio | prefixo obrigatório no path |

## Expondo publicamente

Uma URL pública é varrida por bots em segundos (`/.env`, `/.git/config`, ...).
Com `SECRET`, tudo que não começa com aquele prefixo leva 404 silencioso — sem
sequer virar linha na tela:

```bash
SECRET=$(openssl rand -hex 8) ./dev
```

Quem chama passa a usar `https://<host>/<token>/bilhetis` em vez de
`https://<host>/bilhetis`. Sem `SECRET` (o padrão), todo path fica aberto —
suficiente para localhost.

## Log em disco

Toda request vira uma linha em `webhook.jsonl`, o que sobrevive a restart e
permite investigar depois:

```bash
jq 'select(.status >= 400)' webhook.jsonl
```

## Testes

```bash
go test -race ./...
```
