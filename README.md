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
- `ring.target` (optional Ring gRPC endpoint for account notifications; pushes target the `dev.solsynth.solarwatt` app, are localized to the recipient's account language via `auth.target`, and surface the extracted verification code or key sentence instead of a leading excerpt)
- `personality.target` (optional Persona gRPC endpoint for opt-in AI mail summaries; `personality.agent` names the summarizing agent. Summaries are stored on the message, used as its list preview, and metered by Persona against the account's own usage limits and billing)
- `mail.domain` (canonical mail domain; local-only mailbox addresses are completed with this domain before outbound delivery and exposed via `GET /api/mail/host`)
- `mail.spam.*` (inbound spam filter: SPF/DKIM/DMARC sender authentication, weighted content rules, and an optional global Bayes classifier. `mail.spam.threshold` is the score at which mail is filed into Spam, `mail.spam.addXSpamHeader` writes `X-Spam-Status`/`X-Spam-Score` into the stored source, and `mail.spam.bayes.*` tunes the classifier that `redis.addr` backs. Mail is never rejected at SMTP)

## Attachments

Clients upload outgoing attachments to DysonFS first, then submit the returned
file IDs as the `attachment_ids` string array to `POST /api/emails`. Inbound
delivery and protocol APPEND stream decoded attachment bytes to DysonFS before
ElecPostal persists message metadata. ElecPostal never stores attachment bytes
in PostgreSQL or durable NATS jobs.

## Workspace email quota

- Mailboxes belong to a workspace. ElecPostal accounts for message text,
  headers, recipients, and manifest metadata in its own quota. DysonFS owns
  attachment bytes and reports that storage separately; Valve aggregates both
  services and periodically refreshes the workspace usage snapshot. When the
  shared limit is exceeded, ElecPostal archives the oldest messages and
  permanently removes their metadata after 30 days.

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

ElecPostal stores protocol metadata and an attachment-free MIME manifest with
each message. DysonFS is the only durable attachment-byte store. IMAP and POP3
generate RFC 5322 responses on demand by streaming referenced DysonFS files;
boundaries are generated per response. This pre-release service uses GORM
`AutoMigrate` plus a startup migration for legacy raw sources. The provided
`docker-compose.yml` starts PostgreSQL, JetStream, Redis, and ElecPostal.
Configure TLS certificates for all enabled SMTP, IMAP, and POP3 listeners.

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
`ses:CreateEmailIdentity`, `ses:GetEmailIdentity`, `ses:DeleteEmailIdentity`,
and `ses:PutEmailIdentityMailFromAttributes` in addition to the send
permission. AWS credentials are never sent by clients or stored in ElecPostal.

For either adapter, set `mail.relay.inboundHost` to this service's public MX
hostname. If DNS resolves a recipient domain to that host, ElecPostal stores
the message directly in its local mailbox rather than handing it to the
external delivery path.

Other delivery providers, including SES, are implemented as adapters behind the
same outbound-delivery contract. Attachment IDs require a DysonFS byte source;
until that is configured, enabled delivery rejects messages with attachments
rather than dropping them.
