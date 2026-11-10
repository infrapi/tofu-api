---
title: Operations
description: Inspecting the namespaces' provider processes and metrics, and how concurrent requests reach the provider.
weight: 100
---

# Operations

## What runs

```sh
curl -s localhost:8080/-/providers   # the namespaces' processes: state, started, last used, requests in flight, served, failed
curl -s localhost:8080/-/metrics | grep ^tofu_api
```

| Metric | |
|---|---|
| `tofu_api_providers` | processes running |
| `tofu_api_provider_starts_total{result}` | `ok`, or `failed` (an unknown namespace included) |
| `tofu_api_provider_stops_total{reason}` | `idle`, `max_age`, `evicted`, `gone`, `shutdown` |

The metrics have no namespace label, namespaces being unbounded: `/-/providers`
names them. A process stopping once its last call ends is still counted,
no longer listed.

## Concurrency

Every request goes to one provider process (the namespace's, with `namespaces`), over one gRPC connection;
the provider runs each call in its own goroutine, as under Terraform. At
most `parallelism` requests are at the provider at once (10, as Terraform's
`-parallelism`); the others wait, and one whose caller leaves while waiting
is a 503 that never reached the provider.

A create, update or delete is not cancelled when the caller leaves: a call
cut short may or may not have been made, and no one would know which. It
runs to its end, bounded by `timeout`. A create whose caller left is then
deleted, since no one got its id; reads are cancelled with their request.

Nothing orders two changes to the same resource: two `PUT`s at once both
apply, and the last one wins.
