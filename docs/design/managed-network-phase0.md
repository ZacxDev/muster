# Managed network: phase-0 documentation spikes, results

Status: **spike results**, for
[managed-network.md](managed-network.md) ("[Phase 0: spikes](managed-network.md#phase-0-spikes-no-product-change)",
design at `af368efa`). This page covers the three phase-0 items that can be
answered from vendor documentation alone: the DNS provider's limits, the CT
aggregator's terms and limits, and the GeoIP database's licence. The items that
need a domain, a phone, node access or an outward filing are listed at the end
([spikes not run here](#spikes-not-run-here)).

**Scope.** Every number below was **read** from the vendor's own documentation
on **2026-11-16**; §1's UNVERIFIED cells were re-checked on **2026-11-17**,
against the docs and, for deSEC, its source. Nothing was measured, no account was created, nothing was
filed. Vendors change limits without notice; re-read a row before acting on it.
Anything not confirmed from a primary source is marked **UNVERIFIED**.
`example-muster.net` is the design's placeholder zone.

## Verdict

| spike | answer | changes in the design |
|---|---|---|
| **1. DNS provider** | **Route 53** fits best: 10,000 records per zone (raisable), and an IAM policy can limit a credential to `TXT` at `_acme-challenge.*` names. deSEC and Google Cloud DNS can also scope a credential to one name and type. Cloudflare and Hetzner cannot. | 🔴 **Refutes** "no DNS provider scopes a credential to one TXT name" ([DNS-01](managed-network.md#acme-dns-01-the-servers-client-the-services-txt)). The challenge endpoint's credential can be TXT-only, so it cannot rewrite CAA. 🔴 **On Cloudflare, Universal SSL silently defeats the CAA `accounturi` pin** (CVE-2026-14440). |
| **2. CT aggregator** | Cert Spotter's API wants an **account and API key "for production"**; unauthenticated use is "for personal or evaluation purposes". Free: 100 single-hostname queries/hour (whether that is per account or per key is not stated: **UNVERIFIED**). Paid: $50, $500 and up per month. Watching with webhooks is a separate paid product (from $15/month, 20 endpoints, hourly). crt.sh: no published policy; 60 requests/min per IP (2020 post); no SLA. | The design does not say **whose key** each server polls with. "Up to 24 h" is right for the slowest logs Chrome accepts today (17 usable logs with a 24 h MMD), but it leaves out the aggregator's own lag and the hourly poll. |
| **3. GeoIP** | **DB-IP City Lite**: CC BY 4.0, no account, monthly, country + region + city, MMDB. GeoLite2 City is also usable: free, attribution, an account and licence key, updated Tuesday and Friday, old copies destroyed within 30 days. IPinfo Lite is **country only**. IP2Location LITE **forbids redistribution**. | The **relay** does the lookup ([the relay](managed-network.md#the-relay)), so the public muster-server never ships or downloads a database. The confirm screen needs an **attribution line**. |

## 1. DNS provider limits

**Question.** For each provider: the per-zone record cap, the API write limits,
whether a token can be limited to one zone, one record name or one record type,
and CAA support with `accounturi` / `validationmethods`. Then: how many servers
fit in one zone, at 4 records per server and at 8 with Pages.

### Answer

| | Cloudflare | AWS Route 53 | Hetzner DNS (Console / Cloud API) | deSEC | Google Cloud DNS |
|---|---|---|---|---|---|
| **records per zone** | Free: **200** (zones created on or after 2024-09-01; older Free zones 1,000). Pro and Business: **3,500**. Enterprise: one quota per account, default **1,000,000** (from 2026-06-10) | **10,000** per hosted zone. Can be raised, at extra charge | **500**. Can be raised "if there is a justified demand". 50 per RRset | No per-domain cap in the docs or in the API source (**UNVERIFIED**: an absence, not a statement). 4,091 records per RRset. **Domains per account are computed**, not fixed: `limit_domains` "grows with the domains you have delegated to us securely (with DNSSEC enabled)"; the source's floor for an account with none secured is **1** (deployed value not public). One is enough for this product | A quota on **RRsets**, "Resource record sets per managed zone", adjustable. **The default number is not printed in the docs** (**UNVERIFIED**: the page's sample `gcloud dns project-info describe` output shows `rrsetsPerManagedZone: 10000`, but it is an example, and its `rrsetAdditionsPerChange: 3000` disagrees with the page's own 1,000 limit) |
| **API write limits** | **1,200 requests / 5 min per user**, all endpoints. Going over blocks every call for 5 min (429) | Account bucket: **10 req/s sustained, burst 50**. Change bucket: **100 changes/s, burst 1,500** (UPSERT counts 2). One in-flight change per zone (`PriorRequestNotComplete`) | **3,600 requests/hour per project**, refilled 1/s | RRset write **requests** per domain: **2/s, 15/min, 100/h, 300/day**. A bulk request is **one** request ("If you require more requests, consider using bulk requests") | "DNS writes per region per minute" quota, adjustable. **The default number is not printed** (**UNVERIFIED**: no figure anywhere on the quotas page; `gcloud dns project-info describe` on a real project would show it) |
| **token scoped to one zone** | **yes** (zone resources) | **yes** (IAM `Resource` = the hosted zone) | **no**: a token belongs to a **project** and reaches everything in it, servers included. A project holding only the zone gets the same effect | **yes** (token policy `domain`) | **yes** (IAM on the managed zone) |
| **to one record name / type** | **no**: "DNS Write" covers the whole zone | **yes**: `route53:ChangeResourceRecordSetsNormalizedRecordNames`, `…RecordTypes`, `…Actions` (wildcards with `StringLike`) | **no**: Read or Read & Write only | **yes**: policy on (`domain`, `subname`, `type`) with `perm_write`, longest-prefix match | **yes**: IAM Conditions on `resource.name` ending `/rrsets/<name>./<TYPE>` |
| **CAA** | supported (tag + value). 🔴 **Universal SSL serves its own permissive CAA RRset in place of yours**, so `accounturi` is not enforced unless Universal SSL is off | supported. Parameters go in the value, separated by `;` (the docs' example: `0 issue "ca.example.net; account=123456"`) | listed as supported. Whether the value can carry parameters is not stated (**UNVERIFIED**: the record-types page shows only `letsencrypt.org`; the API spec only lists the type) | listed as supported. Parameters not stated (**UNVERIFIED**: no parameterised value in the docs or the API's test fixtures) | supported. Parameters not stated (**UNVERIFIED**: the record-types page's only example is `ca.example.net`) |
| **price** | Free / paid plans | per zone + per query | free ("we do not charge for our DNS zones") | free (non-profit) | per zone + per query |

`accounturi` and `validationmethods` are text inside the CAA value (RFC 8657),
so any provider that stores the value verbatim can carry them. The CA is what
honours them: Let's Encrypt documents both, and its account URI looks like
`https://acme-v02.api.letsencrypt.org/acme/acct/<number>`. Whether each
provider's validator **accepts** a `;`-parameterised value is shown only by
Route 53's docs. Phase 0's DNS-01 spike, with its negative control, is the
test for the chosen provider ([below](#spikes-not-run-here)).

#### Servers per zone

The design counts **4 records per server** (`A`, `AAAA`, two `CAA` values) and
**8 with Pages**. On top of that the zone has about **7 fixed records**
(apex `A`/`AAAA`/`CAA`, `api` and `tunnel` `A`/`AAAA`, before the provider's
own NS/SOA) and **2 transient TXT** per in-flight order. Floor of
(cap − 7) / 4 and (cap − 7) / 8:

| cap | 4 per server | 8 per server (Pages) |
|---|---|---|
| Hetzner, 500 | **123** | **61** |
| Cloudflare Free (new zone), 200 | **48** | **24** |
| Cloudflare Pro / Business, 3,500 | **873** | **436** |
| Route 53 default, 10,000 | **2,498** | **1,249** |
| Cloudflare Enterprise, 1,000,000 per account | **249,998** | **124,999** |

**Records or RRsets?** Hetzner and Cloudflare count individual records.
Google Cloud DNS counts **RRsets** (its quota is "Resource record sets per
managed zone", with a separate per-RRset records quota). Route 53's quota
says "Records", and whether it counts record sets or individual values is
**UNVERIFIED**: the API's limit type is named `MAX_RRSETS_BY_ZONE` but
described as "the maximum number of records"; `GetHostedZoneLimit`'s `Count`
on a zone holding one two-value RRset would answer it. Counted per RRset, a
server is 3 (`A`, `AAAA`, one two-value `CAA` RRset) and 6 with Pages, and
the zone holds about a third more.

#### Write rates per server

With Let's Encrypt's announced schedule, a renewal needs a fresh DNS-01
authorisation almost every time. The default profile drops to 64-day
certificates with a **10-day** authorisation reuse on **2027-02-10**, then to
45-day certificates with **7-hour** reuse on **2028-02-16**. That resolves the
schedule **UNVERIFIED** in [certificate renewal](managed-network.md#certificate-renewal).
So each renewal costs 2 TXT writes (set, then delete), per name.
Renewing at a third of 45 days left means every 30 days. A server with Pages
is then **about 4 writes a month**, before retries and claims.

- **deSEC's per-domain limit is the binding one, and it counts requests, not
  RRsets.** A bulk request is **one** write: the throttle records one hit per
  HTTP request, and the bulk endpoint (`rrsets/`) shares the per-domain
  scope (source, `desecapi/throttling.py`, `views/records.py`); the docs say
  "If you require more requests, consider using bulk requests". This
  corrects the 2026-11-16 reading, which took one write per RRset.
  - **One request per TXT write**: the whole product is one domain, so
    **300 a day** covers about 150 orders a day, about 4,500 names on a
    30-day cycle, ~2,250 servers with Pages, before retries, claims and
    tombstones. A mass re-issue after a CA incident runs at 50 orders an hour
    (100/h) for 3 hours, then 300/day binds: all ~4,500 names take about
    **30 days**, and a CA's revocation deadline is measured in **days**.
  - **Batched**: 300/day is one flush about every **5 minutes**, each
    carrying every pending set and delete. The rate then no longer caps
    servers or a mass re-issue; request size does (an RRset at most 64,000
    bytes of JSON; the API forwards at most 32 MiB to its backend), and that is
    not quantified here. The cost: the challenge endpoint must queue its
    writes and flush them, and an order waits up to one flush for its TXT
    and one for its removal, so the DNS table's "≤ 10 min per order" holds
    only at about 5-minute flushes.
- **Hetzner's 3,600/h is per project**, shared with anything else in that
  project.
- **Cloudflare's 1,200 / 5 min** and **Route 53's 10 req/s** are far above
  this load. On Route 53, one in-flight change per zone means writes are
  serialised: send them as batches, not in parallel.

### What it changes

- 🔴 **"No DNS provider scopes a credential to one TXT name" is false**
  ([DNS-01](managed-network.md#acme-dns-01-the-servers-client-the-services-txt)).
  It is true for Cloudflare and Hetzner. It is false for Route 53, deSEC and
  Google Cloud DNS. The design does not reopen *DNS-01 delegation to the
  server*: the shared zone and the record writes still belong on the service.
  But "**The same token writes TXT and CAA**" ([DNS](managed-network.md#dns)) is
  a provider choice, not a law. On Route 53 the service can hold two
  credentials:
  - a **TXT credential** for the challenge endpoint: names
    `ForAllValues:StringLike` `_acme-challenge.*.example-muster.net`, types
    `TXT`, actions `UPSERT`/`DELETE`, each key also `Null` = `false`;
  - a **records credential** for claim and tombstone (`A`, `AAAA`, `CAA` at
    `<id>`, `<pid>`).

  The [threat-model](managed-network.md#8-threat-model) row "a bug in the
  challenge endpoint" then has a second, structural guard: that endpoint's
  credential **cannot** rewrite CAA. The row "the operator, or a compromise of
  the node" stands as written while **both** credentials sit on the node: the
  operator holds both, and CT is still the only protection against them. It
  narrows only if the records credential is kept off the node (used by a
  separate claim worker). That is a design choice for the operator, not
  something this spike decides.
  - 🔴 **Confirmed (IAM docs):** `ForAllValues` "also returns `true` if
    there are no context keys in the request", and IAM says to "always
    include the `Null` condition operator in your policy with a `false`
    value". So the TXT credential needs `"Null": {key: "false"}` for **each**
    of the three keys; Route 53's own example policies leave it out. A
    phase-1 test should still run the policy simulator against a CAA
    change, an `A` change and an empty change. Also confirmed: `StringLike`'s
    `*` is a "multi-character match wildcard" in "patternless matching", so
    it matches across dots and `_acme-challenge.a.b.example-muster.net`
    would be writable. That name authorises nothing the design uses, but a
    test should pin it.
- 🔴 **If Cloudflare is chosen, Universal SSL must be off on the zone**, or the
  `accounturi` pin is silently not enforced. CVE-2026-14440 (CNA: Cloudflare,
  2026-07-01): Cloudflare's authoritative DNS "serves this auto-managed RRset
  at query time, superseding any customer-configured CAA records", and "there
  is no in-product workaround that preserves both". The design never proxies
  these names, so turning Universal SSL off costs nothing. But **the
  dashboard does not show the auto-added records**. Only a `dig` of the CAA
  RRset tells, so phase 1's two-resolver DNS test must assert the served CAA
  RRset **exactly**, not just that our record is present.
- **The record cap resolves the [DNS](managed-network.md#dns) UNVERIFIED** for
  each provider (table above). **Hetzner (61–123 servers) and Cloudflare Free
  (24–48) are too small** to launch on. "Run our own authoritative DNS" moves
  out to about 1,250–2,500 servers on Route 53's default cap (more if raised),
  or 436–873 on Cloudflare Pro.
- **Recommendation: Route 53.** It is the only one of the five with all of:
  a published per-zone cap in the thousands, published write limits far above
  the load, per-name and per-type credentials, and a documented
  parameterised-CAA example. deSEC is the free alternative with the same
  credential model, but its 300 write requests/day per domain means the
  challenge endpoint must batch its writes (a flush about every 5 minutes);
  unbatched, it caps the product at about 2,250 servers with Pages, before
  retries and claims. Google Cloud DNS has the credential model
  but does not print its defaults. Phase 0's DNS-01 + CAA negative-control
  spike should run on the provider actually chosen.

### Sources (read 2026-11-16)

- Cloudflare DNS record quotas: <https://developers.cloudflare.com/dns/manage-dns-records/>; Enterprise account quota: <https://developers.cloudflare.com/changelog/post/2026-06-10-account-level-record-quota/>
- Cloudflare API rate limits: <https://developers.cloudflare.com/fundamentals/api/reference/limits/>
- Cloudflare token permissions (DNS Read/Write are zone-scoped, nothing finer): <https://developers.cloudflare.com/fundamentals/api/reference/permissions/>
- Cloudflare automatic CAA records: <https://developers.cloudflare.com/ssl/edge-certificates/caa-records/>
- CVE-2026-14440, NVD API record (CNA Cloudflare, published 2026-07-01): <https://services.nvd.nist.gov/rest/json/cves/2.0?cveId=CVE-2026-14440>
- Route 53 quotas: <https://docs.aws.amazon.com/Route53/latest/DeveloperGuide/DNSLimitations.html>; throttling: <https://docs.aws.amazon.com/Route53/latest/DeveloperGuide/throttling-api-requests.html>
- Route 53 IAM conditions for record names, types and actions: <https://docs.aws.amazon.com/Route53/latest/DeveloperGuide/specifying-rrset-conditions.html>
- Route 53 CAA format: <https://docs.aws.amazon.com/Route53/latest/DeveloperGuide/ResourceRecordTypes.html>
- Hetzner DNS limits and pricing: <https://docs.hetzner.com/networking/dns/overview/>; record types: <https://docs.hetzner.com/networking/dns/migration-to-hetzner-console/features-and-differences/>
- Hetzner Cloud API rate limit (3,600/h per project) and project-bound tokens: OpenAPI spec <https://docs.hetzner.cloud/cloud.spec.json> (rendered at <https://docs.hetzner.cloud/reference/cloud>); token permissions: <https://docs.hetzner.com/cloud/api/getting-started/generating-api-token/>
- deSEC token policies: <https://desec.readthedocs.io/en/latest/auth/tokens.html>; rate limits: <https://desec.readthedocs.io/en/latest/rate-limits.html>; RRsets and CAA: <https://desec.readthedocs.io/en/latest/dns/rrsets.html>
- Google Cloud DNS quotas (page last updated 2026-11-10): <https://docs.cloud.google.com/dns/quotas>; per-RRset IAM conditions: <https://docs.cloud.google.com/dns/docs/zones/iam-per-resource-zones>
- Let's Encrypt CAA parameters: <https://letsencrypt.org/docs/caa/>; lifetime schedule: <https://letsencrypt.org/2025/12/02/from-90-to-45>

Added for the UNVERIFIED cells (read 2026-11-17):

- deSEC source, `desec-stack` at `991b91ab` (2026-11-04): throttle rates `api/api/settings.py`, one hit per request `api/desecapi/throttling.py`, bulk endpoint's scope `api/desecapi/views/records.py`, domain limit `api/desecapi/models/users.py` (`effective_limit_domains`); <https://github.com/desec-io/desec-stack>
- deSEC `limit_domains`: <https://desec.readthedocs.io/en/latest/auth/account.html>; domain limit 403: <https://desec.readthedocs.io/en/latest/dns/domains.html>
- Google Cloud DNS record types (CAA example): <https://docs.cloud.google.com/dns/docs/records-overview>; quotas re-read (page last updated 2026-11-10): <https://docs.cloud.google.com/dns/quotas>
- Route 53 `GetHostedZoneLimit` (`MAX_RRSETS_BY_ZONE`): <https://docs.aws.amazon.com/Route53/latest/APIReference/API_GetHostedZoneLimit.html>
- IAM `ForAllValues` and `Null`: <https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_policies_condition-single-vs-multi-valued-context-keys.html>; `StringLike` wildcards: <https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_policies_elements_condition_operators.html>
- Hetzner record types (CAA): <https://docs.hetzner.com/networking/dns/record-types/overview/>

## 2. CT aggregator

**Question.** Can a product have every server poll SSLMate's Cert Spotter
issuance API hourly for its own names? Under what terms, at what rate limits,
with what auth? Does a per-domain watch or webhook avoid polling? Is crt.sh
fine as the fallback? And what does "up to 24 h detection" assume?

### Answer

**SSLMate Certificate Search API** (`GET /v1/issuances`):

- **Auth and terms.** "You can make a limited number of unauthenticated 'List
  Issuances for a Domain' queries per hour, **for personal or evaluation
  purposes**. When you're ready to launch in production, we ask that you sign
  up for an account and authenticate." The unauthenticated number is **not
  published** (**UNVERIFIED**). Auth is an API key, as a Bearer token or as
  the HTTP Basic username.
- **Rate limits per plan:**

  | plan | price | single-hostname queries/h | full-domain queries/h |
  |---|---|---|---|
  | Small (free, with an account) | $0 | 100 | 10 |
  | Medium | $50/month | 1,000 | 100 |
  | Large | $500/month | 10,000 | 1,000 |
  | 2x / 3x Large | $1,000 / $1,500/month | — | 2,000 / 3,000 |

  Over the limit is the error `rate_limited`. Provisioned indexes for large
  domains cost from $100/month per domain. The Firehose costs $1,000/month.
- **Incremental polling is built in.** Pass the last issuance's `id` as
  `after`. An empty answer carries `Retry-After`, and the next poll returns
  only new issuances. `pubkey_sha256` is in every issuance by default, which
  is exactly what the design's "every key it has ever used" check compares
  ([certificate renewal](managed-network.md#certificate-renewal)).
- **A watch instead of a poll** is a separate, paid product, **Cert Spotter
  monitoring**. There is no free tier. Hobbyist is $15/month for 20 endpoints,
  checked hourly. Startup is $100/month for 150 endpoints every 15 min.
  Business is $500/month for 1,000 endpoints every 5 min. It alerts on
  unknown certificates, and can call a webhook.
- **Open-source certspotter** (MPL-2.0) watches the CT logs **directly**,
  with no SSLMate API, and runs hook scripts. It downloads every log entry,
  so its bandwidth is that of every log. Not quantified here (**UNVERIFIED**).

**crt.sh** (run by Sectigo):

- **No usage policy, rate limit or SLA is published on the site** (front page
  read 2026-11-16).
- The maintainer's posts on the crt.sh group: **"throttled at 60 requests per
  IP address per minute"**, with a burst of 5, and over-limit answers are
  HTTP 50x (2020-01-27). Automated services are acceptable, and anyone
  downloading large datasets should "strongly consider monitoring the CT logs
  directly instead" (2017-07-17). The JSON output (`output=json`) is not
  documented on the site.

**What "up to 24 h" assumes.** It assumes the attacker's certificate is
logged, and that the logs' **maximum merge delay** (MMD) is 24 h:

- The browsers enforce logging: Apple fails the TLS connection for a
  publicly trusted server certificate without enough SCTs. So a certificate
  a device will accept is in at least two logs' pipelines.
- **Chrome's log list today** (v97.3, 2026-11-16T13:35Z): **17 usable RFC 6962
  logs with MMD 86,400 s (24 h)**, and tiled (static-ct-api) logs with MMD
  60 s (**22 usable, 41 more qualified**). An attacker picks the logs, so the
  worst case is the 24 h ones.
- Chrome's policy for **new** applications is shorter: static-ct-api logs
  "must not specify a MMD greater than 1 minute", and RFC 6962 logs "4 hour".
  Logs applying from 2027-01-01 must be static-ct-api. The 24 h worst case
  shrinks as the old logs retire. No date is set.
- **Not in the 24 h:** the aggregator's own ingest lag after a log
  incorporates an entry. Cert Spotter publishes no figure (**UNVERIFIED**).
  Also not in it: the hourly poll itself. The honest bound is
  **24 h + aggregator lag + 1 h**.

### What it changes

- 🔴 **The design does not say whose API key a server polls with**, and the
  unauthenticated tier is not for production. Three ways out, each with a
  cost:
  1. **The operator's key, shared.** 100 single-hostname queries/hour on the
     free plan, at 2 names per server with Pages, is **50 servers**. Large
     ($500/month) is 5,000. But a key shipped in a public repo is not a
     secret, and a key fetched from the service puts the **operator**
     (the threat-model row the CT check exists for) in control of the
     checker's credential. A revoked key reads as "CT not checked", which
     the design already makes a WARN, so it cannot fake a PASS. It can still
     silence detection, and the server would show it.
  2. **Each owner's own free SSLMate account**, pasted into the Network page.
     It is independent of the operator. It adds a sign-up to onboarding and
     a credential to store.
  3. **Unauthenticated, as "personal" use** of a person's own self-hosted
     server. Whether SSLMate reads it that way, when the product ships the
     polling, is a question only SSLMate can answer (**UNVERIFIED**; an
     outward email, the operator's to send).

  **Recommendation:** ask SSLMate (3) before phase 1. Until then build (2) as
  an option and keep crt.sh as the no-credential fallback. Separately, the
  **service** can watch the whole zone with one full-domain query an hour
  (`include_subdomains=true`). That catches the endpoint-bug and DNS-hijack
  rows for every server at once. It does **not** replace the per-server
  check, because it is the operator checking the operator.
- **crt.sh as the fallback is fine at this rate**: one or two requests per
  server per hour is far under 60/min per address. But it has no SLA and
  answers overload with 50x. The design's rule that "neither answering is a
  WARN, never a PASS" is the right response, and it should stay.
- **Threat-model wording** ([§8](managed-network.md#8-threat-model)): "up to
  24 h (a CT log's maximum merge delay, plus the hourly poll)" should add
  "plus the aggregator's ingest lag (unpublished)". The 24 h is the worst
  case for the 17 RFC 6962 logs in Chrome's list today, not a property of
  CT.

### Sources (read 2026-11-16)

- SSLMate CT Search API reference (auth, `after`, `Retry-After`, `pubkey_sha256`, errors): <https://sslmate.com/help/reference/ct_search_api_v1>
- CT Search API pricing: <https://sslmate.com/pricing/ct_search_api>
- Cert Spotter monitoring pricing: <https://sslmate.com/certspotter/pricing>
- Open-source certspotter: <https://github.com/SSLMate/certspotter>
- crt.sh: <https://crt.sh/>; rate limits (2020-01-27): <https://groups.google.com/g/crtsh/c/NZJntKrBdmg>; automated use (2017-07-17): <https://groups.google.com/g/crtsh/c/puZMuqBaWOE>
- Chrome CT log policy (MMD for new logs): <https://googlechrome.github.io/CertificateTransparency/log_policy.html>; log list v3: <https://www.gstatic.com/ct/log_list/v3/log_list.json>
- Apple CT policy: <https://support.apple.com/en-us/103214>

## 3. GeoIP database

**Question.** Which database can show an approximate location (country and
region, [the relay](managed-network.md#the-relay)) on the link confirm, in a
product sold by a company (Innovation Upstream Inc.) as a free and paid
service? What are its terms, attribution, update cadence and account? And is
it compatible with an open-source server: can the database be redistributed,
or must each operator download it?

### Answer

| | MaxMind GeoLite2 City | DB-IP IP to City Lite | IPinfo Lite | IP2Location LITE DB3 |
|---|---|---|---|---|
| **fields** | country, region, city | country, state/province, city, lat/long | **country and ASN only** | country, region, city |
| **licence** | GeoLite EULA (updated 2026-02-12), incorporating CC BY-SA 4.0 | **CC BY 4.0** | CC BY-SA 4.0 | LITE terms: free "for personal or commercial use with attribution" |
| **this use (displayed to end users of a sold service)** | allowed with attribution: you may "display that data to users outside your organization, as long as you attribute the data to MaxMind". Not for "identifying or locating a specific household, individual, or street address" | allowed (CC BY) | allowed, "commercial and non-commercial" | allowed with attribution |
| **attribution** | "This product includes GeoLite Data created by MaxMind, available from https://www.maxmind.com" | a link: "IP Geolocation by DB-IP" | a link to IPinfo | "[Product] uses the IP2Location LITE database for IP geolocation", with a link |
| **updates** | City and Country every **Tuesday and Friday**. Old copies must be **destroyed within 30 days** of a new release | **monthly** (November 2026 release current) | every 24 h | **monthly** (current 2026-11-15, next 2026-12-01) |
| **account to download** | **yes**: account + licence key. 30 downloads/day | **no** | **yes** (free token) | **yes** (free sign-up) |
| **redistribution** | only as CC BY-SA allows, plus EULA flow-down. Otherwise MaxMind's prior written consent. A product that includes the database for its users needs a **Commercial Redistribution License** | allowed (CC BY 4.0), with attribution | allowed (CC BY-SA 4.0) | 🔴 **"You are not permitted to redistribute or resell this product"** |
| **format** | MMDB | MMDB, CSV | MMDB, CSV, JSON, Parquet | BIN, CSV (no MMDB) |

### What it changes

- **The redistribution question does not reach the public repo.** The relay
  does the lookup and sends country and region in a PROXY TLV
  ([the relay](managed-network.md#the-relay); [privacy](managed-network.md#privacy-trade-off)).
  The relay is in the **separate** repository and is run by the operator, so
  muster-server never holds, ships or downloads a GeoIP database. If the relay
  repo is open-sourced ([open question 2](managed-network.md#11-open-questions-for-the-operator)),
  it must **not** vendor the database. Each deployment fetches it at
  **start** (and refreshes it), or rebuilds its image at least monthly; it is
  not baked in once at build time, because GeoLite2 requires old copies to be
  **destroyed within 30 days** of a new release. Fetched that way, it works
  under every licence above (IP2Location's included), provided no image
  containing the database is published.
- 🔴 **The confirm screen needs an attribution line.** Every candidate
  requires one, and the text the visitor sees is rendered by muster-server
  (public), from data the relay chose. The TLV (or the service's
  configuration) should carry a **source id**, so the stock server shows the
  right attribution: "Approximate location: <region>, <country> · IP
  Geolocation by DB-IP". A design change for [flow (b)](managed-network.md#flow-b-link-a-device).
- **IPinfo Lite does not meet the design** (no region). It would do only if
  the confirm showed the country alone.
- **Recommendation: DB-IP City Lite.** CC BY 4.0 with a link is the lightest
  obligation. There is no account or key to manage, the MMDB format is the
  same one GeoLite2 uses, and monthly freshness is plenty for "approximate".
  **GeoLite2 City** is the alternative if accuracy on the confirm turns out to
  matter. It costs an account, a licence key, the 30-day destroy rule, and
  MaxMind's own note that free GeoLite City "is considerably less accurate
  than the paid GeoIP City data, and is not recommended for commercial use
  cases". Accuracy was not compared here (**UNVERIFIED**). Comparing it needs
  real visitor addresses, which is a privacy question in itself.

### Sources (read 2026-11-16)

- GeoLite EULA: <https://www.maxmind.com/en/geolite2/eula>
- GeoLite download, licence key, limits: <https://dev.maxmind.com/geoip/geolite2-free-geolocation-data/>; update schedule: <https://support.maxmind.com/knowledge-base/articles/download-and-update-maxmind-databases>
- GeoLite display and sell: <https://support.maxmind.com/knowledge-base/articles/sell-or-display-data-from-geolite-databases-and-web-services>; redistribution licence: <https://support.maxmind.com/knowledge-base/articles/commercial-license-for-geolite>
- DB-IP Lite: <https://db-ip.com/db/lite.php>; City Lite: <https://db-ip.com/db/download/ip-to-city-lite>
- IPinfo Lite: <https://ipinfo.io/lite>
- IP2Location LITE: <https://lite.ip2location.com/>; DB3: <https://lite.ip2location.com/database/db3-ip-country-region-city>; terms: <https://lite.ip2location.com/data-license>

## Spikes not run here

The rest of [phase 0](managed-network.md#phase-0-spikes-no-product-change),
and what each needs from the operator:

| spike | needs | why it cannot be done from documentation |
|---|---|---|
| **SNI passthrough relay test**: Traefik TCP router matching the zone's names with ALPN preserved, into a relay on `internal/preview/tunnel`; WebSocket and SSE from a real phone; idle past 10 min; latency against Tailscale; throughput at `W` = 128 KiB | **a test domain** (zone + registrar), **production node access** (Traefik config, a relay deployment), **a phone** on a cellular network | it is a measurement on the real edge, like [previews-spike.md](previews-spike.md)'s real-edge section |
| **iOS cookie jars, push and camera**: on a current iOS and the oldest supported one, Safari vs. the installed Home Screen app, both ways; push from the installed app on the network origin; the camera for **Scan** | **two iPhones** (or one plus an old-iOS device), the test domain with a real certificate | the design marks these UNVERIFIED because the behaviour differs by iOS version. Only a device answers |
| **DNS-01 with a staging CA and CAA `accounturi`**: issuance succeeds for the right account and **fails** for another (the negative control) | **the test domain on the chosen provider** (spike 1 recommends Route 53), **two staging ACME accounts** | this is also where the provider's acceptance of a `;`-parameterised CAA value is tested. On Cloudflare, also the served CAA RRset with Universal SSL off |
| **PSL submission** for the product domain | **an outward filing** (a pull request to the Public Suffix List, from the domain's owner, with a `_psl` TXT record) | it is an external, public act. It is the operator's to file, and it takes weeks to merge and longer to reach browsers |
| **CA rate-limit request** (e.g. Let's Encrypt's override for certificates per registered domain) | **an outward filing** by the operator, naming the domain and the expected volume | an external request. Spike 1's write-rate arithmetic gives the volume to ask for: about one order per name per 30 days, doubled with Pages |
| **SSLMate terms question** (new, from spike 2) | **an outward email** from the operator | whether per-server unauthenticated polling counts as "personal" use is SSLMate's to answer |
