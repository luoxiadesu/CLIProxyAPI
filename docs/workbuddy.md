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

## Task status

The v8 management API exposes the configured task plan and the persisted state
for every WorkBuddy account:

```bash
curl -s 'http://127.0.0.1:8317/v8/management/workbuddy/tasks' \
  -H "Authorization: Bearer $MANAGEMENT_KEY"
```

This endpoint exists only under `/v8/management`; there is no legacy
`/v0/management` equivalent. The response contains:

- `enabled` and `home_enabled`: whether the local task runner is enabled and
  whether Home mode is active (Home mode owns account state remotely).
- `state_dir` and `timezone`: the resolved state directory and the configured
  IANA timezone.
- `observed_at`: when the response was generated.
- `tasks`: the fixed plan of ten tasks (`checkin`, `balance`, `keepalive`,
  `report`, `growth`, `travel`, `night-owl`, `sweep`, `compensation`,
  `streak`) with `enabled`, `interval`, `daily_at`, and `timeout`.
- `accounts`: registered WorkBuddy auth records joined with their state files.
  State files without a matching auth record (and every state file when no auth
  manager is registered) are returned with `orphan: true`.

Each account carries a `tasks` map keyed by task name. A task entry may contain:

| Field | Meaning |
| --- | --- |
| `last_run` | Last attempt time. |
| `last_window` | Daily window covered by the last run. |
| `last_status` | `ok` or `error`. |
| `last_error` | Failure message from the last run. |
| `next_retry` | Earliest retry instant after a failure. |
| `consecutive_failures` | Failure count driving the retry backoff. |
| `data` | Task-specific values written back by the task (for example `credit`). |

`next_retry` is only a retry hint after a failed run; it is not the regular
schedule. The regular cadence comes from the configured `interval` and
`daily_at` values in `tasks`. `data` contains only metrics a task persisted for
itself and never includes credential material.

## Management panel

The official WebUI supports WorkBuddy out of the box: the login page shows a
device-code card for the OAuth flow, the configuration editor exposes the
`workbuddy` section, and `/workbuddy/tasks` renders the task status view backed
by the endpoint above.

To host a custom panel build instead, name the single-file artifact
`management.html`, place it in a directory, and point `MANAGEMENT_STATIC_PATH`
at that directory. The server serves the file from there and skips the remote
asset update. Alternatively, fork the panel repository, publish a release, and
set `remote-management.panel-github-repository` to the fork so the server
downloads `management.html` from that release.

## Upstream channel checks

The upstream rejects chat requests that do not look like they come from an
official client:

- the first message must carry role `system`; a user/developer first message is
  answered with `11-128` (`first message is not system prompt` / `Illegal API
  invocation from an unapproved channel`);
- content moderation matches several exact substrings copied verbatim from
  official CLI clients, including Claude Code and Codex system-prompt sentences,
  `x-anthropic-billing-hdr` segments, and the bare digits `11-128`.

The executor normalizes every outbound chat payload before sending it:

- a neutral system message is prepended when the first message is not `system`;
- `developer` roles become `system`;
- the known moderation fingerprints are rewritten or stripped in message text,
  reasoning fields, and tool-call arguments.

The rewrites keep the original meaning and run before user payload rules, so
configured overrides remain the final word. Scheduled night-owl chat requests
include a system message for the same reason.

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
