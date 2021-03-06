## tk reconcile source git

Reconcile a GitRepository source

### Synopsis

The reconcile source command triggers a reconciliation of a GitRepository resource and waits for it to finish.

```
tk reconcile source git [name] [flags]
```

### Examples

```
  # Trigger a git pull for an existing source
  tk reconcile source git podinfo

```

### Options

```
  -h, --help   help for git
```

### Options inherited from parent commands

```
      --kubeconfig string   path to the kubeconfig file (default "~/.kube/config")
      --namespace string    the namespace scope for this operation (default "gitops-system")
      --timeout duration    timeout for this operation (default 5m0s)
      --verbose             print generated objects
```

### SEE ALSO

* [tk reconcile source](tk_reconcile_source.md)	 - Reconcile sources

