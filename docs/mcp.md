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
    A namespace is a tenant. Folders hold a team's dashboards.
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

## Several APIs behind one endpoint

[ulgen](https://infrapi.github.io/ulgen/) puts this endpoint behind a single
MCP endpoint, next to other MCP servers. The agent logs in at Kayra once and
gets every server's tools there, with the namespace as their `tenant`
argument. ulgen passes the caller's token on to tofu-api, which still checks
every call itself: `authorize: upstream`, `tenant: {argument: namespace}`.
