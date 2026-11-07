# fiberd-k8s-controller

![A tenant applies a CapacityGrant once. The controller reconciles it into one Deployment and one Service, Kubernetes schedules the grant Pod, and the Pod fetches its own signed grant from the controller with a bound token. fiberd in the Pod verifies the grant, warms the zygote and joins the Service. Callers then dial each grant Pod's address over mutual TLS with that Pod's grant, verify the Service name its certificate carries, Clone a fiber from the warm zygote, get its endpoint and fence, and connect to it directly.](./docs/images/controller-hero.svg)

fiberd serves fast clones of one container template under signed capacity grants.
This controller lets a tenant ask for that capacity with a `CapacityGrant` object, and it runs the matching fiberd grant Pod.
A grant Pod is an ordinary Kubernetes Pod that runs fiberd for exactly one grant.
The [design](./docs/design/controller.md) explains how the pieces fit.
[What is settled](./docs/design/settled.md) lists the decisions that are closed.

## Install

The cluster needs [cert-manager](https://cert-manager.io/docs/installation/) and its [csi-driver](https://cert-manager.io/docs/usage/csi-driver/) installed first. The controller and every grant Pod take their certificates from them, so neither starts without them.

1. Apply the install with `kubectl apply -k config/default`. Every object in it is a built-in kind, a CRD or a cert-manager object, so one apply is enough.
2. Edit the images in `config/samples/fiberd_v1alpha1_grantpolicy.yaml`, then create the GrantPolicy named default with `kubectl apply -f config/samples/fiberd_v1alpha1_grantpolicy.yaml`.
3. Onboard each tenant namespace with the `config/tenant` component, as its [comment](./config/tenant/kustomization.yaml) shows. [Run one namespace per tenant application](./docs/design/settled.md#tenants).

The GrantPolicy comes before the first tenant because the admission policy fails closed. Until a GrantPolicy named default exists, every create and update of a CapacityGrant is refused.
On an upgrade from a version without the policy, create default right after the apply. Until it exists, existing grants accept no update, finalizer removal included.

Grant Pods serve mutual TLS only, and every CapacityGrant must name its caller's key in `callerThumbprint`. On an upgrade from a version that served plaintext, set it on every CapacityGrant before the apply. A `callerThumbprint` written for a fiberd from before Azure/fiberd#105 bound grants to the caller's public key is the x5t#S256 hash of the caller's certificate. Replace it with the RFC 7638 thumbprint of the certificate's public key. A grant without it reports `InvalidSpec`, accepts no spec change that leaves it unset, and its old plaintext Pods are refused their next renewal, so they stop within a lease. Its metadata can still be updated, finalizers included.

Admission policies hold ClusterIssuer `fiberd-ca` to two kinds of leaf, as the [design](./docs/design/controller.md#security) says: a serving certificate for a grant Pod or the controller, and a client certificate for a caller. A caller's Certificate sets `usages: [client auth]`, no `dnsNames` and no `isCA`, and lives 2160 hours at most, which cert-manager's default of 90 days meets. Only the ServiceAccounts `cert-manager` and `cert-manager-csi-driver` in namespace `cert-manager` may create a CertificateRequest for `fiberd-ca`. Where they run under other names, install with the Helm chart and set `certManager` to them. The policies check a Certificate only when it is created or its spec changes, and cert-manager renews a stored Certificate from the CSR it builds, which no policy reads. So on an upgrade from a version without them, list the Certificates whose `issuerRef.name` is `fiberd-ca` before the apply and recreate any that break the rules. A metadata update does not check them again. The policies check a Pod only when it is created, so also delete any running Pod whose csi volume names `fiberd-ca` and breaks the rules, since its volume is not checked again and keeps renewing. Grant Pods roll on the upgrade anyway.

The upgrade rolls every grant Pod. A grant Pod no longer gets a CPU limit its template does not set, and its CPU request is no longer raised to its limit. Unless the tenant sets a CPU limit equal to its request, the Pod is Burstable instead of Guaranteed. A namespace whose ResourceQuota covers `limits.cpu`, or whose LimitRange has any CPU rule that needs a limit (a default, a max or a maxLimitRequestRatio), needs a CPU limit in the grant's template, or its Pod is refused or limited. The CapacityGrant is still admitted, and the refusal shows only as ReplicaSet events, so on an upgrade the rolled Pods in such a namespace do not come back until the template sets one.

The controller signs the caller binding as the `cnf` `jkt` of [Azure/fiberd#105](https://github.com/Azure/fiberd/issues/105), and a grant Pod refuses a grant for another Pod's UID only since [Azure/fiberd#107](https://github.com/Azure/fiberd/pull/107). A Pod serves a caller's grant only while its own grant holds since [Azure/fiberd#121](https://github.com/Azure/fiberd/pull/121). A grant Pod's image therefore needs fiberd at [aa8e259](https://github.com/Azure/fiberd/commit/aa8e259573e082a240fadfcbebca639c27fa4092) or later, like the version go.mod pins. No fiberd release holds it yet.

A CapacityGrant's zygote is either a command in the image or an artifact fiberd pulls by digest from a registry. A cluster admin opts in to registries by listing repositories in the GrantPolicy's `registries`, and until then every registry grant is refused. Plain HTTP passes only under an entry that spells `http://`. fiberd pulls anonymously today, so only public repositories work. On runtime `hyperlight` the artifact is a sealed Hyperlight snapshot that the helper restores, so the grant needs its seal key, and one without `sealKeySecretName` is refused at admission. Such a grant needs fiberd with [Azure/fiberd#101](https://github.com/Azure/fiberd/pull/101), which carries the snapshot as an artifact's images, and a helper whose `--version` advertises `template-dir`. No fiberd release holds it yet.

Either zygote may name a Secret in the grant's namespace whose `seal-key.json` key holds the tenant's seal key, which seals the grant's deltas and opens sealed artifacts. Each namespace keeps its own key. These fields replace the `-registry` and `-delta-seal-key` args, which a tenant can no longer set.

```yaml
spec:
  zygote:
    digest: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    registry: ghcr.io/team-a/zygotes
    sealKeySecretName: team-a-seal-key
```

What a CapacityGrant may set is [settled](./docs/design/settled.md#what-a-capacitygrant-may-set), and the admission policy refuses the rest.

A tenant's caller fetches the grants of a CapacityGrant's Pods with a client certificate from `fiberd-ca` whose key the CapacityGrant names, as the [design](./docs/design/controller.md#how-a-caller-gets-its-grants) describes.

The project is under construction. The work is tracked in the issues of this repository.
