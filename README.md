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
