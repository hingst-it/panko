# Pre-rendered manifests

For tools that apply plain manifests (an Argo CD directory source, a Flux Kustomization, or
`kubectl apply`), render the published chart once per version and commit the result:

```sh
mkdir -p rendered
helm show crds oci://ghcr.io/hingst-it/charts/panko --version 0.0.2 > rendered/crds.yaml
helm template panko oci://ghcr.io/hingst-it/charts/panko --version 0.0.2 \
  --namespace panko-system --values values.yaml > rendered/panko.yaml
# Without Helm, the panko CLI of the same release renders its embedded chart identically:
#   panko chart template --values values.yaml > rendered/panko.yaml
```

Apply the CRDs first and wait until they are established, then the rest:

```sh
kubectl apply --server-side -f rendered/crds.yaml
kubectl wait --for=condition=Established crd --all --timeout=60s
kubectl apply --server-side -f rendered/panko.yaml
```

Rendering is reproducible: the same chart and values produce the same bytes, and no Secret is
rendered. Plain manifests have no Helm release record, so `panko install` treats such a cluster
as an existing installation (use `--take-ownership` to switch it to a Helm release).
