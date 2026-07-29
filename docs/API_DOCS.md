# ElecPostal API

Base path: `/api`

All endpoints except `/health` and `/api/mail/host` require an authenticated account. Send a Solar
access token using the normal `Authorization` header. For local development,
when authentication is disabled, `X-Account-Id` may be used with an account UUID.

Errors have this shape:

```json
{"error":"description"}
```

## Configuration

### Mail host

`GET /api/mail/host`

Returns the configured canonical mail domain. Clients can use it to render
local-only mailbox addresses as full email addresses.

```json
{"host":"example.com"}
```

When no domain is configured, `host` is an empty string.

## Mailboxes

### List mailboxes

`GET /api/mailboxes?workspace_id={workspace-id}`

Omit `workspace_id` to list mailboxes owned directly by the authenticated
account.

```json
[
  {
    "id": "01J...",
    "account_id": "6d0a54f1-...",
    "workspace_id": "01J...",
    "address": "alice@example.com",
    "name": "Alice",
    "is_default": true,
    "is_verified": false
  }
]
```

### Create a mailbox

`POST /api/mailboxes`

```json
{
  "workspace_id": "01J...",
  "address": "alice@example.com",
  "name": "Alice",
  "is_default": true
}
```

Returns `201 Created` with the mailbox.

Creating a mailbox requires the authenticated account to be an active member of
the selected workspace. Workspace plans allow 1 mailbox on Free, 3 on Pro, and
10 on Enterprise.

### Mailbox email quota

`GET /api/mailboxes/{mailbox-id}/quota`

Returns the raw-email allocation for the mailbox's workspace. The allocation is
10% of the workspace plan's storage quota; attachment bytes are excluded because
DysonFS has already accounted for them.

```json
{
  "workspace_id": "01J...",
  "used_bytes": 268435456,
  "limit_bytes": 1073741824,
  "remaining_bytes": 805306368
}
```

Once this allocation is exceeded, the oldest messages are archived and given a
30-day deletion deadline. Archived messages are omitted from normal email lists.

## Emails

### List emails

`GET /api/emails?offset=0&take=20`

`GET /api/mailboxes/{mailbox-id}/emails?offset=0&take=20`

`take` defaults to `20` and is capped at `200`. The `X-Total` response header
contains the number of matching emails.

The list endpoints support mailbox-style discovery filters. Combine them as
needed:

| Parameter | Description |
| --- | --- |
| `q` | Case-insensitive match against subject, body, sender, and recipients. |
| `is_read` | `true` or `false`. |
| `is_starred` | `true` or `false`. |
| `is_draft` | `true` or `false`. |
| `delivery_status` | Exact delivery state such as `sent`, `failed`, `pending`, or `draft`. |
| `label_id` | Only messages carrying this tag. |
| `folder` | `inbox`, `sent`, `drafts`, `spam`, `trash`, or `archive`. |

For filter counts and navigation badges, use `GET /api/emails/stats` or
`GET /api/mailboxes/{mailbox-id}/stats`. Both return a total, unread, starred,
and draft count plus a count by delivery state.

### Get an email

`GET /api/emails/{email-id}`

Returns the email with its mailbox, recipients, and attachments.

### Conversations

`GET /api/threads?folder=inbox&offset=0&take=20` returns one summary per
conversation. `GET /api/mailboxes/{mailbox-id}/threads` scopes the same list to
a mailbox, and `GET /api/threads/{thread-id}` returns the complete timeline in
chronological order.

Every new message receives a `thread_id`. To reply, provide either the existing
`thread_id` or `reply_to_id` (an email ID) to `POST /api/emails`; the latter
automatically joins the parent message's conversation. The two fields are
intentionally mutually exclusive.

### Send an email

`POST /api/emails`

```json
{
  "mailbox_id": "01J...",
  "to": [
    {"address": "recipient@example.net", "name": "Recipient"}
  ],
  "cc": [],
  "bcc": [],
  "subject": "Hello",
  "body": "Message text",
  "attachment_ids": ["01J-file-id-1", "01J-file-id-2"],
  "is_draft": false
}
```

Upload files to DysonFS before calling this endpoint, then provide its returned
file IDs in `attachment_ids`. ElecPostal does not accept attachment bytes on
this endpoint.

Recipient `kind` can be `to`, `cc`, or `bcc`; the array used supplies the
default when it is omitted.

Returns `201 Created` with the stored email.

Outbound send limits are enforced per mailbox and per workspace for daily and
monthly calendar periods. Exceeding a limit returns `429 Too Many Requests`.
The plan-specific values are configured by the service operator under
`mail.sendLimits`; drafts do not consume quota and scheduled mail consumes it
when delivered.

Non-draft emails include delivery metadata: `delivery_status` (`pending`,
`sent`, `failed`, or `not_configured`), `delivery_attempts`, the last attempt
time, an optional provider message ID, and an error when delivery failed.

### Resend an email

`POST /api/emails/{email-id}/resend`

Retries a previously sent email using its original recipients and content. The
email's delivery status, attempt counter, provider message ID, and any delivery
error are updated. Drafts cannot be resent.

### Delete an email

`DELETE /api/emails/{email-id}`

Soft-deletes the email and returns:

```json
{"ok":true}
```

## Delivery behavior

When an outbound adapter is configured, non-draft messages are delivered to the
recipient provider or MX records. Every attempt is persisted. On failure the
message remains available with a `failed` delivery status and can be retried
with the resend endpoint. Messages with `attachment_ids` are rejected while no
DysonFS attachment-byte source is configured, preventing attachments from being
silently omitted.

### Mark read or unread

`POST /api/emails/{email-id}/read`

`POST /api/emails/{email-id}/unread`

Both return `{"ok":true}`.

### Folders and spam

New inbound mail arrives in `inbox`; sent messages and drafts are stored in
`sent` and `drafts`. `DELETE /api/emails/{email-id}` moves a message to Trash.
Move a message explicitly with `POST /api/emails/{email-id}/move`:

```json
{"folder":"archive"}
```

`POST /api/emails/{email-id}/spam` moves a message to Spam, while
`POST /api/emails/{email-id}/not-spam` restores it to Inbox. Incoming mail that
matches a block rule or the built-in conservative spam heuristic is retained in
Spam for review.

### Scheduled and HTML email

`content_type` accepts `text/plain` (default) or `text/html` when sending.
Set a future RFC3339 `scheduled_at` in `POST /api/emails` to queue a message;
the delivery worker sends it at or after that timestamp.

## Blocklist

`GET /api/blocklist` lists rules, `POST /api/blocklist` creates one, and
`DELETE /api/blocklist/{id}` removes it. Rules can target a whole workspace or
one mailbox and match either an exact address or a domain:

```json
{"scope":"workspace","workspace_id":"01J...","pattern":"spam.example"}
```

```json
{"scope":"mailbox","mailbox_id":"01J...","pattern":"sender@example.com"}
```

## Real-time updates

When `websocket.target` is configured, ElecPostal publishes `mail.created`
packets through the DysonNetwork WebSocket gateway under the
`dev.solsynth.solarwatt` namespace. The packet data is JSON-encoded email data.

When `ring.target` is configured, each new Inbox email also sends a standard
account push notification with the email ID in its metadata. Spam messages do
not trigger user notifications.

### Star and unstar

`POST /api/emails/{email-id}/star`

`POST /api/emails/{email-id}/unstar`

Both return `{"ok":true}`.

## Tags

Tags are account-owned labels. Their data is included in each listed or fetched
email under `labels`.

### List and create tags

`GET /api/labels`

`POST /api/labels`

```json
{"name":"Receipts","color":"#16a34a"}
```

### Apply, remove, and delete tags

`POST /api/emails/{email-id}/labels/{label-id}` applies a tag.

`DELETE /api/emails/{email-id}/labels/{label-id}` removes it.

`DELETE /api/labels/{label-id}` deletes the tag and its mappings.

All successful tag mutation endpoints return `{"ok":true}`.

## Credentials

Credentials are dedicated, revocable app passwords for mail protocols. They are
not valid for HTTP API authentication.

### List credentials

`GET /api/credentials`

The secret and its hash are never returned.

### Create a credential

`POST /api/credentials`

```json
{
  "label": "Thunderbird on laptop",
  "protocols": ["smtp", "imap"]
}
```

Allowed protocol scopes are `smtp`, `imap`, and `pop3`. The response is `201
Created` and includes a `secret`; save it immediately because it is shown only
once.

```json
{
  "credential": {
    "id": "01J...",
    "account_id": "6d0a54f1-...",
    "label": "Thunderbird on laptop",
    "protocols": ["smtp", "imap"],
    "created_at": "2026-07-28T12:00:00Z"
  },
  "secret": "save-this-once"
}
```

### Revoke a credential

`DELETE /api/credentials/{credential-id}`

Returns `{"ok":true}`. Revocation immediately prevents future mail-protocol
logins with that secret.

## Health

`GET /health`

Returns:

```json
{"ok":true,"service":"elecpostal"}
```

ElecPostal also implements the standard gRPC health service on the configured
gRPC port (default `9090`). Gateways may check either the aggregate service name
`""` or the explicit service name `"elecpostal"`; both report `SERVING` after
startup.
