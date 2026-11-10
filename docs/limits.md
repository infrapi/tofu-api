---
title: Limits
description: Known limits of tofu-api, from inferred replacements to provider retries, and what it does not cover.
weight: 110
---

# Limits

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
