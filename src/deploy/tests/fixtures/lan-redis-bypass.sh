#!/bin/sh
# LAN Redis exception: only the confirmed LAN source and destination.
set -eu
set -- -i br-lan -s 192.168.10.0/24 -d 192.168.10.1/32 -p tcp \
    --dport 6379 -m comment --comment relay-lan-redis-bypass -j RETURN
if iptables -w 5 -t mangle -C PREROUTING "$@" 2>/dev/null; then
    printf '%s\n' 'Redis exception already exists'
else
    if iptables -w 5 -t mangle -I PREROUTING 1 "$@"; then
        printf '%s\n' 'Redis exception added'
    else
        printf '%s\n' 'Redis exception failed' >&2
        exit 1
    fi
fi

# Application management and database ports; Redis remains independent.
set -- -i br-lan -s 192.168.10.0/24 -d 192.168.10.1/32 -p tcp \
    -m multiport --dports 18080,13306 -m comment --comment relay-lan-app-bypass -j RETURN
if iptables -w 5 -t mangle -C PREROUTING "$@" 2>/dev/null; then
    printf '%s\n' 'Application exception already exists'
else
    if iptables -w 5 -t mangle -I PREROUTING 1 "$@"; then
        printf '%s\n' 'Application exception added'
    else
        printf '%s\n' 'Application exception failed' >&2
        exit 1
    fi
fi
