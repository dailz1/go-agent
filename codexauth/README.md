# Codex subscription login

This satellite owns interactive login and the independent credential file.
`llm/codex/auth` owns non-interactive token refresh; `llm/codex` owns Responses
requests. Neither kernel package reads your home directory or starts a browser.

## Login

Run these commands from the repository root:

```sh
go run ./examples/codexauth login
go run ./examples/codexauth status
```

When stdin is a TTY, `login` first asks you to choose an authorization method:

1. **Open the default browser** (the default when you press Enter). If opening
   the browser fails, open the printed authorization URL manually.
2. **Copy the authorization URL and open it manually**, including from a personal
   computer for remote/SSH use. This choice never attempts to open a browser.

`login --headless` skips the chooser and always uses manual URL authorization.
With non-TTY stdin, `login` also defaults to manual authorization, prints a
notice, and does not prompt or attempt to open a browser.

After the choice or override, login binds `127.0.0.1:1455` before printing the
authorization URL. The registered redirect is
`http://localhost:1455/auth/callback`; a busy port is an error, not a reason to
choose another port. Ctrl+C cancels login and closes any active listener.

`status` reads the local store without refreshing credentials or making network
requests. It prints JSON containing the masked `account`, `expires_at`, and
`needs_login`; successful login prints the same status fields. Authorization
URLs and instructions go to stderr, while status JSON goes to stdout.

For a remote machine, forward the port **before** starting login:

```sh
# Personal computer:
ssh -L 1455:127.0.0.1:1455 user@server
# In that remote shell:
go run ./examples/codexauth login --headless
```

Open the printed URL in your personal computer's browser. Its localhost callback
travels through SSH to the remote listener. Device flow and manually pasting a
callback URL/code are not supported.

The default store is `~/.go-agent/codex/auth.json`. Both `login` and `status`
accept `--store /absolute/path` after the subcommand for a different independent
store. Login and runtime use an exclusive `.lock`
file. If a process crashes, confirm the PID in that file has stopped before
manually removing the lock; the library never steals it based on age.

The owner corrected this profile on 2026-09-22 after a live `invalid_client`
response, cross-checking an independent implementation and the Codex CLI binary.
The previous client ID and connector scopes belonged to another context.

- Issuer: `https://auth.openai.com`
- Client ID: `app_EMoamEEZ73f0CkXaXp7hrann`
- Redirect: `http://localhost:1455/auth/callback`
- Scope: `openid profile email offline_access`
- Extra authorization parameters: `id_token_add_organizations=true`,
  `codex_cli_simplified_flow=true`, `originator=go_agent`

Inference sends the mandatory `chatgpt-account-id` header from the access-token
JWT's `https://api.openai.com/auth` → `chatgpt_account_id` claim. Returned refresh
tokens replace the previous value and are persisted before use.

Binary evidence is not a claim of completed live acceptance. Real login,
refresh, inference with `originator: go_agent`, and coexistence with a separate
official CLI authorization must be checked against the live service. A private
backend/profile change requires review rather than automatic parameter changes.

## Embed

```go
path, err := codexauth.DefaultPath()
if err != nil { return err }
source, err := codexauth.OpenFileSource(path, auth.RefreshConfig{})
if err != nil { return err }
defer source.Close()

provider := codex.NewProvider(model, codex.WithAuthSource(source))
// Share source among providers in this process. Stop their calls before Close.
```

`OpenFileSource` loads your independent store and holds its lock until `Close`.
`RefreshConfig.Persist` is replaced by this store's atomic save callback.
Refresh-token rotation is persisted before a new access token is published.
A save failure blocks inference and the next `Refresh` retries saving, not OAuth.
If the refresh result is lost after the POST, login is required: replaying an old
refresh token could corrupt the rotation family.

For an existing platform credential manager, implement `auth.Source` and inject
it directly. `Token` must only return a snapshot. `Refresh` owns all proactive
expiry checks, 401 handling, concurrency, and persistence.

`OpenOfficialSource("/explicit/path/to/.codex/auth.json")` is read-only,
access-token-only compatibility. It never refreshes or writes official
credentials. At expiry, within 60 seconds of expiry, or following a matching 401,
it returns `auth.ErrLoginRequired`. Separate copies of a refresh token do not
create separate authorizations. API-key credentials belong in `openairesponses`.

## Limits and errors

Codex always uses the server's default output limit. Positive `WithMaxTokens`
values are accepted but **not sent**; output is not guaranteed to stay below the
configured value. Applications must disclose
`configured_max_output_tokens` and `output_limit_mode=server_default` at startup.
The provider also warns once when consuming a positive cap. Temperature and Stop
are not supported.

The default originator is `go_agent`. `codex.WithOriginator("codex_cli_rs")` is an
explicit compatibility opt-in, never an automatic response to rejection.
There is no API-key fallback, automatic account switch, or stream restart.

Quota errors retain their HTTP status with `llm.APIError.NonRetryable=true`.
Other 429/5xx errors retain existing retry classification, but iterator errors do
not trigger the agent's outer pre-stream retry loop. Recording v3 preserves
these classifications and reads v1/v2 recordings without rewriting them.
