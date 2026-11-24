---
title: tofu-api
description: Exposes resource types of an OpenTofu-protocol provider as an HTTP API with an OpenAPI contract, and as MCP tools for AI agents, with no code per resource.
weight: 10
---

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

- the contract is generated from each type's schema, so the provider is the
  only authority on fields, types, required and sensitive attributes;
- `infrapi/lib/pkg/server` loads it, serves it at `<root>/-/docs` (`/foreman/v1/-/docs` by default) and rejects any
  request that does not match it, before a handler runs;
- the handlers call `pkg/tofu`, which drives the provider;
- the same API is served to AI agents over the Model Context Protocol, at
  `<root>/-/mcp`: see [For AI agents](#for-ai-agents).

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
without Terraform: no HCL, no state file. Callers that are not
Terraform, such as a portal, a script or another service, get a plain REST
API with an OpenAPI contract. What is left to write is what is specific to
the platform, in one YAML file per provider rather than in code:

- which resource types to expose, and the rules over them: values the
  service sets, defaults, patterns, enums, hidden attributes;
- the namespaces: a provider process per namespace, configured with the
  namespace's own credentials from Vault, so that what a caller reaches is
  what those credentials allow;
- which HTTP error each of the product's errors stands for.

The limits come with it: the API does what the provider does, no list of a
type's resources (the service keeps nothing to list), and a bug in the
provider is a bug in the API. See [Limits](limits.md).

## For AI agents

Every tofu-api is also an MCP server, natively: no plugin, no code, no
separate service. An agent such as Claude Code connects to `<root>/-/mcp`
with its user's token and gets:

- **a tool per operation**, generated from the contract: `get_` to read a
  resource, `read_` for a data source, `plan_create_` and `plan_update_` to
  see what a change would do, and, with `mcp.writes: true`, `create_`,
  `update_`, `delete_`;
- **the same API, not a copy of it**: each call goes through the service's
  own handlers, so the token, the RBAC, the rules and the contract's checks
  apply as for any HTTP client, and the agent can do what its user can, no
  more;
- **context generated from the provider**: a document per resource type
  and data source (its attributes, who sets them, the rules' constraints, a
  minimal body), prompts to check a resource's drift or plan a new one, and
  instructions on how the tools fit together;
- **what only you know**, in the configuration: instructions, documents and
  prompts of your own.

Read only by default: an agent reads and plans, and changes nothing until
the configuration allows it. See [MCP](mcp.md).

## Pages

| Page | What |
|---|---|
| [Getting started](getting-started.md) | install, download a provider, write one file: an API over Grafana folders, no code |
| [Install](install.md) | release packages, files under `/etc/tofu-api`, installing providers from a mirror |
| [Configuration](configuration.md) | the per-provider YAML file |
| [Authentication](authentication.md) | bearer tokens and RBAC with `auth` |
| [Namespaces](namespaces.md) | a provider process per namespace, credentials from Vault |
| [Data sources](data-sources.md) | read-only data sources, arguments in the query |
| [Rules](rules.md) | attribute rules, error rules, CEL expressions |
| [Dry run](dry-run.md) | `?dryRun=true`: plan a create or an update, see a resource's drift |
| [MCP](mcp.md) | the API as MCP tools for AI agents, with generated documents and prompts |
| [Mapping](mapping.md) | provider schema to OpenAPI, and why the service keeps no state |
| [Operations](operations.md) | provider processes, metrics, concurrency |
| [Limits](limits.md) | known limits and what is not covered |
| [Testing](testing.md) | what was tested live, with the random and Grafana providers |
