# The GitHub App

A GitHub App is muster's GitHub identity when one is configured. It replaces
the per-user OAuth connection (`GITHUB_CLIENT_ID`, `MUSTER_GITHUB_TOKEN`), which
is **deprecated** and stays only as the fallback for a deployment without an
App.

With the App configured:

- **Agents get one-repository tokens.** The `github` connector is backed by
  the App. Each request an approved agent makes through muster's proxy
  (`/connect/github/…`, which is how `ccd` clones, fetches and pushes) carries
  an installation token that GitHub issues for **that agent's one repository**.
  The token has `contents: write` and `pull_requests: write`, plus the
  `metadata: read` that GitHub always adds. muster caches each token in memory
  until five minutes before it expires (GitHub issues them for one hour). It is
  never stored, logged or given to the agent. A request for any other
  repository is refused by muster before a token is minted, and GitHub would
  refuse the token there anyway.
- **You still approve each agent.** An agent's `github` request still waits
  for you under Connectors. The App changes where the credential comes from,
  not who decides. A request filed before the App was configured (when no
  `github` connector existed) shows **Approve** once the App is configured.
  Approve it and the agent's next clone retry (every 30 s) succeeds, with no
  new dispatch.
- **The Repos tab lists the installation's repositories.** So do the dispatch
  pickers and the "connected repository" check that every claude-code dispatch
  and `task_dispatch` applies. The OAuth connection is not consulted while the
  App is configured.
- The `github` connector card under Connectors says it is provided by the App.
  There is no Rotate or Delete. A stored `github` connector (a personal access
  token) is hidden while the App is configured, and comes back unchanged if you
  remove the App.

⚠ The App belongs to the deployment, not to one user. On a multi-user muster,
every user's dispatch check accepts the installation's repositories.

## Register the App

Sign in to GitHub as the account that will **own** the App (a personal
account is fine). Go to **Settings → Developer settings → GitHub Apps → New
GitHub App** (`https://github.com/settings/apps/new`) and fill in:

| field | value |
|---|---|
| GitHub App name | Any unique name, e.g. `muster-<yourname>` |
| Description | Optional |
| Homepage URL | Required by the form. Your muster's URL or the repository URL. |
| Callback URL | **Leave empty.** muster does not sign users in with the App. |
| Expire user authorization tokens | Leave as is (it does not apply). |
| Request user authorization (OAuth) during installation | **Unchecked** |
| Enable Device Flow | **Unchecked** |
| Setup URL | Leave empty |
| Webhook → Active | **Unchecked.** muster uses no webhooks; the Webhook URL is then not required. |
| Repository permissions → Contents | **Read and write** |
| Repository permissions → Pull requests | **Read and write** |
| Repository permissions → Metadata | **Read-only** (GitHub selects it, and it cannot be removed) |
| Every other repository permission | No access |
| Organization permissions | All: No access |
| Account permissions | All: No access |
| Subscribe to events | None |
| Where can this GitHub App be installed? | **Any account.** "Only on this account" cannot be installed on an organization that you, the owner, do not own. |

Click **Create GitHub App**. On the App's page:

1. Note the **App ID** (under "About"). It is `MUSTER_GITHUB_APP_ID`.
2. Under **Private keys**, click **Generate a private key**. GitHub downloads a
   `.pem` file. That file is `MUSTER_GITHUB_APP_PRIVATE_KEY`. Keep it only in
   the Secret below. If it leaks, generate a new one and delete the old one on
   this page.

muster only ever asks the App for those two repository permissions, whatever
the App was granted. But give the App no more than this table, so that a leaked
key cannot do more.

## Install it on the organization

On the App's page, click **Install App**, then **Install** next to the
organization (for example, the one whose repositories your agents work in).
Choose **Only select repositories** and pick the repositories agents may work
in. Click **Install**.

If you are not an owner of that organization, GitHub sends an installation
**request** instead. GitHub notifies the organization's owners. One of them
must approve it (the organization's settings, under **Third-party Access →
GitHub Apps**) before muster can mint any token.

To add or remove repositories later, go to the organization's **Settings →
Third-party Access → GitHub Apps → <your App> → Configure → Repository access**. muster's repository list
is cached for a minute.

The installation ID is the number at the end of that Configure page's URL
(`…/settings/installations/<id>`). You need it only if the App is installed on
several accounts and you want muster to use just one
(`MUSTER_GITHUB_APP_INSTALLATION_ID`). Otherwise muster picks the installation
whose account matches the repository's owner.

## Configure muster

| variable | value |
|---|---|
| `MUSTER_GITHUB_APP_ID` | The App ID |
| `MUSTER_GITHUB_APP_PRIVATE_KEY` | The whole `.pem` file's contents. A value with its line breaks written as `\n` is accepted too. |
| `MUSTER_GITHUB_APP_INSTALLATION_ID` | Optional; see above |

The App also needs the connector store (`MUSTER_CONNECTOR_ENCRYPTION_KEY`,
[connectors.md](connectors.md)), because agents reach GitHub only through the
proxy, and their grants live in that store. muster refuses to boot if the App ID
is set without the key (or the other way round), if the key does not parse, or
if there is no connector store.

On Kubernetes, keep all three in one Secret and load it with `envFrom`:

```sh
kubectl -n <muster-namespace> create secret generic muster-github-app \
  --from-literal=MUSTER_GITHUB_APP_ID=<app id> \
  --from-file=MUSTER_GITHUB_APP_PRIVATE_KEY=./<app-name>.<date>.private-key.pem
```

```yaml
envFrom:
  - secretRef:
      name: muster-github-app
```

The Secret's keys are `MUSTER_GITHUB_APP_ID`, `MUSTER_GITHUB_APP_PRIVATE_KEY`
and, if you pin one, `MUSTER_GITHUB_APP_INSTALLATION_ID`.

## Check it

At boot muster prints a `github app: ON — …` line, then lists the App's
installations in the background:

- `github app: App <id> authenticates and is installed on <accounts>`: it works.
- `github app: the boot check FAILED: GitHub refused the GitHub App's credentials (401) …`:
  the ID and key do not belong to the same App, the key was deleted, or the
  server's clock is more than a minute off. The App's JWT is valid from 60 s
  before to 9 minutes after muster signs it.
- `… is installed on NO account`: install it (above).

A failure that blocks agents is also shown on the connector request card and on
each affected agent's card (`GitHub: …`). Examples are the credentials refused,
a rate limit, or a repository that is not in the installation (GitHub's 422).
The Repos tab shows it above the list.

## The kickoff wait

A claude-code agent whose repository cannot be cloned yet holds its first turn.
It waits up to `MUSTER_CHECKOUT_WAIT_TIMEOUT` (default `24h`) for your approval,
then goes to `error`. The wait is measured from the agent's last status change
(its `updated_at`). Recording the wait does not move that time. So raising the
variable and restarting muster also extends an agent that is already waiting:
its deadline becomes `updated_at` plus the new value.

## Removing the OAuth connection

The OAuth code (`/github/connect`, the per-user token store,
`MUSTER_GITHUB_TOKEN`) is still in this release as the fallback. It will be
removed once the App has been verified on a live deployment.
