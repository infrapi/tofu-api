---
title: Testing
description: What tofu-api was tested with live, with the random provider, Grafana 13.3.0 and 11.3, namespaces with Vault and error rules.
weight: 120
---

# Testing

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

## Namespaces with Grafana 13 and Vault

Tested with Grafana 13, Vault 2.2.1 in dev mode, and an Editor service
account token per org (acme in org 2, beta in org 1): ids are then `0:<uid>`,
the org being the token's. acme reading beta's folder, as `0:` or `1:`, is a
404; a dashboard of acme with `org_id: "1"` is a 400 (the provider refuses
`org_id` with a token); an unknown namespace is a 404; with `max_providers: 1`,
beta's first request stopped acme's idle process; beta's process killed, the
next request was a 503 and the one after a 200.

## Error rules with Grafana

With Grafana and rules of the same kind: a taken uid was a 409
`already_exists`, another org's folder a 404, a wrong password a 502
`upstream_auth`, Grafana down a retryable 503.
