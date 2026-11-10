---
title: Configuration
description: The YAML file per provider, with the binary to run, its configuration, the resource types to expose and the rules over them.
weight: 40
---

# Configuration

```yaml
# api:
#   base_path: /{provider}/v1/namespaces/{namespace}   # the paths' start, as by default; {namespace} is required
#   namespace_pattern: '^[a-z0-9][a-z0-9_-]{0,62}$'    # what a namespace matches, as by default; anchored
provider:
  source: terraform-coop/foreman   # [host/]namespace/type, installed (see Providers)
  version: 0.7.0                   # exact
  # binary_path: providers/.../terraform-provider-foreman_v0.7.0   # instead of source and version
  # name: foreman                  # {provider} in the paths; the type, or from the binary's name, by default
  # parallelism: 10                # requests at the provider at once; the others wait
  # timeout: 10m                   # bound on a create, update or delete
  config:                          # the provider block, as in Terraform
    server_hostname: ${FOREMAN_HOSTNAME}          # ${VAR} from the environment, when serving
    client_password: ${FOREMAN_CLIENT_PASSWORD}   # $$ is a $
# auth:                            # a bearer token, and the RBAC's leave (see Authentication)
#   url: https://auth.example.com
resources:
  foreman_host:                    # rules, see below
    attributes: {…}
  foreman_subnet: {}               # exposed as the provider has it
errors: […]                        # see below
```

`resources` names every resource type to expose. `gen` does not read the
environment; `serve` stops if a variable `provider.config` names is not set.
Server settings (address, logs) stay in `INFRAPI_*` variables, as for any
service on the lib.

See [Providers](install.md#providers), [Authentication](authentication.md),
[Namespaces](namespaces.md), [Data sources](data-sources.md) and
[Rules](rules.md) for each section of the file.
