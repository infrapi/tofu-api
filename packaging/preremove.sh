#!/bin/sh
# rpm: $1 is 0 on erase, 1 on upgrade. deb: $1 is remove on removal.
case "$1" in
    0 | remove)
        systemctl stop 'tofu-api@*.service' >/dev/null 2>&1 || :
        # What disabling each instance would do: systemctl disable takes no pattern.
        rm -f /etc/systemd/system/*.wants/tofu-api@*.service
        ;;
esac
