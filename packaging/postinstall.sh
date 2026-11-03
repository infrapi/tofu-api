#!/bin/sh
# rpm: $1 is 1 on install, 2 or more on upgrade.
# deb: $1 is configure, $2 the version upgraded from, empty on install.
systemctl daemon-reload >/dev/null 2>&1 || :
case "$1" in
    1) ;;
    configure) [ -n "$2" ] && systemctl try-restart 'tofu-api@*.service' >/dev/null 2>&1 || : ;;
    *) systemctl try-restart 'tofu-api@*.service' >/dev/null 2>&1 || : ;;
esac
