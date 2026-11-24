---
title: Install
description: Install tofu-api from release packages or its container image, lay out its files under /etc/tofu-api and install providers from a network mirror.
weight: 30
---

# Install

Each [release](https://github.com/infrapi/tofu-api/releases) has the binary
for linux amd64 and arm64, and RPM and DEB packages. A package holds the
binary and a systemd template, an instance per provider, run as the
`tofu-api` user:

| File | What |
|---|---|
| `/etc/tofu-api/<provider>.yaml` | the provider's file: `provider.source` and `version`, or `binary_path` absolute |
| `/etc/tofu-api/terraformrc` | where providers are installed from and to, if by `source` (`TF_CLI_CONFIG_FILE`); `plugin_cache_dir` under `/var/lib/tofu-api`, the only directory the service writes |
| `/etc/tofu-api/<provider>.env` | `INFRAPI_APP_*`, its own listen port |
| `/etc/tofu-api/<provider>.secrets` | what `${VAR}` reads, such as `FOREMAN_CLIENT_PASSWORD`; 0600 root, read by systemd |
| `/etc/tofu-api/<provider>.vault-secret-id` | namespace mode, if `secret_id_file` names it; 0640 root:tofu-api |

None is shipped, and nothing in the package names a provider:
`/usr/share/doc/tofu-api/examples/` has an `.env` and a `.secrets`, and a
provider's file starts from this repository's `examples/`. The provider's
binary is not in the package either: tofu-api installs it, or
`binary_path` names one already there.

## Container image

Every release also publishes an image, `ghcr.io/infrapi/tofu-api`, for
`linux/amd64` and `linux/arm64`, tagged with the version
(`<major>.<minor>.<patch>`), its minor and `latest`. A container serves one
provider, as a systemd instance does. It runs as an unprivileged user (uid
10001), and reads:

| Path | What |
|---|---|
| `/etc/tofu-api/tofu-api.yaml` | the provider's file (`serve -config`, the default command) |
| `/etc/tofu-api/tofu-api.env` | `INFRAPI_APP_*` (`INFRAPI_CONFIG_DOTENV_FILE`): `INFRAPI_APP_LISTEN_ADDRESS="0.0.0.0:8080"` to be reached from outside the container |
| `/etc/tofu-api/terraformrc` | `TF_CLI_CONFIG_FILE`: the image's has `plugin_cache_dir` and no mirror; mount yours with a `network_mirror` to install providers by `source` |
| `/var/lib/tofu-api` | a volume: the installed providers, kept across restarts |

What `${VAR}` reads, the `.secrets` of a package, goes in the container's
environment (`--env-file`).

```bash
podman run --rm \
  -v ./grafana.yaml:/etc/tofu-api/tofu-api.yaml:ro,z \
  -v ./grafana.env:/etc/tofu-api/tofu-api.env:ro,z \
  -v ./terraformrc:/etc/tofu-api/terraformrc:ro,z \
  -v tofu-api-providers:/var/lib/tofu-api \
  --env-file ./grafana.secrets -p 8080:8080 ghcr.io/infrapi/tofu-api:latest
```

## Providers

With `provider.source` and `provider.version`, tofu-api installs the
provider as `terraform init` does, from Terraform's CLI configuration: the
file `TF_CLI_CONFIG_FILE` names (`~/.terraformrc` otherwise).

```hcl
plugin_cache_dir = "/var/lib/tofu-api/providers"
provider_installation {
  network_mirror {
    url = "https://mirror.example.com/providers/"
  }
}
credentials "mirror.example.com" {
  token = "..."                    # or TF_TOKEN_mirror_example_com
}
```

- A version already in `plugin_cache_dir`, as Terraform lays it out
  (`<host>/<namespace>/<type>/<version>/<os>_<arch>/`), is run from there:
  a restart needs no network.
- Another is downloaded from the
  [network mirror](https://developer.hashicorp.com/terraform/internals/provider-network-mirror-protocol),
  with the token of its host, and checked against the hashes the mirror
  gives (`zh:` of the zip or `h1:` of its contents, one is enough; a mirror
  that gives none is trusted over HTTPS, as by `terraform init`). It is
  unpacked aside, and moved into the cache once checked.
- `gen` installs it too: a `gen` before a deployment leaves the version
  ready for the service.
- A `network_mirror` is the only installation method; `direct` and
  `filesystem_mirror` are refused. `TF_PLUGIN_CACHE_DIR` overrides
  `plugin_cache_dir`. Former versions stay in the cache.

```bash
systemctl enable --now tofu-api@foreman
```
