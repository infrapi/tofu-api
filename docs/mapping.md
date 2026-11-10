---
title: Mapping
description: How provider schema attributes map to the request and response schemas, and how resources are rebuilt from their id.
weight: 90
---

# Mapping

| provider schema | request (`<Type>Spec`) | response (`state`) |
|---|---|---|
| required | `required` | yes |
| optional, optional+computed | optional | yes |
| computed only, `id` | absent: sending one is a 400 | yes |
| sensitive | `format: password` | yes, `format: password`: the caller may need it, such as a token the provider creates |
| write-only | `writeOnly`, `format: password` | absent: the provider never reports it |
| deprecated | `deprecated: true` | `deprecated: true` |
| nested block list/set | array, `minItems`/`maxItems`, `uniqueItems` for set | |
| nested block map | object, `additionalProperties` | |
| `set(T)` / `map(T)` / `object` / `dynamic` | array unique / object / closed object / any | |

## No state in the service

A resource is returned flat, as the provider reports it, `id` included,
sensitive values included, without write-only or hidden attributes (`hidden`
keeps a sensitive one from the caller). The service keeps nothing, and logs
no request or response body; the caller keeps the id.
`GET`, `PUT` and `DELETE` on `/{id}` rebuild the state from the id by
importing the resource from the provider, as `terraform import` does, then check that
every attribute a `value` rule fixes has the namespace's value: an id from another
namespace is a 404. There is no list, since nothing is kept to list.
