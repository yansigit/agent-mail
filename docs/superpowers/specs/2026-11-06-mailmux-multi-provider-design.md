# mailmux: multi-provider mail for agents — design

- **Date:** 2026-11-06
- **Status:** draft, awaiting review
- **Scope of this spec:** version 1.0 of mailmux (mail only)
- **Background research:** "Open source mail provider survey" (session report, 2026-11-06)

## 1. Summary

mailmux is a fork of `rlrghb/olkcli` (olk), an MIT-licensed Go CLI and MCP server
for Microsoft Outlook. mailmux adds Gmail (through the Gmail REST API) and generic
IMAP/SMTP providers, lets one MCP server work across several accounts (including
combined reads across providers), and adds stronger agent-safety controls. It is
published as a separate product, but keeps merging upstream olk, so almost all new
code lives in new packages and upstream files change only at a few hook points.

## 2. Decisions

| # | Question | Decision |
|---|---|---|
| D1 | What is the fork for? | A separate multi-provider product with its own name and release channels. |
| D2 | Keep merging upstream? | Yes. New code goes beside upstream code; upstream files change only at hook points. |
| D3 | Scope of 1.0 | Mail only, for Outlook (existing), Gmail (REST API) and generic IMAP/SMTP. Calendar, contacts, To Do and OneDrive stay Outlook-only. |
| D4 | POP3 | Dropped. |
| D5 | Gmail sign-in | Users register their own Google OAuth client ("bring your own"), helped by a setup command; Gmail over IMAP with an app password works as a fallback through the IMAP provider. Tokens are stored per named client so a verified shared client can be added later without migration. No other application's client ID is ever shipped or documented. |
| D6 | Microsoft app | mailmux registers its own Entra app instead of using upstream's client ID. |
| D7 | Accounts in MCP | One server can hold several accounts, chosen per call from a launch-time allowlist, and read tools can work across all of them at once. |
| D8 | Search syntax | A Gmail-style query string with a fixed operator set, parsed into a syntax tree and translated per provider. Structured flags build the same tree. `--raw` passes a native query through for one account. |
| D9 | Safety in 1.0 | Everything from the research shortlist except the approval queue: send lock, recipient allowlist and new-recipient check, persisted daily send cap, per-account policy file, Gmail scopes per tier, explicit-ID caps, content cleaning under MCP. |
| D10 | Name | `mailmux`. Applied at build and release time; the Go module path stays `github.com/rlrghb/olkcli`. |
| D11 | Architecture | Approach A: a dispatch hook routes commands either to upstream's own `Run` or to mailmux handlers (section 4). |

## 3. Goals and non-goals

**Goals**

1. Every mail command that olk has works for Gmail and IMAP accounts, or fails with a
   clear `capability-missing` error before signing in.
2. One MCP server can serve several accounts and answer combined reads (list, search,
   folders, sync) across providers.
3. Every send, for every provider, passes through one safety layer.
4. Upstream merges stay cheap: about ten changed lines in upstream files in total.
5. Same output contract as olk: the JSON shape of results is unchanged, with added
   `account` and `provider` fields.

**Non-goals for 1.0**

- POP3; JMAP; calendar, contacts or tasks for non-Microsoft providers.
- IMAP OAuth (XOAUTH2 / OAUTHBEARER).
- An approval queue for blocked sends.
- A Google-verified shared OAuth client.
- Importing existing olk accounts into mailmux.
- Gmail settings (filters, forwarding, vacation replies).

## 4. Architecture

### 4.1 Keeping upstream merges cheap

1. **Separate main package.** `cmd/mailmux/main.go` is ours; `cmd/olk` stays
   untouched and still builds plain `olk`. The release builds `cmd/mailmux` with
   `-X internal/config.Namespace=mailmux`, which gives mailmux its own config
   directory and keychain service (upstream already names keychain storage after
   the namespace).
2. **Our files inside upstream packages,** named `mux_*.go` (for example
   `internal/cmd/mux_hooks.go`, `internal/msauth/mux_lock.go`). They can use
   upstream's unexported helpers and never conflict in a merge.
3. **Hook edits in upstream files** (the only upstream edits):

   | File | Edit |
   |---|---|
   | `internal/cmd/root.go` | `ctx.Run(runCtx)` → `runSelected(ctx, runCtx)`; `kong.Parse(...)` also receives `extraKongOptions...` |
   | `internal/cmd/mcp_invoke.go` | `kctx.Run(runCtx)` → `runSelected(kctx, runCtx)` |
   | `internal/cmd/mcp_server.go` | the tool loop ranges over `allCuratedTools()` (upstream's `curatedTools` plus ours); `newKongParser` receives `extraKongOptions...`; one filter hook for per-account capability filtering and the `account` argument |
   | `internal/config/config.go` | `const DefaultClientID` → `var DefaultClientID`, so the build can set mailmux's own Microsoft app ID (worth offering upstream as a pull request) |

   `runSelected`, `extraKongOptions`, the extra tools and the hook variables live in
   `internal/cmd/mux_hooks.go`. When no router is registered, `runSelected` calls
   `kctx.Run`, so plain `olk` behaves exactly as before.
4. **New commands and global flags** (`accounts`, `setup google`, `policy`,
   `mail labels`, `--accounts`, `--sanitize-content`) are added with `kong.Embed`
   through `extraKongOptions`, not by editing upstream's `CLI` or `RootFlags` structs.
5. **Environment variables.** Before parsing, `cmd/mailmux` copies each `MAILMUX_*`
   variable to the matching `OLK_*` name unless that `OLK_*` name is already set.

### 4.2 New packages (`internal/mux/…`)

| Package | Responsibility |
|---|---|
| `core` | Provider interfaces, shared result types, error sentinels with stable codes |
| `capability` | Capability model (`Full`, `Partial(detail)`, `Server(detail)`, `None(detail)`) and one table per provider |
| `query` | Search parser → syntax tree; translators to KQL, Gmail query and IMAP SEARCH |
| `accounts` | Account registry across Outlook (upstream config) and our `accounts.json` |
| `graphprov` | Adapter over the unchanged `graphapi.Client` |
| `gmailprov` | Gmail backend (wraps `google.golang.org/api/gmail/v1`) |
| `imapprov` | IMAP + SMTP backend (wraps `emersion/go-imap/v2` and `emersion/go-smtp`; the go-imap beta API is confined to this package) |
| `compose` | Builds outgoing RFC 5322 messages with `jhillyerd/enmime/v2` |
| `googleauth` | Google loopback sign-in with PKCE, named clients, `setup google` |
| `autoconfig` | IMAP/SMTP server discovery |
| `guard` | Safety decorator wrapped around every provider |
| `policy` | Per-account policy file, send log, send cap |
| `route` | Router, cross-account fan-out and merge, command handlers and their output |

### 4.3 Routing

For each command (CLI or MCP), `runSelected` asks the router:

1. **Resolve accounts** from `--account`, `--accounts a,b|all`, or the default account.
2. **Check capabilities** for each account's provider, without connecting. If the
   provider can't serve the command or a search operator, fail with
   `capability-missing` or `search-unsupported` before any sign-in.
3. **Choose the code path:**
   - **Upstream `Run`:** exactly one Outlook account, a command upstream has, not a
     send-tier command, and (for searches) a query whose KQL translation is exact.
     The router checks the account's policy ceiling first and translates mailmux
     search syntax to KQL in the parsed command struct.
   - **mailmux handler:** everything else, meaning Gmail or IMAP accounts, more than
     one account, mailmux-only commands, KQL translations that need filtering after
     fetching, and **every send-tier command for every provider** (so the guard layer
     sees actual recipients). Handlers read the flags kong already parsed into
     upstream's command struct and call `core` interfaces through `guard`.
   - **Error:** a non-mail command (calendar, contacts, To Do, drive, people) on a
     non-Outlook account returns `capability-missing`.

### 4.4 Output contract

- mailmux handlers print upstream's result structs (for example
  `graphapi.MailMessage`) embedded in a wrapper that adds `account`, `provider` and,
  where relevant, `labels`. JSON output therefore matches upstream plus those fields.
- Table and TSV output for handler-served commands is reimplemented to match
  upstream's layout, with an added account column when more than one account is
  shown.
- **Risk to check first (M0):** the untrusted-text wrapper (`outfmt.wrapUntrusted`)
  must descend into embedded structs. If it does not, either wrap the fields
  explicitly (not embed), or add a `mux_*.go` helper in `outfmt`.

## 5. Provider model

### 5.1 Interfaces (`core`)

```go
type Reader interface {
    Folders(ctx context.Context) ([]Folder, error)
    List(ctx context.Context, opts ListOpts) (Page[Message], error)
    Get(ctx context.Context, id ID, body BodyPref) (*Message, error)
    Search(ctx context.Context, q query.Node, opts PageOpts) (Page[Message], error)
}

// Optional; discovered by type assertion and declared in the capability table.
type Labeler     interface { ModifyLabels(ctx context.Context, ids []ID, add, remove []string) error }
type Mover       interface { Move(ctx context.Context, ids []ID, dest FolderRef) ([]ID, error) }
type Flagger     interface { SetFlags(ctx context.Context, ids []ID, change FlagChange) error }
type Deleter     interface { Trash(ctx context.Context, ids []ID) error; Purge(ctx context.Context, ids []ID) error }
type DraftStore  interface { CreateDraft(...); UpdateDraft(...); SendDraft(...) }
type Sender      interface { Send(ctx context.Context, msg Outgoing) (SendResult, error) }
type DeltaSyncer interface { Delta(ctx context.Context, scope Scope, tok Token) (Delta, error) }
type Attachments interface { ListAttachments(...); GetAttachment(...) }
```

- `(...)` marks signatures left to the implementation plan.
- Every changing method takes explicit IDs. There is no "change everything matching
  a query" operation. Searching and acting are separate calls.
- `Move` returns the new IDs (IMAP IDs change on move; Graph immutable IDs and Gmail
  IDs do not).
- `ErrCapabilityMissing`, `ErrSearchUnsupported` and `ErrStaleID` are sentinels
  (`errors.Is`) that carry a stable code (section 9).

### 5.2 Capability tables (`capability`)

- One table per provider, mapping each named capability (each command, flag and
  search operator) to `Full`, `Partial(detail)`, `Server(detail)` (depends on server
  extensions, checked on connect) or `None(detail)`. Modelled on himalaya v3
  (`src/capability.rs`, MIT OR Apache-2.0).
- The same table drives: refusals before sign-in, `--help` notes, MCP tool filtering,
  the capability list reported by `mailmux version`, and the search translator tests.
- Examples: Gmail `flag:answered` → `None("Gmail has no label for it")`; IMAP
  `has:attachment` → `None("IMAP SEARCH has no attachment key")`; IMAP
  `UpdateDraft` → `Server("clean only with UIDPLUS")`.

### 5.3 Message IDs

- IDs are provider-native and scoped to an account. Every result carries `account`
  and `provider`; follow-up calls name the account (`--account` or the MCP `account`
  argument). IDs do not embed the account, because upstream's `Run` prints raw Graph
  IDs and both Outlook paths must print the same thing.
- **Outlook:** Graph IDs, with upstream's `--immutable-ids` on by default in mailmux.
- **Gmail:** Gmail message IDs (immutable).
- **IMAP:** base64url encoding of `(mailbox, UIDVALIDITY, UID)`, after aerc. Before
  acting, compare the encoded UIDVALIDITY with the mailbox's current one; on mismatch
  return `stale-id`. After MOVE or COPY, take the new UIDs and UIDVALIDITY from the
  server's COPYUID/APPENDUID response rather than a stored value.

### 5.4 Folders and labels

| Concept | Outlook | Gmail | IMAP |
|---|---|---|---|
| Folder (place to list from) | Mail folder | System label (INBOX, SENT, …) or user label | Mailbox |
| Label (tag; many per message) | Category | User label | Keyword (`Server`: needs custom keywords in PERMANENTFLAGS) |
| Move | Native | Add destination label, remove source label | MOVE, else COPY + EXPUNGE |
| Read / flagged | Native | Absence of `UNREAD` / `STARRED` | `\Seen` / `\Flagged` |

- A Gmail user label is both a folder and a label, which matches Gmail.
- "Answered" and "Forwarded" have no Gmail equivalent: `None`.
- `mail list --folder inbox` resolves to each provider's inbox (SPECIAL-USE or
  `INBOX` on IMAP).
- An IMAP account whose host is Gmail's is refused at login with a suggestion to use
  the Gmail provider, which avoids `[Gmail]/All Mail` duplicating every message.

### 5.5 Incremental sync

- Token: base64url of `{v, provider, account, state}`; opaque to callers.
- Result: changed or new messages, removed IDs, next token, or `fullResyncRequired`.
  Output keeps upstream's `MailDelta` shape plus `account`.
- **Gmail:** `historyId`; a per-folder scope filters by label ID. An expired ID
  (HTTP 404) → `fullResyncRequired`.
- **IMAP:** CONDSTORE (`CHANGEDSINCE`, `HIGHESTMODSEQ`) plus a UID-set comparison to
  find expunged messages. Without CONDSTORE: UIDs above the last seen one, plus a
  flag re-read over a recent window. A changed UIDVALIDITY → `fullResyncRequired`.
  QRESYNC can replace the UID comparison once go-imap supports it (PR #757).
- **Outlook:** upstream's delta, unchanged.
- Combined sync across accounts: one combined token holding a token per account.

### 5.6 Sending, sent copies and drafts

- `compose` builds every outgoing message once (text and HTML alternatives, inline
  `cid:` images, attachments, In-Reply-To and References). The bytes go to SMTP
  `DATA` or Gmail `messages.send` (`raw`, with `threadId` for replies). Outlook keeps
  Graph's own send calls via `graphprov`.
- **IMAP/SMTP send:** SMTP first, then APPEND the exact bytes with `\Seen` to the
  `\Sent` mailbox, unless the account's preset says the provider files its own copy.
  If the send succeeded and the copy failed, return success with
  `sentCopySaved: false` and the `sent-copy-not-saved` warning. Never retry the send.
- **IMAP drafts:** APPEND with `\Draft` to `\Drafts`. Update = append new copy, then
  remove the old one (`Server`: clean only with UIDPLUS). Send draft = send stored
  bytes, then file as above, then remove the draft.
- **Gmail drafts:** native drafts API.

## 6. Search and combined reads

### 6.1 Syntax (`query`)

| Kind | Operators |
|---|---|
| People and text | `from:`, `to:`, `cc:`, `subject:`, bare words, `"quoted phrase"` |
| State | `is:unread`, `is:read`, `is:flagged` (alias `is:starred`), `has:attachment` |
| Location | `in:<folder>`, `label:<label>` |
| Time | `after:YYYY-MM-DD`, `before:YYYY-MM-DD`, `newer_than:Nd`, `older_than:Nd` (dates read in the `--tz` zone; translated to exact times where the provider allows) |
| Logic | adjacency = AND, `OR`, `-` = NOT, parentheses |

- Structured flags (`--from`, `--to`, `--subject`, `--text`, `--since`, `--before`,
  `--unread`, `--flagged`, `--has-attachment`, `--in`, `--label`) build the same tree
  and are ANDed with any query.
- Each provider translates the tree using its capability table. An operator it
  cannot express is either filtered after fetching (`Partial`; the response notes
  that a page may hold fewer results) or refused with `search-unsupported`, naming
  the operator and provider. Operators are never silently dropped.
- Free-text matching differs slightly between Gmail, IMAP `TEXT` and KQL; the docs
  and capability notes say so.
- `--raw '<native query>'` passes a provider's own syntax unchanged, for one account.

### 6.2 Combined reads (`route`)

- **Commands:** `mail list` (including the combined inbox), `mail search`,
  `mail folders list` (grouped by account), `mail delta` and `changes`. `get` and all
  changing commands need exactly one account.
- **Selection:** CLI `--accounts a,b` or `--accounts all`; MCP `account: "all"` or an
  address from `--allow-account`.
- **Fan-out:** at most 4 accounts in parallel, each with its own deadline inside the
  overall `--timeout`.
- **Merge:** newest first by received time; ties by account, then ID. Each item
  carries `account` and `provider`.
- **Paging:** an opaque cursor holding, per account, the provider's page token and how
  many items of that page were already returned; the next call refetches that page
  and skips them. `--top` at most 100.
- **Partial failure:** results from healthy accounts plus
  `errors: [{account, code, message}]`; exit status non-zero using upstream's
  `reportedInOutputError` (JSON stays one document); `--allow-partial` exits 0. An
  account that cannot serve the query reports `search-unsupported` in `errors` while
  the others answer.

## 7. Safety

### 7.1 Guard layer (`guard`)

`guard.Wrap(p core.Provider, pol policy.Account, s *Session)` returns a provider
whose changing methods check, in order:

1. `--no-write` / `--no-send`;
2. the policy ceiling (`maxTier`) for the account;
3. the send lock (section 7.2);
4. recipient rules (section 7.4);
5. the daily send cap (section 7.4);
6. the per-call ID cap (100 under MCP, 1000 in the CLI).

The Gmail and IMAP clients also enforce `--no-write`/`--no-send` themselves; the SMTP
check runs before any connection is opened. For Outlook commands served by upstream's
`Run`, the router checks the policy ceiling first; upstream's own `graphapi` guards
cover `--no-write`/`--no-send`. All send-tier commands go through `guard` (section
4.3), including Outlook reply-all, whose recipients `graphprov` computes from the
original message before sending.

### 7.2 Send lock (MCP only)

- **Triggered by** any tool result containing untrusted text (subjects, sender names,
  bodies) from a message that is not trusted. *Trusted* = sender on the account's
  `trustedSenders` list **and** the topmost `Authentication-Results` header reports
  `dmarc=pass` for the From domain. Results that contain only IDs, dates and flags do
  not trigger it.
- **Covers** every account on the server until the server restarts. No tool can clear
  it.
- **When locked,** send-tier tools refuse with `send-locked`, saying when and by which
  account and message the lock was set, and suggesting a draft for a human to send
  (draft creation stays allowed) or a restart.
- **Opt-out:** `sendLock: "off"` in an account's policy; the server prints a warning
  at start.

### 7.3 Policy file (`policy`)

- Location: `<config dir>/policy.json`, where `<config dir>` is the namespace's config
  directory (`~/.config/mailmux/` on Linux, `~/Library/Application Support/mailmux/` on
  macOS). Per account:
  `maxTier` (`read` | `safe-write` | `send` | `destructive`), `allowedRecipients`
  (exact addresses or `@domain`), `trustedSenders`, `newRecipients`
  (`refuse` | `prompt` | `allow`), `dailySendCap`, `sendLock`.
- The stricter of the policy and the flags applies; flags cannot loosen the policy.
- A malformed policy stops `mailmux mcp` from starting and makes CLI commands fail.
- Written only by `mailmux policy init`; shown by `mailmux policy show`. Neither is an
  MCP tool. The docs state that an agent with shell access can edit this file; running
  MCP without shell access (or a compiled-in policy, a later release) closes that gap.

### 7.4 Recipients and send cap

- With `allowedRecipients` set, every To, Cc and Bcc must match (reply-all included),
  otherwise `recipient-not-allowed`.
- A *new* recipient is one neither on the allowlist nor in the account's send log.
  Default under MCP: `refuse`; in the CLI: `prompt` (refused under `--no-input`).
- Send log: `<config dir>/state/sends.jsonl`, one line per send with time, account,
  recipient addresses and a hash of the Message-ID; never content. Writes take a file
  lock.
- The daily cap is counted from the log, so restarts do not reset it; days follow
  `--tz`. Defaults: 20 per account per day under MCP; no cap in the CLI unless the
  policy sets one. Exceeding it returns `send-cap-reached`.

### 7.5 Gmail scopes per tier

| `--tier` at login | Scope requested |
|---|---|
| `read` | `gmail.readonly` |
| `write` (organise, drafts, send) | `gmail.modify` |
| `full` (permanent delete) | `https://mail.google.com/` |

- To verify while planning: whether `gmail.modify` includes sending. If it does,
  "organise but not send" is enforced by `guard`, not by the scope.
- The actually granted scopes (read from the token response) determine the recorded
  tier.
- `gmail.settings.*` is never requested in 1.0.
- IMAP app passwords grant full access; `guard` is the only control there.

### 7.6 Content cleaning

- `--sanitize-content=off|default|strict`. CLI default: `off`. Under MCP, `default` is
  forced on (the operator can raise it to `strict`, not lower it to `off`).
- `default`: HTML converted to text (`k3a/html2text`); hidden text removed
  (`display:none`, same-colour text); zero-width and bidi control characters removed.
  URLs kept as text.
- `strict`: also removes URLs.
- Runs before upstream's untrusted-content markers are applied; markers unchanged.

### 7.7 MCP changes

- `--allow-account a,b`: accounts the server may use (default: the default account).
- Every tool gets an `account` argument limited to that list; read tools that support
  combined reads also accept `"all"`.
- Tier flags (`--allow-write`, `--allow-send`, `--allow-destructive`) stay per tool;
  each account's policy narrows them further.
- A tool is listed if at least one allowed account's provider supports it; a call for
  an account whose provider does not returns `capability-missing`.
- Read tools get `readOnlyHint`; upstream already sets `destructiveHint` on send and
  destructive tools.
- New read tool `safety_status`: lock state and cause, sends remaining per account,
  effective tiers per account, policy file path.

## 8. Accounts and sign-in

### 8.1 Registry (`accounts`)

- Account key: provider + email. Users type the email; if it exists under two
  providers they write `gmail:me@example.com`.
- Outlook accounts stay in upstream's config and keychain entries and are signed in by
  upstream's code. Gmail and IMAP accounts live in `<config dir>/accounts.json`
  (mode `0600`): provider, email, display name, Google client name, sign-in method,
  IMAP/SMTP host, port and TLS mode, preset, whether the server saves its own sent
  copies, and capabilities seen at login (re-checked on connect).
- `mailmux accounts list|default|remove` covers both kinds. mailmux keeps its own
  default account in `accounts.json`.

### 8.2 Keychain

- Service: `mailmux` (from `Namespace`). Upstream's `olk:token:<email>` entries for
  Outlook live in it unchanged.
- mailmux entries:
  - `mux:oauth:<provider>:<client>:<email>`: refresh token only;
  - `mux:password:<provider>:<email>`: IMAP/SMTP password or app password;
  - `mux:client:google:<name>`: Google client secret.
- All well under Windows Credential Manager's 2,560-byte limit; a value over it fails
  with a clear error.
- Token refresh takes the same cross-process account lock as upstream, exposed by
  `internal/msauth/mux_lock.go`.

### 8.3 Google

- `mailmux setup google [--name N] [--gcloud-project ID]`: with a signed-in `gcloud`,
  creates or picks a project and enables the Gmail API; for steps `gcloud` cannot do,
  opens the Cloud Console pages with instructions (consent screen External and
  **In production**, to avoid the 7-day token expiry of Testing; Desktop OAuth
  client). `--client-json <file>` imports the downloaded client. Planning must confirm
  exactly what `gcloud` can automate.
- `mailmux auth login --provider gmail --client N --tier read|write|full [--manual]`:
  loopback redirect on `127.0.0.1` with PKCE (`golang.org/x/oauth2` helpers),
  `access_type=offline`, `prompt=consent`. `--manual`: open the URL elsewhere and
  paste back the redirect URL (SSH sessions). The device flow is not used (Google does
  not allow Gmail scopes in it).
- On `invalid_grant`, the error lists the likely causes: access revoked, password
  changed, client still in Testing (7-day expiry), or Google's limit of 100 refresh
  tokens per client.

### 8.4 IMAP/SMTP

- `mailmux auth login --provider imap --email me@example.com`: discovery (8.5) shows
  proposed settings for confirmation (never applied silently; `--imap-host`,
  `--smtp-host` etc. override). Password from the terminal or `--password-stdin`;
  never a command-line argument. The login tests IMAP (login, CAPABILITY, special-use
  LIST) and SMTP (EHLO, AUTH) without sending anything.
- TLS: implicit TLS (993, 465) preferred; STARTTLS (143, 587) only when advertised and
  the upgrade succeeds; credentials never over plaintext. `--tls-ca <file>` for
  self-signed servers; no option to skip verification.
- Presets: iCloud, Fastmail, Yahoo, AOL, Zoho, GMX, mailbox.org, Proton Bridge. Each
  records hosts, ports, whether the provider saves sent copies, and an app-password
  help link.

### 8.5 Discovery (`autoconfig`)

Order: built-in presets → `https://autoconfig.<domain>/mail/config-v1.1.xml` and
`https://<domain>/.well-known/autoconfig/mail/config-v1.1.xml` → Thunderbird ISPDB
(`https://autoconfig.thunderbird.net/v1.1/<domain>`, queried live, never vendored) →
the same for the MX base domain → SRV `_imaps._tcp`, `_submissions._tcp`,
`_submission._tcp` (accepted only when the target is inside the queried domain) →
guessed `imap.`/`smtp.`/`mail.` hosts → ask the user. `--no-remote-autoconfig` skips
the remote lookups (the ISPDB lookup sends the domain to Mozilla). tidemail's MIT
`internal/autodiscover` may be copied with its notice.

### 8.6 Microsoft

- Register a multi-tenant public-client Entra app "mailmux" with upstream's scopes,
  loopback and device-code redirects. Its ID is set at build time through
  `DefaultClientID` (section 4.1).
- Existing olk users sign in again; no import in 1.0.
- `docs/mailmux/enterprise-setup.md` adapts upstream's admin-consent guide.

## 9. Errors and retries

- Stable codes: `capability-missing`, `search-unsupported`, `stale-id`, `send-locked`,
  `policy-denied`, `recipient-not-allowed`, `send-cap-reached`, `reauth-required`,
  `provider-unavailable`, `partial-failure`; warning `sent-copy-not-saved`.
- `runSelected` prints mailmux errors in upstream's `{error:{code,status}}` JSON
  shape itself, then returns upstream's `reportedInOutputError` so nothing prints
  twice. MCP results carry `error[<code>]: <message>`.
- Text from servers passes through `outfmt.SanitizeMultiline` before display.
- Reads retry with backoff on 429, 5xx and dropped connections, honouring
  `Retry-After`; IMAP reconnects once.
- Sends are never retried once the request may have reached the server (SMTP `DATA`
  started, or the Graph/Gmail send request sent). The error states that delivery is
  unknown and suggests checking Sent.
- IMAP connections: per command in the CLI; under MCP at most one idle connection per
  account, closed after 2 minutes idle.

## 10. Testing

**Tier 1, hermetic (`go test -race`, CI):**

- Gmail: `httptest` server via `option.WithEndpoint` / `option.WithHTTPClient`;
  fixtures for expired `historyId` (404), paging, 429, label resolution, raw bodies.
- IMAP: go-imap `imapmemserver` on `localhost:0` with pre-created special-use
  folders; a scripted fake for CONDSTORE, UIDVALIDITY changes and malformed responses.
- SMTP: go-smtp server as a recording backend; `--no-send` opens no TCP connection.
- Query: golden table of every operator × provider, generated from the capability
  tables.
- Router: (command, accounts) → path table tests.
- Output parity: upstream `Run` vs mailmux handler on the same fake Graph data;
  identical JSON apart from added fields; equivalent table/TSV.
- Safety: lock from a listing; lock across accounts; cap survives restart; malformed
  policy refuses start; ID caps; recipient rules including reply-all.
- MCP: capability filtering, `account` enum, `"all"`, `safety_status`.
- The embedded-struct untrusted-wrapping check (section 4.4).
- Upstream's test suite passes unchanged.

**Tier 2, fuzzing:** MIME parsing (enmime's malformed corpus plus CRLF in encoded
words, path-traversal attachment names, bidi/zero-width subjects, conflicting
duplicate headers); IMAP response parsing at each connection state; the query parser.
Seeds run in every `go test`; a scheduled job runs `-fuzztime`.

**Tier 3, integration (`//go:build integration`):** `dovecot/dovecot:latest-dev`
and `mox localserve` started with `docker run` in a CI job; tests skip capabilities
the server does not advertise.

**Manual:** smoke checklist against throwaway Gmail, iCloud and Fastmail accounts;
each send step asks first.

**Licences:** `go-licenses` in CI; a `NOTICE` file crediting aerc, himalaya/io-email,
gogcli and tidemail where code or logic is reused.

## 11. Release

- `.goreleaser-mailmux.yaml` builds `cmd/mailmux` with `Namespace=mailmux` and the
  mailmux Microsoft client ID.
- Tags `mailmux/vX.Y.Z` trigger `release-mailmux.yml`. Upstream's `release.yml`
  (tags `v*`) is never triggered because those tags are not pushed.
- Homebrew cask `mailmux` in the owner's tap; npm `mailmux` plus per-platform
  packages with Trusted Publishing (OIDC) and provenance, modelled on
  `scripts/build-npm.mjs`; MCP registry name `io.github.yansigit/mailmux` from a
  mailmux `server.json`.
- Root `README.md` becomes mailmux's (occasional trivial merge conflicts accepted);
  mailmux docs under `docs/mailmux/`; upstream docs untouched.
- Before the first release: confirm the npm, Homebrew and GitHub names are still free,
  and run a trademark search.

## 12. Milestones

| # | Milestone | Done when |
|---|---|---|
| M0 | Foundations: hook edits, `cmd/mailmux`, namespace, env mapping, `DefaultClientID` var, embedded-struct check, release skeleton | `mailmux` behaves exactly like olk under a new name; upstream tests pass unchanged |
| M1 | `core`, `capability`, `query`, router, `graphprov`, combined reads | Two Outlook accounts searched together with mailmux syntax |
| M2 | `guard`, `policy`, send log and cap, recipient rules, send lock, content cleaning, `--allow-account`, `safety_status` | All safety tests pass on Outlook |
| M3 | `imapprov`, `compose`, `autoconfig`, presets, password sign-in | Full mail command set against Dovecot and one real iCloud or Fastmail account |
| M4 | `gmailprov`, `googleauth`, `setup google` | Full mail command set against a real Gmail account with a user-registered client |
| M5 | Docs, packaging, smoke checklist, mailmux Entra app | Tagged release installs from Homebrew and npm |

M4 needs the owner's Google Cloud project; M5 needs the owner's Microsoft account to
register the Entra app.

## 13. Open items to verify during planning

1. `outfmt.wrapUntrusted` and embedded structs (section 4.4).
2. Whether `gmail.modify` includes sending (section 7.5).
3. How much of Google client setup `gcloud` can automate (section 8.3).
4. Whether Graph returns `Authentication-Results` through `internetMessageHeaders`
   for the send-lock trust check, and at what cost per message.
5. Which KQL operators Graph `$search` supports for messages, to fill the Outlook
   capability table (for example whether `is:unread` needs filtering after fetching).
6. The exact binary-size cost of `google.golang.org/api/gmail/v1`; if unacceptable,
   replace it with a small hand-written REST client inside `gmailprov`.
7. go-imap v2 beta API changes between beta.8 and any later release before M3.
