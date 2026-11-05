# tofu-api

> Early stage: tested live with the Grafana and random providers; the Foreman
> example's contract is generated from the provider's schemas, its calls not
> yet tested against a Foreman.

Exposes resource types of an OpenTofu-protocol provider as an HTTP API with
no code per resource:

```
provider binary ──GetProviderSchema──► resource schemas ──► OpenAPI 3.0 contract
                                                              │
      POST /foreman/v1/namespaces/acme/host ──► validated ───┘──► pkg/tofu Create
```

- the contract is generated from each type's schema, so the provider is the
  only authority on fields, types, required and sensitive attributes;
- `infrapi/lib/pkg/server` loads it, serves it at `/-/docs` and rejects any
  request that does not match it, before a handler runs;
- the handlers call `pkg/tofu`, which drives the provider.

## Why

An API in front of a product, say an internal API to create hosts in
Foreman, is mostly a client of that product's API: its resources and their
fields, authentication, pagination, asynchronous tasks, errors, and how all
of these change from one version of the product to the next. Writing that
client is the bulk of the work, and keeping it right is the bulk of the
maintenance.

A Terraform provider is that client, already written:

- **maintained by others**: the product's vendor or its community follows
  the product's releases; moving to a new one is bumping the provider's
  version and regenerating the contract, whose diff is the API's change;
- **tested by its users**: everyone who manages that product with Terraform
  or OpenTofu runs the same create, read, update and delete code daily,
  against real installations of every version, on top of the provider's own
  acceptance tests: far more than a client written for one API would ever be;
- **complete in what matters for an API**: each resource's schema (required,
  computed, sensitive, what forces a replacement), import by id, and the
  product's errors.

tofu-api runs the provider as Terraform does, over the plugin protocol, but
without Terraform: no HCL, no plan, no state file. Callers that are not
Terraform, such as a portal, a script or another service, get a plain REST
API with an OpenAPI contract. What is left to write is what is specific to
the platform, in one YAML file per provider rather than in code:

- which resource types to expose, and the rules over them: values the
  service sets, defaults, patterns, enums, hidden attributes;
- the namespaces: a provider process per namespace, configured with the
  namespace's own credentials from Vault, so that what a caller reaches is
  what those credentials allow;
- which HTTP error each of the product's errors stands for.

The limits come with it: the API does what the provider does, resources but
no data sources and no list (the service keeps nothing to list), and a bug
in the provider is a bug in the API. See [Limits](#limits).

## Run

One binary for every provider; one YAML file per provider says which binary
to run, how to configure it, which resource types to expose and the rules
over them. Another provider is another file, and no code:

[`examples/foreman/foreman.yaml`](examples/foreman/foreman.yaml) exposes
hosts, hostgroups, domains and subnets in Foreman, a namespace per
organization.

```bash
scripts/fetch-provider.sh terraform-coop/foreman 0.7.0   # prints the binary path, for provider.binary_path

# the contract only, or a check of the file: no Foreman needed
go run . gen -config examples/foreman/foreman.yaml -o foreman-v1.json

# the API
export FOREMAN_HOSTNAME=foreman.example.com FOREMAN_CLIENT_USERNAME=admin FOREMAN_CLIENT_PASSWORD=...
export FOREMAN_ORGANIZATION_ID=1 FOREMAN_LOCATION_ID=2
INFRAPI_CONFIG_DOTENV_FILE=app.env go run . serve -config examples/foreman/foreman.yaml
curl -X POST localhost:8080/foreman/v1/namespaces/acme/domain \
  -H 'Content-Type: application/json' -d '{"name":"acme.example.com"}'   # → {"id":"<id>", "name":…, "fullname":…}
curl localhost:8080/foreman/v1/namespaces/acme/domain/<id>
curl -X DELETE localhost:8080/foreman/v1/namespaces/acme/domain/<id>
```

The contract is served at `/-/docs`, with the service's own endpoints
(`/-/healthz`, `/-/readyz`, `/-/metrics`).

## Install

Each [release](https://github.com/infrapi/tofu-api/releases) has the binary
for linux amd64 and arm64, and RPM and DEB packages. A package holds the
binary and a systemd template, an instance per provider, run as the
`tofu-api` user:

| File | What |
|---|---|
| `/etc/tofu-api/<provider>.yaml` | the provider's file: `provider.source` and `version`, or `binary_path` absolute |
| `/etc/tofu-api/terraformrc` | where providers are installed from and to, if by `source` (`TF_CLI_CONFIG_FILE`); `plugin_cache_dir` under `/var/lib/tofu-api`, the only directory the service writes |
| `/etc/tofu-api/<provider>.env` | `INFRAPI_APP_*`, its own listen port |
| `/etc/tofu-api/<provider>.secrets` | what `${VAR}` reads, such as `FOREMAN_CLIENT_PASSWORD`; 0600 root, read by systemd |
| `/etc/tofu-api/<provider>.vault-secret-id` | namespace mode, if `secret_id_file` names it; 0640 root:tofu-api |

None is shipped, and nothing in the package names a provider:
`/usr/share/doc/tofu-api/examples/` has an `.env` and a `.secrets`, and a
provider's file starts from this repository's `examples/`. The provider's
binary is not in the package either: tofu-api installs it, or
`binary_path` names one already there.

### Providers

With `provider.source` and `provider.version`, tofu-api installs the
provider as `terraform init` does, from Terraform's CLI configuration: the
file `TF_CLI_CONFIG_FILE` names (`~/.terraformrc` otherwise).

```hcl
plugin_cache_dir = "/var/lib/tofu-api/providers"
provider_installation {
  network_mirror {
    url = "https://mirror.example.com/providers/"
  }
}
credentials "mirror.example.com" {
  token = "..."                    # or TF_TOKEN_mirror_example_com
}
```

- A version already in `plugin_cache_dir`, as Terraform lays it out
  (`<host>/<namespace>/<type>/<version>/<os>_<arch>/`), is run from there:
  a restart needs no network.
- Another is downloaded from the
  [network mirror](https://developer.hashicorp.com/terraform/internals/provider-network-mirror-protocol),
  with the token of its host, and checked against the hashes the mirror
  gives (`zh:` of the zip or `h1:` of its contents, one is enough; a mirror
  that gives none is trusted over HTTPS, as by `terraform init`). It is
  unpacked aside, and moved into the cache once checked.
- `gen` installs it too: a `gen` before a deployment leaves the version
  ready for the service.
- A `network_mirror` is the only installation method; `direct` and
  `filesystem_mirror` are refused. `TF_PLUGIN_CACHE_DIR` overrides
  `plugin_cache_dir`. Former versions stay in the cache.

```bash
systemctl enable --now tofu-api@foreman
```

## Configuration

```yaml
# api:
#   base_path: /{provider}/v1/namespaces/{namespace}   # the paths' start, as by default; {namespace} is required
#   namespace_pattern: '^[a-z0-9][a-z0-9_-]{0,62}$'    # what a namespace matches, as by default; anchored
provider:
  source: terraform-coop/foreman   # [host/]namespace/type, installed (see Providers)
  version: 0.7.0                   # exact
  # binary_path: providers/.../terraform-provider-foreman_v0.7.0   # instead of source and version
  # name: foreman                  # {provider} in the paths; the type, or from the binary's name, by default
  # parallelism: 10                # requests at the provider at once; the others wait
  # timeout: 10m                   # bound on a create, update or delete
  config:                          # the provider block, as in Terraform
    server_hostname: ${FOREMAN_HOSTNAME}          # ${VAR} from the environment, when serving
    client_password: ${FOREMAN_CLIENT_PASSWORD}   # $$ is a $
types:
  foreman_host:                    # rules, see below
    attributes: {…}
  foreman_subnet: {}               # exposed as the provider has it
errors: […]                        # see below
```

`types` names every resource type to expose. `gen` does not read the
environment; `serve` stops if a variable `provider.config` names is not set.
Server settings (address, logs) stay in `INFRAPI_*` variables, as for any
service on the lib.

## Namespaces

A namespace is the path's `{namespace}`: lower case letters, digits, `-` and
`_` (`^[a-z0-9][a-z0-9_-]{0,62}$`) unless `api.namespace_pattern` says
otherwise, such as `^[A-Za-z0-9_.-]{1,63}$`. Another is a 400. Keep `/` out of it: the namespace is also a
segment of the path, and may end up in a Vault path or in an attribute
through a rule's `ns`.

Without `namespaces`, one provider, configured once, serves every namespace: the
namespaces share its credentials, and only rules keep them apart. With it,
each namespace gets a provider process of its own, configured with the
namespace's secret in Vault laid over `provider.config`:

```yaml
namespaces:
  vault:
    address: ${VAULT_ADDR}
    # ca_cert: /etc/ssl/vault-ca.pem          # when the system's CAs do not sign Vault's
    # approle_mount: approle                  # where the AppRole method is mounted
    role_id: ${VAULT_ROLE_ID}
    secret_id_file: /run/secrets/vault-secret-id   # read at each login; or secret_id
    # mount: secret                          # where the KV v2 engine is mounted; kv-{namespace} for one per namespace
    path: tofu-api/foreman/{namespace}   # under the mount; mount or path must have {namespace}
  # idle: 10m
  # max_age: 1h
  # max_providers: 20
```

```sh
vault kv put secret/tofu-api/foreman/acme organization_id=3 location_id=2 \
  client_username=tofu-api-acme client_password=...   # a user limited to acme's organization
```

A namespace exists for the service once its secret does, which needs its
tenant in the product first (an organization in Foreman or Grafana):
onboarding comes before the API, with an administrator's credentials the
service never holds.

A namespace whose engine the AppRole's policy does not cover is a 502
`upstream_auth`, not a 404: Vault refuses before it can tell the engine is
missing.

- **Isolation comes from the credentials.** With credentials limited to the
  namespace's tenant, such as a Grafana service account token of its org,
  the provider of `acme` reaches nothing else, whatever the request says;
  with admin credentials, an id in the body still reaches any tenant.
- **A namespace without a secret is a 404.** Adding one is a `vault kv put`,
  without restarting the service.
- **A process starts on its namespace's first request** (about 0.4 s for
  Grafana), and stops after `idle`, or after `max_age` once no request
  holds it, so that a rotated secret is read again. At `max_providers`
  (about 75 MiB each for Grafana), the least recently used idle process
  stops to make room; when every one is busy, the request is a retryable
  503.
- **`parallelism` is per namespace**: one namespace's requests do not wait for
  another's.
- **A process that stops answering is replaced**: the call that finds it
  gone (gRPC `Unavailable`, as when the process died) fails, a 503 with the
  example's rules, and the namespace's next request starts another.
- **The service logs in to Vault at startup** (`pkg/vault`), and stops if
  the login is refused; the client logs in again before its token expires,
  or when Vault refuses it. Vault down is a retryable 503 for a namespace
  whose process is not running; running ones go on.
- `gen` reads the schemas from a provider with no configuration: it needs
  neither Vault nor credentials.

What runs:

```sh
curl -s localhost:8080/-/providers   # the namespaces' processes: state, started, last used, requests in flight, served, failed
curl -s localhost:8080/-/metrics | grep ^tofu_api
```

| Metric | |
|---|---|
| `tofu_api_providers` | processes running |
| `tofu_api_provider_starts_total{result}` | `ok`, or `failed` (an unknown namespace included) |
| `tofu_api_provider_stops_total{reason}` | `idle`, `max_age`, `evicted`, `gone`, `shutdown` |

The metrics have no namespace label, namespaces being unbounded: `/-/providers`
names them. A process stopping once its last call ends is still counted,
no longer listed.

Tested with Grafana 13, Vault 2.2.1 in dev mode, and an Editor service
account token per org (acme in org 2, beta in org 1): ids are then `0:<uid>`,
the org being the token's. acme reading beta's folder, as `0:` or `1:`, is a
404; a dashboard of acme with `org_id: "1"` is a 400 (the provider refuses
`org_id` with a token); an unknown namespace is a 404; with `max_providers: 1`,
beta's first request stopped acme's idle process; beta's process killed, the
next request was a 503 and the one after a 200.

## Rules

Rules lay over the provider's schemas what a Terraform module adds around a
bare resource, and say which HTTP error a provider's failure stands for:

```yaml
types:
  foreman_host:
    attributes:
      name:
        pattern: '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'
      provision_method:
        enum: [build, image]
      enable_bmc:
        hidden: true                      # not the caller's choice
  foreman_domain:
    attributes:
      fullname:
        default: spec.name                # when the caller sends none
  grafana_folder:
    attributes:
      org_id:
        value: ns                         # the service's, not the caller's
        hidden: true                      # nor shown to them
```

| setting | contract | request |
|---|---|---|
| `value` | attribute removed: sending it is a 400 | set by the service; a resource whose state holds another value is another namespace's (404) |
| `default` | optional, `x-default` and the description name the expression | used when the attribute is absent; on an update the current value wins, so a rename keeps the uid |
| `pattern`, `minLength`, `maxLength`, `enum` | added to the attribute's schema | checked by the contract, and on a default's result |
| `hidden` | attribute removed from request and response | sending it is a 400; the provider's default applies, or the rule's `value`/`default` |
| `description` | replaces the provider's | |

An unknown key, a misspelt `maxlength` included, stops the service.

### Errors

`pkg/tofu` classifies what it can: a configuration the provider refuses is a
422 `invalid_configuration`, a change that needs replacing the resource a 409
`replace_required`, an unknown id a 404. Any other failure is a 502, with the
provider's text. The `errors` list matches that text with regular
expressions, in order, and gives the HTTP error it stands for; one none
matches stays a 502 `upstream_error`:

```yaml
errors:
  - code: already_exists                       # the problem's "code"
    match: 'statusCode: \[422\][\s\S]*has already been taken'
    status: 409
  - code: upstream_unavailable
    match: connection refused|no such host
    status: 503
    retryable: true
```

In single quotes, YAML keeps backslashes as they are: `'\('` is the regular
expression `\(`. `title` defaults to the status's text.

With Grafana and rules of the same kind: a taken uid was a 409
`already_exists`, another org's folder a 404, a wrong password a 502
`upstream_auth`, Grafana down a retryable 503.

### Expressions

`value` and `default` are [CEL](https://cel.dev) expressions, the language of
Kubernetes' `x-kubernetes-validations`, with its strings and regex
extensions:

| | |
|---|---|
| `spec.title.replace(" ", "-")` | `Platform Team` → `Platform-Team` |
| `spec.title.lowerAscii()` | lower case |
| `"%s-%s".format([namespace, spec.title])` | formatting |
| `regex.replace(spec.title, "[^a-z]", "_")` | regular expressions |
| `has(spec.uid) ? spec.uid : "none"` | whether the caller sent an attribute |

`value` reads `ns`, the namespace, only (CEL reserves the word `namespace`); `default` reads `ns` and `spec.<attribute>`.
JSON numbers are doubles in CEL, which does not mix them with integers:
`spec.size + 1.0`, not `spec.size + 1`. The file is checked against the
schemas at start: a type the provider does not have, an unknown or computed
attribute, an expression that does not compile or reads anything else stops
the service.

No contract is kept in the repository: the service generates it at start
from the file and the provider's schemas, and serves it at
`/-/openapi/<provider>/v1.json` and in Swagger UI at `/-/docs`. `tofu-api gen`
writes it without starting the service, or checks a file before it is
deployed: it fails as the service would.

## Mapping

| provider schema | request (`<Type>Spec`) | response (`state`) |
|---|---|---|
| required | `required` | yes |
| optional, optional+computed | optional | yes |
| computed only, `id` | absent: sending one is a 400 | yes |
| sensitive, write-only | `writeOnly`, `format: password` | absent |
| deprecated | `deprecated: true` | `deprecated: true` |
| nested block list/set | array, `minItems`/`maxItems`, `uniqueItems` for set | |
| nested block map | object, `additionalProperties` | |
| `set(T)` / `map(T)` / `object` / `dynamic` | array unique / object / closed object / any | |

## No state in the service

A resource is returned flat, as the provider reports it, `id` included,
without sensitive values or hidden attributes. The service keeps nothing; the caller keeps the id.
`GET`, `PUT` and `DELETE` on `/{id}` rebuild the state from the id by
importing the resource from the provider, as `terraform import` does, then check that
every attribute a `value` rule fixes has the namespace's value: an id from another
namespace is a 404. There is no list, since nothing is kept to list.

## Tested with the random provider

POST (201), PUT (200), GET, DELETE (204), and 400s from the contract for a
wrong type, an unknown field, a computed field and a missing required one.

## Tested with Grafana 13.3.0

With the provider 4.35.0, under the namespace `acme`, the caller sending
`org_id`: folders, dashboards, teams and folder permission items created,
read, updated (with or without `org_id`, which is kept) and deleted; an id
already deleted is a 404.

Earlier, with Grafana 11.3: folder, team and dashboard created, the folder
renamed in place (same uid), read and deleted by id; an unknown id is a 404.
With rules deriving a folder's uid from its title: a folder without uid
gets `Platform-Team` from "Platform Team" and keeps it when renamed; a
title that gives an invalid uid, an invalid uid, an `org_id` and
a permission outside the enum are 400s; another namespace's id is a 404.

## Concurrency

Every request goes to one provider process (the namespace's, with `namespaces`), over one gRPC connection;
the provider runs each call in its own goroutine, as under Terraform. At
most `parallelism` requests are at the provider at once (10, as Terraform's
`-parallelism`); the others wait, and one whose caller leaves while waiting
is a 503 that never reached the provider.

A create, update or delete is not cancelled when the caller leaves: a call
cut short may or may not have been made, and no one would know which. It
runs to its end, bounded by `timeout`. A create whose caller left is then
deleted, since no one got its id; reads are cancelled with their request.

Nothing orders two changes to the same resource: two `PUT`s at once both
apply, and the last one wins.

## Limits

- **Replacement is inferred.** `pkg/tofu` (v0.8.0) sees a replacement when
  the plan makes the `id` unknown, and `PUT` is then a 409: the caller
  deletes and creates, and gets a new id. Changes it misses are applied as
  if in place: `random_string` from `length: 8` to `12` returns 200 with an
  8-character `result`, and the Grafana provider ignores a folder's new
  `uid` without an error (the response shows the uid it kept).
- **Failures during apply are text.** Error rules recover the status from
  the provider's message, which depends on its wording.
- **The provider's text is the detail**, internal URLs included.
- **A failed call takes 14 s with the Grafana provider's defaults**: 3
  retries waiting 2, 4 then 8 s, on a connection error and on 429, 5xx, 401
  and 412, a taken uid included. `retries = 0` and `retry_status_codes = []`
  are read as unset and fall back to those defaults; `GRAFANA_RETRIES=0` in
  the environment does work. For an API, whose caller retries a 503:
  `"retry_status_codes": ["429", "5xx"], "retries": 1, "retry_wait": 1`.
- **An id the provider cannot read is a 502**, before the namespace check,
  unless an error rule says otherwise: the Grafana provider answers "Resource
  not found" for a team or a permission item that is gone, and Grafana's 403
  for a folder of an org it cannot read, both of which the example's
  `not_found` rule makes a 404 (the 403 not tested again since the switch to
  import).
- **A provider may store another value than it was given**, without an
  error: the Grafana provider puts a resource in the default org when
  `org_id` is not a number. A resource that does not hold what a `value`
  rule fixes after a create is deleted, and the create is a 502.

## Not covered

- What rules cannot say: one call making several resources (a folder and
  its default permissions), or renaming an attribute (`name` in the API for
  a resource's `title`).
- References between resources (a folder's uid in a dashboard): the caller
  passes values.
- Credentials per region, or from another source than Vault.
- Without `namespaces`, a provider process that dies is not replaced: the
  service needs restarting.
