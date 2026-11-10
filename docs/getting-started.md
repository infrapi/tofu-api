---
title: Getting started
description: Run tofu-api with the Foreman example to generate the contract, serve the API and call it with curl.
weight: 20
---

# Getting started

One binary for every provider; one YAML file per provider says which binary
to run, how to configure it, which resource types to expose and the rules
over them. Another provider is another file, and no code:

[`examples/foreman/foreman.yaml`](https://github.com/infrapi/tofu-api/blob/main/examples/foreman/foreman.yaml) exposes
hosts, hostgroups, domains and subnets in Foreman, a namespace per
organization.

```bash
go run . -h                                              # the commands; go run . gen -h, serve -h for their flags
scripts/fetch-provider.sh terraform-coop/foreman 0.7.0   # prints the binary path, for provider.binary_path

# the contract only, or a check of the file: no Foreman needed
go run . gen -config examples/foreman/foreman.yaml -o foreman-v1.json

# the API
export FOREMAN_HOSTNAME=foreman.example.com FOREMAN_CLIENT_USERNAME=admin FOREMAN_CLIENT_PASSWORD=...
export FOREMAN_ORGANIZATION_ID=1 FOREMAN_LOCATION_ID=2
INFRAPI_CONFIG_DOTENV_FILE=app.env go run . serve -config examples/foreman/foreman.yaml
curl -X POST localhost:8080/foreman/v1/namespaces/acme/domain \
  -H 'Content-Type: application/json' -d '{"name":"acme.example.com"}'   # → {"id":"<id>", "name":…, "fullname":…}
curl localhost:8080/foreman/v1/namespaces/acme/domain/<id>
curl -X DELETE localhost:8080/foreman/v1/namespaces/acme/domain/<id>
```

The contract is served at `/-/docs`, with the service's own endpoints
(`/-/healthz`, `/-/readyz`, `/-/metrics`).
