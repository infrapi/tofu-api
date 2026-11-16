# Dry run

`?dryRun=true` on a `POST` or a `PUT` plans the request and applies nothing,
as `terraform plan` does for one resource. The body is the
request's, and so are the checks: the contract, the token and RBAC, the rules.

```sh
curl -s -X PUT "$API/folder/0:Ops-Team?dryRun=true" \
  -H 'Content-Type: application/json' -d '{"title":"Ops"}'
```

```json
{
  "action": "update",
  "changes": [
    { "path": "title", "current": "Renamed by hand", "planned": "Ops" }
  ],
  "current": { "id": "0:Ops-Team", "title": "Renamed by hand", "uid": "Ops-Team" },
  "planned": { "id": "0:Ops-Team", "title": "Ops", "uid": "Ops-Team" },
  "unknown": []
}
```

| `action` | Means |
|---|---|
| `none` | the resource matches the spec |
| `update` | it differs: the `PUT` would change it in place |
| `replace` | it differs in a way the provider cannot change in place: deleting and creating it again |
| `create` | a `POST` dry run: what would be created |

- `changes` lists what would change, by path: `title`, `settings.theme`,
  `permission[0].role`, `labels["env"]`. A sensitive attribute is listed
  with `sensitive: true` and no values; a value known only once applied with
  `unknown: true`. `unknown` lists those paths; `planned` has them null.
- A `PUT` dry run plans against the resource as it is now, imported by its
  id as for a `GET`: planned with the spec it was created with, the changes
  are its **drift**, what was changed outside of the API. An id the provider
  no longer knows is a 404.
- Hidden and write-only attributes are left out, and so are those with an
  `ignore_drift` rule: one the provider cannot read back after an import
  differs at every dry run.
- With `auth`, a dry run is a `get` for the RBAC: it changes nothing.

## Detecting drift

The service keeps no state: whoever created the resources knows what they
should be. A caller that keeps the spec it sent can check them, on a
schedule:

```
for each resource it created (id + spec):
  PUT …/{id}?dryRun=true {spec}
  404             → deleted outside: alert, or POST it again
  action: none    → nothing
  action: update  → drift: alert, or PUT {spec} to set it back
  action: replace → alert
```

## Limits

- A resource type the provider cannot import has no `GET`, `PUT` or dry run
  of a `PUT`.
- The replacement is inferred from the plan making the resource's `id`
  unknown. A provider that keeps its id while it replaces the resource (a
  plugin framework resource with `UseStateForUnknown`) plans an `update`.
- What an import cannot know comes back as a provider default: a
  `random_string` created with `special: false` imports with `true`, and
  differs at every dry run. `ignore_drift` is for such attributes.
