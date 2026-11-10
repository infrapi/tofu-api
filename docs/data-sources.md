---
title: Data sources
description: Expose provider data sources read only, with GET on their path and their arguments in the query.
weight: 70
---

# Data sources

`data_sources` exposes data sources, read only, with the same rules as
`resources`:

```yaml
data_sources:
  grafana_folders: {}              # GET …/namespaces/{namespace}/folders
  grafana_dashboards:
    attributes:
      folder_uids:                 # an argument, in the query: ?folder_uids=a&folder_uids=b
        maxLength: 40
```

- `GET` on the data source's path, the provider's prefix stripped as for a
  resource type: its arguments come in the query, the response is what it
  reads. A data source and a resource type of the same name share their
  path: `GET` reads the data source, `POST` creates.
- An argument is a query parameter when it is a string, a number, a bool,
  or a list or set of them (the parameter repeated). Another is set by a
  rule, `value` or `default`, or `hidden`: tofu-api does not start
  otherwise. A parameter that is no argument is a 400.
- It reads what the provider's configuration lets it see, the namespace's
  with `namespaces`. Nothing is kept.
- With `auth`, reading is the verb `list` on the data source (`folders`).
