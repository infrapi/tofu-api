---
title: MCP
description: "The API as MCP tools for AI agents, with generated documents and prompts per resource type."
weight: 87
---

# MCP

tofu-api also serves its API to AI agents over the
[Model Context Protocol](https://modelcontextprotocol.io), read only unless
told otherwise, at
`<root>/-/mcp` (Streamable HTTP): `/apis/telemetry.example.com/v1/-/mcp` for
a `base_path` of `/apis/telemetry.example.com/v1/namespaces/{namespace}`.
Nothing is written per resource type: the tools are the contract's
operations, and the context is generated from the provider's schemas and the
rules. The `mcp` section is optional: it adds what only you know.

```yaml
mcp:
  enabled: true                   # false: no MCP endpoint
  instructions: |                 # added to what tofu-api tells every agent
    A namespace is a team's Grafana organization. Folders hold its dashboards.
  writes: false                   # true: the agent may also create, update and delete
  documents:                      # context, offered as resources doc://<name>
    - name: folders
      title: Folders
      file: docs/folders.md       # relative to this file; or content: inline text
  prompts:                        # ready-made requests the agent's user can pick
    - name: check-drift
      description: Check a folder against its spec
      text: Read the folder, then plan an update with the spec I give you, and show me the changes.
```

A complete example, Grafana folders with their conventions as a document and
a prompt: [`examples/grafana/grafana.yaml`](https://github.com/infrapi/tofu-api/blob/main/examples/grafana/grafana.yaml).

## Tools

| Tool | Operation | Without `writes` |
|------|-----------|------------------|
| `get_<type>` | `GET …/<type>/{id}` | yes |
| `read_<data source>` | `GET …/<data source>` | yes |
| `plan_create_<type>`, `plan_update_<type>` | `POST` / `PUT` with `?dryRun=true` | yes |
| `create_<type>`, `update_<type>`, `delete_<type>` | `POST`, `PUT`, `DELETE` | only with `writes: true`; the `plan_` tools stay |

Every call goes through the API itself, in process: the contract, the
token and RBAC, the rules (`value`, `default`, `hidden`), the dry run and
the error rules apply as for any HTTP client, and the call shows in the
access log. An error is a tool error whose text is the problem document.

## Context

Generated:

- the instructions every agent gets: the provider, its resource types and
  data sources, that a resource lives in its namespace, that nothing lists
  them, what `plan_` tools are, whether the server is read only, how to read
  an error, which documents to read; then `mcp.instructions`;
- a document per resource type, `doc://<type>`: the provider's description,
  each attribute with its type, who sets it (you, the provider) and its
  notes (the provider's description, the rules' `default`, `pattern`,
  lengths, `enum`, sensitive, write only), the nested blocks, the attributes
  the service sets from the namespace, those a plan does not compare
  (`ignore_drift`), and a minimal body. Hidden attributes are not named;
- a document per data source, `doc://<data source>`: its arguments and its
  answer;
- two prompts per resource type: `check-drift-<type>` (read, plan an update
  with the spec, show the drift) and `plan-create-<type>` (write a spec from
  the document, plan its creation);
- the contract itself, the resource `openapi://contract`.

Configured, added to the above: `documents` (resources `doc://<name>`,
Markdown) and `prompts`. One named as a generated one replaces it.

In Claude Code, the instructions are in the agent's context from the start;
it reads a document when the instructions point it there, or when you attach
it with `@`; a prompt is the command `/mcp__<server>__<prompt>`.

## Authentication

The agent sends its user's bearer token; tofu-api passes it on to every call,
so the agent can do what its user can, no more. With `auth`, the endpoint
itself also requires a valid token.

## Bridged MCP servers

`mcp.bridges` serves the tools of other MCP servers as part of the API.
mcp-grafana, for one, knows nothing of namespaces or RBAC: it runs with
whatever credential it is given. Bridged, each of its tools is a route of
the API, authorized as one of its resources, and called with the
namespace's own provider configuration, the one the provider process of the
namespace is configured with.

```yaml
provider:
  config:
    url: ${GRAFANA_URL}             # and, per namespace, its secret: auth
mcp:
  bridges:
    - name: grafana
      url: http://127.0.0.1:8000/mcp
      tools:                        # first match wins; a tool no rule selects is not bridged
        - {match: "query_prometheus.*|list_prometheus_.*", resource: mcp-prometheus}
        - {match: "query_loki.*|list_loki_.*", resource: mcp-loki}
      # writes: true                # the tools that are not read-only too
      headers:                      # every request, ${VAR} from the environment
        Authorization: Bearer ${MCP_GRAFANA_TOKEN}
      call_headers:                 # each call: a key of the namespace's configuration
        X-Grafana-Service-Account-Token: auth
        X-Grafana-Org-Id: grafana_org_id   # a key only bridges read
      # timeout: 5m
      # protocol_version: 2025-06-18
```

Each resource is one route, `{tool}` one of its tools:

| Route | Tools | RBAC |
|-------|-------|------|
| `GET <base>/<resource>/{tool}?arguments=<JSON object>` | the read-only ones | `get` on `<resource>` |
| `POST <base>/<resource>/{tool}`, the arguments the body | the others, with `writes` | `create` on `<resource>` |

A resource is named `mcp-<family>`: a role reads as what it grants, and a
bridged family never shares a resource with one of the API's resource types
or data sources (a route of the contract under the same resource is refused
at start).

The routes are in the contract the service serves at start, which Kayra
reads: a role granting `mcp-prometheus` and not `mcp-loki` on the API gives
the Prometheus tools and not the Loki ones. `tofu-api gen` writes the contract
without them: they come from the bridged server, listed at start.

An agent sees each bridged tool as a tool of this server, with its own
arguments and a `namespace`.

`call_headers` reads the namespace's configuration: `provider.config` with
the namespace's secret over it. A key a bridge reads that the provider's
configuration block does not have stays in the secret for the bridges only,
and is not passed to the provider: `grafana_org_id`, for mcp-grafana's
`X-Grafana-Org-Id`, which the Grafana provider refuses beside a service
account token (`org_id is only supported with basic auth`). Without it,
mcp-grafana's tools that go through Grafana's Kubernetes API fail outside
org 1 (`API key does not belong to the requested organization`). Any other
key the provider does not know is still an error.

The namespace's configuration is read again from Vault after a minute, so a
rotated credential applies within one. A namespace whose configuration lacks
a key of `call_headers` is a `502 upstream_auth`; a tool's own error, a
`422 tool_error`; the bridged server down, a retryable `502`.

See lib's [mcp/bridge](https://infrapi.github.io/lib/packages/mcp/#bridging-another-mcp-server).

## Several APIs behind one endpoint

[Ülgen](https://infrapi.github.io/ulgen/) puts this endpoint behind a single
MCP endpoint, next to other MCP servers. The agent logs in at Kayra once and
gets every server's tools there, with the namespace as their `tenant`
argument. Ülgen passes the caller's token on to tofu-api, which still checks
every call itself: `authorize: upstream`, `tenant: {argument: namespace}`.
