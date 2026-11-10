---
title: Authentication
description: Bearer tokens checked by an authentication service, and RBAC decisions per verb, resource type and namespace.
weight: 50
---

# Authentication

Without `auth`, the API is open: whoever reaches it acts with the
provider's credentials. With it, every request needs a bearer token of an
authentication service, as infrapi/lib's `pkg/auth` checks them:

```yaml
api:
  base_path: /apis/stacks.example.com/v1/namespaces/{namespace}
auth:
  url: https://auth.example.com    # resolves a token (POST /oauth2/userinfo), and its RBAC decides (pkg/auth's AuthorizePath)
  # cache_ttl: 1h                  # how long a token's identity is kept; -1 for not at all
  # authorize: true                # ask the RBAC for every request; false: a valid token is enough
  # api: stacks.example.com        # the API group the RBAC's rules name; base_path's /apis/<group>/ by default
```

- **Who**: a request without a token, or with one the service refuses, is
  a 401. The service decides which tokens are valid (an identity
  provider's, a service account's); tofu-api takes bearer tokens only.
- **What**: the RBAC is asked whether the token's caller may do the
  request's verb on the resource type in the path's namespace: `create`
  (POST), `get` (GET), `update` (PUT), `delete` (DELETE), on `host` for
  `foreman_host`. A refusal is a 403 with the RBAC's reason, and the
  provider is not called. Its answers are never kept: a changed role
  applies to the next request.
- The operational endpoints under `/-/` (health, metrics, contracts, docs)
  stay open; `/-/providers` needs a token.
- The contract declares a bearer scheme: Swagger UI's Authorize button
  sends it with Try it out.
- The provider's credentials do not change: the token says who may call
  tofu-api, not what the provider may do. A proxy in front of tofu-api
  must pass `Authorization` on as it is.
- A request is checked against the contract before its token: a body the
  contract refuses is a 400 even without one.
