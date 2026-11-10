---
title: Namespaces
description: The path's namespace, and a provider process per namespace configured with the namespace's own credentials from Vault.
weight: 60
---

# Namespaces

A namespace is the path's `{namespace}`: lower case letters, digits, `-` and
`_` (`^[a-z0-9][a-z0-9_-]{0,62}$`) unless `api.namespace_pattern` says
otherwise, such as `^[A-Za-z0-9_.-]{1,63}$`. Another is a 400. Keep `/` out of it: the namespace is also a
segment of the path, and may end up in a Vault path or in an attribute
through a rule's `ns`.

Without `namespaces`, one provider, configured once, serves every namespace: the
namespaces share its credentials, and only rules keep them apart. With it,
each namespace gets a provider process of its own, configured with the
namespace's secret in Vault laid over `provider.config`:

```yaml
namespaces:
  vault:
    address: ${VAULT_ADDR}
    # ca_cert: /etc/ssl/vault-ca.pem          # when the system's CAs do not sign Vault's
    # approle_mount: approle                  # where the AppRole method is mounted
    role_id: ${VAULT_ROLE_ID}
    secret_id_file: /run/secrets/vault-secret-id   # read at each login; or secret_id
    # mount: secret                          # where the KV v2 engine is mounted; kv-{namespace} for one per namespace
    path: tofu-api/foreman/{namespace}   # under the mount; mount or path must have {namespace}
  # idle: 10m
  # max_age: 1h
  # max_providers: 20
```

```sh
vault kv put secret/tofu-api/foreman/acme organization_id=3 location_id=2 \
  client_username=tofu-api-acme client_password=...   # a user limited to acme's organization
```

A namespace exists for the service once its secret does, which needs its
tenant in the product first (an organization in Foreman or Grafana):
onboarding comes before the API, with an administrator's credentials the
service never holds.

A namespace whose engine the AppRole's policy does not cover is a 502
`upstream_auth`, not a 404: Vault refuses before it can tell the engine is
missing.

- **Isolation comes from the credentials.** With credentials limited to the
  namespace's tenant, such as a Grafana service account token of its org,
  the provider of `acme` reaches nothing else, whatever the request says;
  with admin credentials, an id in the body still reaches any tenant.
- **A namespace without a secret is a 404.** Adding one is a `vault kv put`,
  without restarting the service.
- **A process starts on its namespace's first request** (about 0.4 s for
  Grafana), and stops after `idle`, or after `max_age` once no request
  holds it, so that a rotated secret is read again. At `max_providers`
  (about 75 MiB each for Grafana), the least recently used idle process
  stops to make room; when every one is busy, the request is a retryable
  503.
- **`parallelism` is per namespace**: one namespace's requests do not wait for
  another's.
- **A process that stops answering is replaced**: the call that finds it
  gone (gRPC `Unavailable`, as when the process died) fails, a 503 with the
  example's rules, and the namespace's next request starts another.
- **The service logs in to Vault at startup** (`pkg/vault`), and stops if
  the login is refused; the client logs in again before its token expires,
  or when Vault refuses it. Vault down is a retryable 503 for a namespace
  whose process is not running; running ones go on.
- `gen` reads the schemas from a provider with no configuration: it needs
  neither Vault nor credentials.

The namespaces' processes and their metrics: see
[Operations](operations.md#what-runs). Tested with Grafana 13 and Vault: see
[Testing](testing.md#namespaces-with-grafana-13-and-vault).
