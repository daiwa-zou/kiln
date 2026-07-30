# Plain Kubernetes manifests

Not every team deploys with Helm. The chart in [`../helm/kiln`](../helm/kiln)
is still the source of truth, but it renders to plain YAML you can commit to a
GitOps repository, feed to `kubectl apply`, or patch with Kustomize:

```bash
make manifests            # -> deploy/kubernetes/kiln.yaml
kubectl apply -f deploy/kubernetes/kiln.yaml
```

The rendered file is deliberately **not** committed here. A generated manifest
checked in beside its generator drifts the first time the chart changes and
nobody re-renders, and a stale manifest that still applies cleanly is worse
than no manifest at all — it deploys the wrong thing quietly. Render it into
your own environment repository, where it is reviewed as a diff on each bump.

## Before you apply

Create the secret the workloads reference (they mount keys optionally, so only
`master-key` is mandatory):

```bash
kubectl create namespace kiln
kubectl -n kiln create secret generic kiln-secrets \
  --from-literal=master-key="$(openssl rand -hex 32)" \
  --from-literal=anthropic-api-key="sk-ant-..." \
  --from-literal=database-url="postgres://kiln:...@postgres:5432/kiln?sslmode=require"
```

In production, prefer a Secret managed by External Secrets, Sealed Secrets, or
a cloud secret-manager CSI driver over `kubectl create secret`, and prefer
cloud identity (IRSA, Workload Identity) over static object-storage keys —
annotate the ServiceAccount via `serviceAccount.annotations` and leave
`storage.accessKey` / `storage.secretKey` unset.

## Customizing with Kustomize

Render once, then patch what your environment needs without forking the chart:

```yaml
# kustomization.yaml
resources:
  - kiln.yaml
patches:
  - target:
      kind: Deployment
      name: kiln-worker
    patch: |
      - op: replace
        path: /spec/replicas
        value: 8
```

## What gets created

| Object | Purpose |
| --- | --- |
| `Job/kiln-migrate` | `kiln admin migrate`, as a pre-install/pre-upgrade hook. Advisory-locked and idempotent. |
| `Deployment/kiln-api` | Stateless API and reading UI. `/readyz` gates traffic, `/healthz` gates restarts. |
| `Deployment/kiln-worker` | Build workers. No probes by design; a worker holds no listener, and killing one mid-build only wastes spend. |
| `Service/kiln` + `Ingress/kiln` | Ingress raises the body limit to match the 32 MiB upload cap. |
| `HorizontalPodAutoscaler` | Optional, per role. Workers scale down slowly so long builds are not evicted. |
| `PodDisruptionBudget/kiln-api` | Keeps the API available through node drains. |
| `ServiceAccount/kiln` | No RBAC and no mounted token — kiln never calls the Kubernetes API. Exists to carry cloud identity annotations. |

See [`docs/deployment.md`](../../docs/deployment.md) for configuration,
upgrades, backups, and operating notes.
