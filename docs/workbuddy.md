# WorkBuddy (CodeBuddy CN) provider

CLIProxyAPI can proxy WorkBuddy / CodeBuddy (China mainland) accounts through
the native `workbuddy` provider. The provider uses the upstream device
authorization flow and exposes the usual OpenAI-compatible and Anthropic
Messages endpoints through the existing translators.

This provider is **CN-only**. It does not implement the international
(`workbuddy.ai`) realm or cross-domain fallback.

## Configuration

```yaml
workbuddy:
  # Enables the scheduled account task runner.
  enabled: true

  # Directory for task state files. Empty means <auth-dir>/workbuddy-state.
  state-dir: ""

  # Maximum number of accounts processed concurrently. 0 uses the default (4).
  workers: 4

  # Timeout for non-chat task requests.
  request-timeout: "30s"

  # IANA timezone used to evaluate daily task windows.
  timezone: "Asia/Shanghai"

  # Per-task schedules. A task without "enabled" is enabled by default.
  tasks:
    checkin:      {enabled: true, daily-at: "08:00"}
    balance:      {enabled: true, interval: "6h"}
    keepalive:    {enabled: true, interval: "30m"}
    report:       {enabled: true, interval: "1h"}
    growth:       {enabled: true, interval: "6h"}
    travel:       {enabled: true, interval: "12h"}
    night-owl:    {enabled: true, daily-at: "01:00"}
    sweep:        {enabled: true, daily-at: "23:30"}
    compensation: {enabled: true, daily-at: "09:00"}
    streak:       {enabled: true, daily-at: "08:10"}
```

The `workbuddy` section is optional. When it is absent, the provider is not
started and the section is omitted from saved configuration. `workers` is
capped at 64. `workbuddy` is a v8-only section: adding it to a legacy-layout
config makes the document count as v8 on the next management save, so prefer
`config-version: 8` from the start.

## Login

Start a device authorization flow through the v8 management API:

```bash
curl -s 'http://127.0.0.1:8317/v8/management/oauth/auth-url?provider=workbuddy' \
  -H "Authorization: Bearer <management-key>"
```

The response contains `url`, `state`, and `flow: "device"`. Open the URL,
authorize the account, then poll:

```bash
curl -s 'http://127.0.0.1:8317/v8/management/oauth/status?state=<state>' \
  -H "Authorization: Bearer <management-key>"
```

When the status becomes `ok`, the credential is stored as
`<auth-dir>/workbuddy-<uid>.json`.

## Importing existing credentials

Credential files exported by the WorkBuddy panel are recognized directly. Both
the flat form and the panel-nested form are supported:

```json
{
  "auth": {
    "accessToken": "...",
    "refreshToken": "...",
    "expiresAt": 1767225600,
    "domain": "copilot.tencent.com"
  },
  "account": {
    "uid": "...",
    "enterpriseId": "...",
    "nickname": "..."
  },
  "device_token": "..."
}
```

Place the file in the auth directory as `workbuddy-<uid>.json`. Files without a
`type` field are recognized by their credential shape plus an account UID or the
`workbuddy` file-name prefix. Other providers are never re-typed.

## Models

The provider ships a static seed catalog in `internal/registry/models/models.json`
(DeepSeek, GLM, Kimi, MiniMax entries). Remote catalog refreshes that omit the
`workbuddy` section preserve the embedded list.

Use the model ID directly, or prefix it with `workbuddy/` when the client needs
an explicit provider prefix.

## Scheduled tasks

| Task | Default schedule | Notes |
| --- | --- | --- |
| `checkin` | 08:00 daily | Skips enterprise accounts; treats "already checked in" as success. |
| `balance` | every 6h | Personal and enterprise resource endpoints. Writes task state only. |
| `keepalive` | every 30m | Refreshes the OAuth token through the auth manager. |
| `report` | every 1h | Posts `chat_request_send` activity events. |
| `growth` | every 6h | Scans, accepts, and claims growth tasks (default and mini-program). |
| `travel` | every 12h | Buddy adoption, departure, and reward claim. |
| `night-owl` | 01:00 daily | Only runs inside the 23:00-08:00 window. |
| `sweep` | 23:30 daily | Accepts and claims already-completed tasks. |
| `compensation` | 09:00 daily | Claims active compensation when available. |
| `streak` | 08:10 daily | Streak status, makeup cards, tier redemption, lottery, gift. |

Task state is stored per account as
`<auth-dir>/workbuddy-state/<auth-id>.wbt`. The directory is separate from the
auth directory so the credential watcher does not parse it.

Enterprise accounts skip personal-only tasks (check-in, growth, travel,
compensation, streak) and use the enterprise balance endpoint. Accounts using an
enterprise-switched session skip activity and growth-style tasks.

The task runner is single-instance only. State files are local; do not run
multiple CLIProxyAPI instances against the same auth directory without external
coordination.

## Limitations

- CN realm only; no international realm or cross-domain fallback.
- Chat upstream always streams; non-streaming clients are served by aggregating
  the upstream SSE stream.
- Image, video, and speech endpoints are not implemented for this provider.
- `count_tokens` uses a local tokenizer estimate, not an upstream count.
- Growth tasks that require interactive expert/desktop actions are only
  partially automated; accept/claim and simple event actions are implemented.
- Outbound identity headers mirror the official 5.5.4 / 2.137.1 client shape and
  may need updates when upstream risk-control rules change.
