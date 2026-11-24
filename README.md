# tofu-api

> Early stage: tested live with the Grafana and random providers; the Foreman
> example's contract is generated from the provider's schemas, its calls not
> yet tested against a Foreman.

Exposes resource types of an OpenTofu-protocol provider as an HTTP API, and
as MCP tools for AI agents, with no code per resource:

```
provider binary ──GetProviderSchema──► resource schemas ──► OpenAPI 3.0 contract
                                                              │
      POST /foreman/v1/namespaces/acme/host ──► validated ───┘──► pkg/tofu Create
                                                              ▲
      AI agent ──MCP──► /foreman/v1/-/mcp ── tools from the contract
```

## Why

An API in front of a product is mostly a client of that product's API, and
a Terraform provider is that client, already written: maintained by others,
tested by its users, complete in what matters for an API. tofu-api runs the
provider over the plugin protocol, without Terraform, and what is left to
write is one YAML file per provider. See [Why](docs/index.md#why) and
[Limits](docs/limits.md).

## For AI agents

Every tofu-api is also an MCP server, at `<root>/-/mcp`: a tool per
operation, each call going through the API with its user's token (RBAC,
rules, contract), a document per resource type generated from the
provider's schema, and prompts. Read only by default: an agent reads and
plans (`plan_create_`, `plan_update_`), and writes only when the
configuration allows it. See [MCP](docs/mcp.md), and
[`examples/grafana/grafana.yaml`](examples/grafana/grafana.yaml), Grafana
folders with an `mcp` section.

```bash
claude mcp add --transport http grafana http://localhost:8080/grafana/v1/-/mcp \
  --header "Authorization: Bearer $TOKEN"
```

## Run

Install the binary, download a provider, describe the API in one file:

```bash
bin install github.com/infrapi/tofu-api ~/.local/bin/tofu-api
```

```yaml
# grafana.yaml
provider:
  binary_path: providers/terraform-provider-grafana_v4.35.0
  config: {url: "${GRAFANA_URL}", auth: "${GRAFANA_AUTH}"}
resources:
  grafana_folder: {}
```

```bash
INFRAPI_CONFIG_DOTENV_FILE=grafana.env tofu-api serve -config grafana.yaml
curl -X POST localhost:8080/grafana/v1/namespaces/acme/folder -d '{"title": "Ops Team"}' -H 'Content-Type: application/json'
```

The whole walkthrough, from nothing, is in [Getting started](docs/getting-started.md).

## Install

```bash
bin install github.com/infrapi/tofu-api ~/.local/bin/tofu-api
```

Or the RPM and DEB packages, the container image `ghcr.io/infrapi/tofu-api`,
or `go install`: see [Install](docs/install.md).

## Documentation

| Page | What |
|---|---|
| [Overview](docs/index.md) | what tofu-api is, and why a provider |
| [Getting started](docs/getting-started.md) | install, download a provider, write one file: an API over Grafana folders, no code |
| [Install](docs/install.md) | release packages, files under `/etc/tofu-api`, installing providers from a mirror |
| [Configuration](docs/configuration.md) | the per-provider YAML file |
| [Authentication](docs/authentication.md) | bearer tokens and RBAC with `auth` |
| [Namespaces](docs/namespaces.md) | a provider process per namespace, credentials from Vault |
| [Data sources](docs/data-sources.md) | read-only data sources, arguments in the query |
| [MCP](docs/mcp.md) | the API as MCP tools for AI agents, with documents and prompts |
| [Dry run](docs/dry-run.md) | `?dryRun=true`: the plan of a request, a resource's drift |
| [Rules](docs/rules.md) | attribute rules, error rules, CEL expressions |
| [Mapping](docs/mapping.md) | provider schema to OpenAPI, and why the service keeps no state |
| [Operations](docs/operations.md) | provider processes, metrics, concurrency |
| [Limits](docs/limits.md) | known limits and what is not covered |
| [Testing](docs/testing.md) | what was tested live, with the random and Grafana providers |

## License

[MIT](LICENSE)
