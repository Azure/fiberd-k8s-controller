# fiberd-k8s-controller

![A tenant applies a CapacityGrant once. The controller reconciles it into one Deployment and one Service, Kubernetes schedules the grant Pod, and the Pod fetches its own signed grant from the controller with a bound token. fiberd in the Pod verifies the grant, warms the zygote and joins the Service. Callers then dial the Service over mutual TLS, Clone a fiber from the warm zygote, get its endpoint and fence, and connect to it directly.](./docs/images/controller-hero.svg)

fiberd serves fast clones of one container template under signed capacity grants.
This controller lets a tenant ask for that capacity with a `CapacityGrant` object, and it runs the matching fiberd grant Pod.
A grant Pod is an ordinary Kubernetes Pod that runs fiberd for exactly one grant.
The [design](./docs/design/controller.md) explains how the pieces fit.

## Install

The cluster needs [cert-manager](https://cert-manager.io/docs/installation/) and its [csi-driver](https://cert-manager.io/docs/usage/csi-driver/) installed first. The controller and every grant Pod take their certificates from them, so neither starts without them.

1. Apply the install with `kubectl apply -k config/default`. Every object in it is a built-in kind, a CRD or a cert-manager object, so one apply is enough.
2. Edit the images in `config/samples/fiberd_v1alpha1_grantpolicy.yaml`, then create the GrantPolicy named default with `kubectl apply -f config/samples/fiberd_v1alpha1_grantpolicy.yaml`.
3. Onboard each tenant namespace with the `config/tenant` component, as its [comment](./config/tenant/kustomization.yaml) shows.

The GrantPolicy comes before the first tenant because the admission policy fails closed. Until a GrantPolicy named default exists, every create and update of a CapacityGrant is refused.
On an upgrade from a version without the policy, create default right after the apply. Until it exists, existing grants accept no update, finalizer removal included.

A CapacityGrant's zygote is either a command in the image or an artifact fiberd pulls by digest from a registry. A cluster admin opts in to registries by listing repositories in the GrantPolicy's `registries`, and until then every registry grant is refused. Plain HTTP passes only under an entry that spells `http://`. fiberd pulls anonymously today, so only public repositories work, and runtime `hyperlight` cannot run a pulled zygote yet.

Either zygote may name a Secret in the grant's namespace whose `seal-key.json` key holds the tenant's seal key, which seals the grant's deltas and opens sealed artifacts. Each namespace keeps its own key, not one across the fleet. These fields replace the `-registry` and `-delta-seal-key` args, which a tenant can no longer set.

```yaml
spec:
  zygote:
    digest: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    registry: ghcr.io/team-a/zygotes
    sealKeySecretName: team-a-seal-key
```

A CapacityGrant's env names must match `^FIBERD_TENANT_[A-Z0-9_]+$`, and the admission policy refuses the rest. Only the Hyperlight helper reads tenant env. It never reaches the zygote or fibers. `envFrom` is refused because a source's key names are unknown at admission.

A CapacityGrant's args may name only the fiberd flags a tenant sets. [`pkg/workload/podtemplate/args.go`](./pkg/workload/podtemplate/args.go) lists them with the reason for each, and the admission policy refuses the rest.

A CapacityGrant may mount a volume only at or under `/data` for data volumes, `/config` for config files or `/secrets` for Secrets. The same holds for a device path and a termination message path. fiberd's images keep nothing in those directories, so a mount cannot replace anything fiberd runs or trusts.

The project is under construction. A grant Pod fetches and renews its own grant, but a caller cannot fetch one yet. The work is tracked in the issues of this repository.
