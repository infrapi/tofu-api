---
title: Getting started
description: Install tofu-api, download a provider, write one YAML file and get an HTTP API over Grafana folders, with no code.
weight: 20
---

# Getting started

tofu-api turns a Terraform or OpenTofu provider into an HTTP API, with no
code: you install one binary, give it the provider, and say in a YAML file
which resource types to expose. This guide exposes Grafana's folders as
`/grafana/v1/namespaces/<namespace>/folder`.

You need `curl`, `unzip`, and a Grafana you can reach with an admin login.
To start one locally:

```bash
podman run -d --name grafana -p 3000:3000 docker.io/grafana/grafana:13.3.0   # admin / admin
```

## 1. Install tofu-api

With [bin](https://github.com/marcosnils/bin/), which picks the release
archive matching your OS and architecture:

```bash
bin install github.com/infrapi/tofu-api ~/.local/bin/tofu-api
tofu-api version
```

The packages (rpm, deb with a systemd unit) and the container image are in
[Install](install.md).

## 2. Download the provider

A provider is a binary its authors publish on every release. Take Grafana's,
and unpack it in a `providers` directory next to your files:

```bash
mkdir -p grafana-api/providers && cd grafana-api
curl -fsSLO https://github.com/grafana/terraform-provider-grafana/releases/download/v4.35.0/terraform-provider-grafana_4.35.0_linux_amd64.zip
unzip terraform-provider-grafana_4.35.0_linux_amd64.zip -d providers
ls providers   # terraform-provider-grafana_v4.35.0
```

In production, tofu-api installs it itself from a network mirror, by
`source` and `version`: see [Providers](install.md#providers).

## 3. Describe the API

`grafana.yaml` says which provider to run, how to configure it, and which
resource types to expose:

```yaml
provider:
  binary_path: providers/terraform-provider-grafana_v4.35.0
  config:                       # the provider's own settings, as in a .tf file
    url: ${GRAFANA_URL}         # ${VAR}: read from the environment
    auth: ${GRAFANA_AUTH}

resources:
  grafana_folder: {}            # served as .../folder: the type without its provider prefix
```

That is the whole API: its fields, types and required attributes come from
the provider's schema. [Configuration](configuration.md) adds rules on
attributes, error mappings, data sources and namespaces; the repository's
[`examples/grafana/grafana.yaml`](https://github.com/infrapi/tofu-api/blob/main/examples/grafana/grafana.yaml)
uses them.

`grafana.env` holds the service's own settings, as for every infrapi/lib
service:

```bash
INFRAPI_APP_NAME="grafana-api"
INFRAPI_APP_PLATFORM="sandbox"
INFRAPI_APP_REGION="eu-west-1"
INFRAPI_APP_LOCATION="dc1"
INFRAPI_APP_FQDN="grafana-api.example.com"
INFRAPI_APP_URL="http://localhost:8080"
INFRAPI_APP_LISTEN_ADDRESS="127.0.0.1:8080"
```

## 4. Check the contract

`gen` starts the provider, reads its schema and writes the OpenAPI contract
the API will serve, without calling Grafana. It is also the check of your
file:

```bash
tofu-api gen -config grafana.yaml -o grafana-v1.json
```

## 5. Run it

```bash
export GRAFANA_URL=http://localhost:3000 GRAFANA_AUTH=admin:admin
INFRAPI_CONFIG_DOTENV_FILE=grafana.env tofu-api serve -config grafana.yaml
```

The contract is browsable at <http://localhost:8080/grafana/v1/-/docs>, next
to the service's own endpoints (`-/healthz`, `-/readyz`, `-/metrics`).

## 6. Call it

In another terminal, create a folder in the namespace `acme`, read it and
delete it:

```bash
curl -s -X POST localhost:8080/grafana/v1/namespaces/acme/folder \
  -H 'Content-Type: application/json' -d '{"title": "Ops Team"}'
# {"id":"<id>","title":"Ops Team","uid":"…",…}
curl -s localhost:8080/grafana/v1/namespaces/acme/folder/<id>
curl -s -X DELETE localhost:8080/grafana/v1/namespaces/acme/folder/<id>
```

The folder is in Grafana: tofu-api keeps no state, the provider reads it
back on every call ([Mapping](mapping.md)).

## 7. Next

- Another product is another file: download its provider, list its
  resource types. Run one tofu-api per provider.
- Put the API behind Kayra, with tokens and RBAC: [Authentication](authentication.md).
- Give each namespace its own Grafana organisation or credentials:
  [Namespaces](namespaces.md).
- Let AI agents use it: every tofu-api is also an MCP server, at
  `/grafana/v1/-/mcp` ([MCP](mcp.md)).
- Deploy it with the packages or the image, providers from a mirror:
  [Install](install.md).
