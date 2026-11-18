# MCP

With an `mcp` section, tofu-api also serves its API to AI agents over the
[Model Context Protocol](https://modelcontextprotocol.io), at
`<root>/-/mcp` (Streamable HTTP): `/apis/telemetry.example.com/v1/-/mcp` for
a `base_path` of `/apis/telemetry.example.com/v1/namespaces/{namespace}`.
Nothing is written per resource type: the tools are the contract's
operations.

```yaml
mcp:
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

## Tools

| Tool | Operation | Without `writes` |
|------|-----------|------------------|
| `get_<type>` | `GET …/<type>/{id}` | yes |
| `read_<data source>` | `GET …/<data source>` | yes |
| `plan_create_<type>`, `plan_update_<type>` | `POST` / `PUT` with `?dryRun=true` | yes |
| `create_<type>`, `update_<type>`, `delete_<type>` | `POST`, `PUT`, `DELETE` | only with `writes: true` |

Every call goes through the API itself, in process: the contract, the
token and RBAC, the rules (`value`, `default`, `hidden`), the dry run and
the error rules apply as for any HTTP client, and the call shows in the
access log. An error is a tool error whose text is the problem document.

## Context

- The instructions every agent gets: the provider, its resource types and
  data sources, that a resource lives in its namespace, that nothing lists
  them, what `plan_` tools are, whether the server is read only, how to read
  an error; then `mcp.instructions`.
- `documents`: resources `doc://<name>`, Markdown.
- `prompts`: prompts, the user's message.
- The contract itself: the resource `openapi://contract`.

## Authentication

The agent sends its user's bearer token; tofu-api passes it on to every call,
so the agent can do what its user can, no more. With `auth`, the endpoint
itself also requires a valid token.
