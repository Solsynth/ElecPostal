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
- `workspace.target` (Workspace gRPC endpoint required for workspace mailbox authorization and plan-based mail quotas)
- `ring.target` (optional Ring gRPC endpoint for account notifications)
- `mail.domain` (canonical mail domain; local-only mailbox addresses are completed with this domain before outbound delivery and exposed via `GET /api/mail/host`)

## Attachments

Clients upload outgoing attachments to DysonFS first, then submit the returned
file IDs as the `attachment_ids` string array to `POST /api/emails`. Inbound delivery
services pass raw attachments to `EmailService.ReceiveEmail`; ElecPostal streams
them to DysonFS under the destination mailbox's owner and workspace before it
persists the email.

## Workspace email quota

Mailboxes belong to a workspace. ElecPostal reads that workspace's plan quota
and reserves 10% for raw email records in its database (for example, a 10 GB
plan allows 1 GB of active email). DysonFS attachment content is excluded from
this calculation because it is already counted by DysonFS. When the allowance
is exceeded, ElecPostal archives the oldest messages and permanently removes
their raw records after 30 days.

## Outbound send limits

Outbound messages are counted both per mailbox and across the workspace. The
defaults are configurable under `mail.sendLimits.free`, `mail.sendLimits.pro`,
and `mail.sendLimits.enterprise`; each plan has `mailboxDaily`,
`mailboxMonthly`, `workspaceDaily`, and `workspaceMonthly` values. Set a value
to `0` to disable that specific limit. Drafts do not consume quota; scheduled
messages consume it when they are delivered.

## Credentials

Create scoped, revocable app passwords at `POST /api/credentials` with a
`mailbox_id`, label, and one or more protocols (`smtp`, `imap`, `pop3`). The
returned `secret` is shown once and is only accepted by that mailbox address's
protocol listeners—not by HTTP APIs or another address on the same account.

## Protocol storage

ElecPostal retains the canonical RFC 5322 source for received and sent mail,
with per-address IMAP folders and stable POP3/IMAP UIDs. This pre-release
service uses GORM `AutoMigrate` on startup. The provided `docker-compose.yml`
starts PostgreSQL, JetStream, Redis, and ElecPostal. Configure TLS certificates
for all enabled SMTP, IMAP, and POP3 listeners.

## JMAP

JMAP is available over the main HTTPS listener at `GET /jmap/session` and
`POST /jmap/api`. It uses the same bearer-token authentication as the REST API.
Each hosted address is a JMAP account and IMAP folders are JMAP Mailboxes.
This initial support includes `Core/echo`, `Mailbox/get`, `Mailbox/query`,
`Email/get`, `Email/query`, and `Email/set` for flags, moves, and trashing.
JMAP reads and updates the same message state as IMAP and POP3.

## Outbound delivery

Set `mail.relay.adapter = "direct-smtp"` to deliver directly to recipient MX
records without a relay. `mail.relay.host` must be this server's public mail
hostname for EHLO. Direct delivery uses TCP port 25 and opportunistically uses
STARTTLS; set `mail.relay.tlsMode = "required"` to reject MX servers that do
not support STARTTLS.

Set `mail.relay.adapter = "ses"` to use the AWS SDK's SES API v2 client.
Set `mail.relay.region` and authenticate through the AWS SDK default credential
chain (for example, an IAM role, `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`, or
an AWS shared profile). SES SMTP credentials and `mail.relay.host` are not used
by this adapter. The same adapter supports workspace-owned custom domains
through `/api/custom-domains`; grant its IAM principal
`ses:CreateEmailIdentity`, `ses:GetEmailIdentity`, and `ses:DeleteEmailIdentity`
in addition to the send permission. AWS credentials are never sent by clients
or stored in ElecPostal.

For either adapter, set `mail.relay.inboundHost` to this service's public MX
hostname. If DNS resolves a recipient domain to that host, ElecPostal stores
the message directly in its local mailbox rather than handing it to the
external delivery path.

Other delivery providers, including SES, are implemented as adapters behind the
same outbound-delivery contract. Attachment IDs require a DysonFS byte source;
until that is configured, enabled delivery rejects messages with attachments
rather than dropping them.
