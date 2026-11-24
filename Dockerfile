# The tofu-api image GoReleaser publishes on every release
# (ghcr.io/infrapi/tofu-api, see .goreleaser.yaml): the tofu-api binary it
# has just built, for each platform, on Alpine. A container per provider, as
# a systemd instance per provider with the packages. Mount:
#
#   /etc/tofu-api/tofu-api.yaml   the provider's file (-config)
#   /etc/tofu-api/tofu-api.env    INFRAPI_APP_*, its listen address
#                                 0.0.0.0:8080 to be reached from outside
#   /etc/tofu-api/terraformrc     with a network_mirror, to install providers
#                                 by source; the image's has none
#   /var/lib/tofu-api             a volume: the installed providers, kept
#                                 across restarts
#
#   podman run -v ./grafana.yaml:/etc/tofu-api/tofu-api.yaml:ro,z \
#     -v ./grafana.env:/etc/tofu-api/tofu-api.env:ro,z \
#     -v tofu-api-providers:/var/lib/tofu-api -p 8080:8080 ghcr.io/infrapi/tofu-api
#   podman run --rm ghcr.io/infrapi/tofu-api version
FROM docker.io/library/alpine:3.22

# ca-certificates: the providers' APIs, the mirror, Kayra and Vault over
# TLS. curl: the healthcheck an orchestrator runs.
RUN apk add --no-cache ca-certificates curl \
 && adduser -D -H -u 10001 tofu-api \
 && mkdir -p /etc/tofu-api /var/lib/tofu-api/providers \
 && printf 'plugin_cache_dir = "/var/lib/tofu-api/providers"\n' > /etc/tofu-api/terraformrc \
 && chown -R tofu-api /var/lib/tofu-api

ARG TARGETPLATFORM
COPY $TARGETPLATFORM/tofu-api /usr/local/bin/

ENV INFRAPI_CONFIG_DOTENV_FILE=/etc/tofu-api/tofu-api.env \
    TF_CLI_CONFIG_FILE=/etc/tofu-api/terraformrc
VOLUME /var/lib/tofu-api
USER tofu-api
EXPOSE 8080
ENTRYPOINT ["tofu-api"]
CMD ["serve", "-config", "/etc/tofu-api/tofu-api.yaml"]
