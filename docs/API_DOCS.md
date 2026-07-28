# ElecPostal API

Base path: `/api`

All endpoints except `/health` require an authenticated account. Send a Solar
access token using the normal `Authorization` header. For local development,
when authentication is disabled, `X-Account-Id` may be used with an account UUID.

Errors have this shape:

```json
{"error":"description"}
```

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

## Emails

### List emails

`GET /api/emails?offset=0&take=20`

`GET /api/mailboxes/{mailbox-id}/emails?offset=0&take=20`

`take` defaults to `20` and is capped at `200`. The `X-Total` response header
contains the number of matching emails.

### Get an email

`GET /api/emails/{email-id}`

Returns the email with its mailbox, recipients, and attachments.

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

### Delete an email

`DELETE /api/emails/{email-id}`

Soft-deletes the email and returns:

```json
{"ok":true}
```

## Delivery behavior

When the service is configured with the `direct-smtp` outbound adapter,
non-draft messages are delivered directly to recipient MX records. Delivery
errors are returned by `POST /api/emails`; a message is not persisted when
direct delivery fails. Messages with `attachment_ids` are rejected while no
DysonFS attachment-byte source is configured, preventing attachments from being
silently omitted.

### Mark read or unread

`POST /api/emails/{email-id}/read`

`POST /api/emails/{email-id}/unread`

Both return `{"ok":true}`.

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
