# ElecPostal

Go implementation of the Solar Network email service.

## Run

```bash
cp config.example.toml config.toml
# edit config.toml
go run ./cmd
```

## Logging

- `ZEROLOG_PRETTY=true` enables console-style pretty logs
- `LOG_LEVEL=debug|info|warn|error` sets the log level

## Config

Use `--config` or `CONFIG_PATH` for a TOML config file.

Key settings:

- `app.name`
- `database.dsn`
- `http.port`
- `grpc.port`
- `grpc.useTLS`
- `auth.target`
- `auth.useTLS`
- `filesystem.target` (optional FileSystem gRPC endpoint for inbound email attachments)
- `solarNetwork.baseUrl`
- `solarNetwork.accessToken`

## Attachments

Clients upload outgoing attachments to DysonFS first, then submit the returned
file IDs as the `attachment_ids` string array to `POST /api/emails`. Inbound delivery
services pass raw attachments to `EmailService.ReceiveEmail`; ElecPostal streams
them to DysonFS under the destination mailbox's owner and workspace before it
persists the email.

## Credentials

Create scoped, revocable app passwords at `POST /api/credentials` with a
label and one or more protocols (`smtp`, `imap`, `pop3`). The returned `secret`
is shown once and is only accepted by mail protocol listeners—not by HTTP APIs.

## Outbound delivery

Set `mail.relay.adapter = "direct-smtp"` to deliver directly to recipient MX
records without a relay. `mail.relay.host` must be this server's public mail
hostname for EHLO. Direct delivery uses TCP port 25 and opportunistically uses
STARTTLS; set `mail.relay.tlsMode = "required"` to reject MX servers that do
not support STARTTLS.

Other delivery providers, including SES, are implemented as adapters behind the
same outbound-delivery contract. Attachment IDs require a DysonFS byte source;
until that is configured, enabled delivery rejects messages with attachments
rather than dropping them.
