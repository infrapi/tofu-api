---
title: Rules
description: Rules over the provider's schemas, error rules mapping provider failures to HTTP errors, and CEL expressions.
weight: 80
---

# Rules

Rules lay over the provider's schemas what a Terraform module adds around a
bare resource, and say which HTTP error a provider's failure stands for:

```yaml
resources:
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

## Errors

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

Tested with Grafana: see [Testing](testing.md#error-rules-with-grafana).

## Expressions

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
