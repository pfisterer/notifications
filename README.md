# notifications

The notification service of the DHBW Cloud self-service platform. Services report events (a request waits, an allocation expired, a project ends); this service decides whether, when and how they reach people. E-mail is the first channel; further ones (Matrix, Telegram) plug in behind the same `Deliverer` interface (`internal/mail`).

## Why one service

Every service could send its own mails, and each would decide on its own when somebody has had enough. Then a person who manages three budgets and a project gets mail from three senders, each within its own limit, and none of them knows about the others. One service that receives every event can bundle, count and offer one settings page for all of it.

## The contract: `POST /v1/events`

Authenticated with a service token: `Authorization: Bearer <token>`. Each sending service has one, configured as `NOTIFY_SERVICE_TOKENS=name=token,name=token` (from a Secret, key `service-tokens`); the name is how the service is recognised, and event keys are scoped to it, so one service can neither collide with nor resolve another's events.

```json
{"source":"openstack-management-api","type":"allocation.expired","key":"allocation-expired:<id>","class":"action","resolve":false,
 "recipients":[{"user":"someone@example.edu"},{"group":"group:dualis-ma-wdski25a#studierende"}],
 "actor":"someone-else@example.edu","due":"2026-12-09T00:00:00Z",
 "object":{"kind":"project","id":"p_...","name":"...","url":"https://self-service.staging.dhbw.cloud/..."},
 "data":{"...":"free-form, for the texts"}}
```

| Field | |
| --- | --- |
| `source` | The sending service. Stored as given; defaults to the token's name. |
| `type` | Kind of event, lowercase (`[a-z0-9][a-z0-9_.-]*`). Selects the text. An unknown type is delivered with a generic text: `data.title` and `data.text` when given (a string, or `{"de": …, "en": …}`), else the type and the object's name. |
| `key` | Identifies the event against duplicates: while an event with this key (from the same token) is open, another one with the same key is a duplicate and delivers nothing. Choose it so that a new occurrence gets a new key (`allocation-expired:<id>`, `project-ending:<id>:<end date>`). |
| `class` | `action` (somebody has to act), `disruption` (something that affects the recipient went wrong) or `info` (good to know). Required unless `resolve`. |
| `resolve` | `true` closes the open event with this key: what of it has not gone out yet is dropped, no reminder follows, and the key may be used again. Needs only `type` and `key`. |
| `recipients` | `user` = a mail address; `group` = a role-provider userset (`group:<id>` or `group:<id>#<relation>`), resolved to people through the role provider. Roles at objects (owners of a project, managers of a budget) the sender resolves itself into `user`s. Required unless `resolve`; at most 1000 entries. |
| `actor` | The person whose action caused the event. Never notified about it. |
| `due` | A deadline (RFC 3339). Shown in the texts where a type has no date of its own in `data`. |
| `object` | What the event is about; `url` (http or https only) is the button in the mail. Without a `url` the service links the portal itself (`PORTAL_URL`): `/projects?project=<id>` for `kind: project`, `/projects?budget=<id>` for `kind: budget`, `/projects?node=<id>` for anything else of the budget tree. The portal opens these where the viewer sees the object (a project of their own in its area's list, anything else under Budgets), so senders can use the same form or leave `url` out. |
| `data` | Free-form input for the texts (see below). |

Answers: **202** for every valid event, with `{"status": "accepted" | "duplicate" | "resolved" | "nothing_to_resolve"}`; **400** for an invalid body (`{"error": "…"}`); **401** without a valid service token. Delivery happens asynchronously: a 202 means stored, not sent.

### Texts

Types with their own text (German and English, HTML and plain text):

| Type | Typical class | `data` fields read |
| --- | --- | --- |
| `approval.waiting` | action | `requester`, `summary` (what is requested), `budget`, `reason` |
| `approval.decided` | info | `decision` (`approved`, `rejected`, `partially_approved`), `decided_by`, `summary`, `comment` |
| `project.ending` | action | `end` (date; else `due`) |
| `allocation.expired` | action | `budget`, `summary`, `shrink_at` (date the project is reduced; else `due`) |
| `allocation.shortened` | disruption | `budget`, `changed_by`, `summary`, `reason` |

Every type also takes `data.text` as an extra paragraph. Text fields may be a string or `{"de": …, "en": …}`; dates are `YYYY-MM-DD` or RFC 3339. The class comes from the sender, not from the type.

## Rules

- **Classes.** `action` and `disruption` go out at once, `info` only in the summary (weekly by default, Mondays at 8; daily if the person chooses). A summary that holds an action or disruption (see the daily limit) goes out at the next daily slot even for weekly readers.
- **Bundling.** An immediate notification waits `BUNDLE_COLLECT_SECONDS` (120) for more of its type for the same person; after a mail of a type, further ones of that type wait until `BUNDLE_WINDOW_MINUTES` (15) have passed and go out together ("3 requests await a decision").
- **Duplicates.** Same token and key while the event is open: nothing new.
- **Reminders.** An `action` that has been delivered and is still open is reminded of after 2 and 7 days (`REMINDER_DAYS`), never more than twice, until an event with `resolve: true` and the same key arrives.
- **Daily limit.** At most `DAILY_LIMIT` (5) immediate mails per person and calendar day; the rest goes into the summary.
- **Own actions.** The `actor` never gets a notification about the event.
- **Settings per person**: language, mode per class (action: immediate or summary; disruption: immediate, summary or off; info: summary or off), summary daily or weekly, and types unsubscribed from. Without settings: action and disruption at once, info weekly.

Every mail links to the settings page (`SETTINGS_URL`) and carries `List-Unsubscribe` with `List-Unsubscribe-Post: List-Unsubscribe=One-Click` (RFC 8058) for its type; a summary's link switches the info class off. The link is a signed token (`UNSUBSCRIBE_SECRET`), works without signing in, and shows a confirmation page on GET; only a POST changes anything, so mail scanners following links unsubscribe nobody.

## API for the portal

The person routes take the portal's ID token (forwarded by its proxy, checked with golib's `oidcauth` against `OIDC_ISSUER_URL`/`OIDC_CLIENT_ID`). Settings are personal: a request with `X-Act-As` is refused (403).

| Route | |
| --- | --- |
| `GET /v1/preferences` | Own settings, or the defaults (`is_default: true`) |
| `PUT /v1/preferences` | Save own settings; missing fields take the defaults |
| `GET /v1/types` | Types with their own text (names in the language of `Accept-Language`) and the modes each class may take |
| `GET /v1/notifications?limit=50` | Own recent notifications, newest first, rendered in the request's language (for a bell later) |
| `GET/POST /unsubscribe?token=…` | Unsubscribe link, no sign-in |
| `POST /v1/admin/pass` | Admin token (`NOTIFY_ADMIN_TOKENS`): run a delivery pass now |
| `POST /v1/admin/digests` | Admin token: send the summaries now, of `{"recipient": "…"}` or of everybody |

The OpenAPI description is served at `/swagger.json` (generated with swag, `make generate-docs`). `/healthz` and `/readyz` are the probes; `/metrics` is on its own port (`METRICS_BIND`, default `:8088`) that no ingress exposes.

## Metrics

| Series | |
| --- | --- |
| `notify_events_received_total{sender,class}` | events accepted |
| `notify_delivered_total{channel,kind}` | notifications delivered, `kind` = immediate or digest |
| `notify_messages_total{channel,kind}` | mails sent |
| `notify_bundled_total{channel}` | notifications that rode in a mail with others of their type |
| `notify_suppressed_total{reason}` | not delivered on purpose: duplicate, own_action, resolved, unsubscribed, off, no_recipient |
| `notify_deferred_total{reason}` | moved to the summary (daily_limit) |
| `notify_failed_total{stage}` | send (one failed attempt), gave_up (after `MaxAttempts`), resolve (a group could not be resolved; the event waits) |
| `notify_waiting{status}` | pending and digest notifications in the database |
| `notify_last_pass_timestamp_seconds` | the delivery loop's last pass |

## Configuration

| Variable | Default | |
| --- | --- | --- |
| `DB_CONNECTION_STRING` | – | PostgreSQL DSN (required); the schema is migrated at start |
| `NOTIFY_SERVICE_TOKENS` | – | `name=token,…`, each token ≥ 32 characters (required) |
| `NOTIFY_ADMIN_TOKENS` | – | comma-separated, for `/v1/admin` |
| `UNSUBSCRIBE_SECRET` | – | ≥ 32 characters (required); changing it invalidates every link sent so far |
| `PUBLIC_URL` | – | where the unsubscribe links point (required) |
| `SETTINGS_URL` | – | the portal's settings page |
| `PORTAL_URL` | – | the portal, for links to objects sent without a `url` |
| `MAIL_MODE` | `smtp` | `smtp`, or `log` (only logs what would be sent) |
| `SMTP_HOST`, `SMTP_PORT`, `SMTP_TLS` | –, `25`, `starttls` | `starttls` uses TLS when offered (and requires it with a login), `tls` is implicit TLS, `none` plain |
| `SMTP_USERNAME`, `SMTP_PASSWORD` | – | optional login (PLAIN, only over TLS) |
| `SMTP_FROM`, `SMTP_FROM_NAME`, `SMTP_REPLY_TO`, `SMTP_HELO_NAME` | –, `DHBW Cloud`, –, `localhost` | |
| `ROLE_PROVIDER_URL`, `ROLE_PROVIDER_TOKEN` | – | group resolution (a resolve or read token there); without a URL, events naming a group wait |
| `OIDC_ISSUER_URL`, `OIDC_CLIENT_ID`, `OIDC_JWKS_URL` | – | the portal's sign-in; empty issuer leaves the person routes out |
| `DEV_AUTH_USER` | – | development only (`API_MODE=development`, no OIDC): person requests without Authorization are this person |
| `TIMEZONE`, `BUNDLE_COLLECT_SECONDS`, `BUNDLE_WINDOW_MINUTES`, `DAILY_LIMIT`, `REMINDER_DAYS`, `DIGEST_HOUR`, `RETENTION_DAYS`, `PASS_INTERVAL_SECONDS` | `Europe/Berlin`, 120, 15, 5, `2,7`, 8, 90, 30 | the rules |
| `API_BIND`, `METRICS_BIND`, `API_MODE`, `TRUSTED_PROXIES` | `:8087`, `:8088`, `production`, – | |

Run one replica only: the delivery loop is not coordinated between pods (the chart's schema allows at most one, and the Deployment uses `Recreate`).

## Development

```sh
make test        # generates the swagger docs, then go test ./... (SQLite in memory, no Docker)
make vet
make run         # needs a PostgreSQL in DB_CONNECTION_STRING; a .env file is read
```

The Helm chart is in `helm-chart/` with a `values.schema.json`; the umbrella chart [cloud-self-service](https://github.com/pfisterer/cloud-self-service) carries it as the subchart `notifications`, switched off by default.

## Releases

Same flow as the other services: `VERSION` is the source of truth, `make bump V=x.y.z` sets it and the chart version, `-test.N` versions are the staging channel. CI (`.github/workflows/docker-image.yml`, workflow "CI") runs the checks and builds the image in parallel and then publishes: every green commit on `main`, under `VERSION` if that is not published yet, otherwise under an automatic prerelease that sorts right after it (`X.Y.Z-test.N.<run>`, or `X.Y.(Z+1)-0.ci.<run>` after a stable version), by retagging the image built once; the umbrella and with it staging follow within about ten minutes, production pins exact versions. Stable versions get a tag and a GitHub release with `swagger.json`. `make publish` does the same from a machine.
