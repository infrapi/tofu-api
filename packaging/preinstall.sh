#!/bin/sh
getent group tofu-api >/dev/null || groupadd -r tofu-api
getent passwd tofu-api >/dev/null ||
    useradd -r -g tofu-api -d /etc/tofu-api -s /sbin/nologin -c "tofu-api" tofu-api
exit 0
