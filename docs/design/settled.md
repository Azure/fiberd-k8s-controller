# What is settled

These decisions are closed. State them as facts. A change to one changes this page in the same pull request. Anything not on this page is checked at source before anyone calls it agreed.

## The two projects

- fiberd core holds fiberd, its runtimes, the Kubernetes home that `fiberd -home kubernetes` runs, its grant fetcher and the generic issuer parts ([fiberd's design](https://github.com/Azure/fiberd/blob/main/docs/design/kubernetes.md)).
- This repository holds only Kubernetes code and builds one image, the controller. Grant Pods run fiberd's images.
- Both repositories stay cloud agnostic. No code, doc or test names a cloud's service, login, annotation or instance type.

## Grants

- One CapacityGrant makes one Deployment with one replica and one Service.
- The grant UID is the Pod UID, and the audience is the Pod name.
- Every spec change rolls the Pods ([what rolls a Pod](controller.md#what-rolls-a-pod)).
- Status holds `observedGeneration`, the `Ready` and `Progressing` conditions, and `resolvedImage`. Nothing per Pod goes in status.
- `spec.callerThumbprint` is the RFC 7638 thumbprint of the caller's public key. A grant has one caller identity.

## GrantPolicy

- `GrantPolicy` is one cluster-scoped object named `default`. It has no per-namespace setting beyond the `privilegedNamespaces` list, and there is no policy object per namespace.
- The API server reads it through the admission policy. The controller never reads it.
- Every admission list an operator keeps lives there. These are `images`, `helperImages`, `runtimes`, `privilegedNamespaces` and `registries`.
- This page records the object. It does not settle how runtime policy should work.

## Tenants

- The issuer sets a grant's tenant to its namespace.
- Operators run one namespace per tenant application. This is an operating rule, and the controller does not enforce it.

## What a CapacityGrant may set

- The fiberd flags that [`args.go`](../../pkg/workload/podtemplate/args.go) lists and no others.
- Env names under `FIBERD_TENANT_`.
- Mounts, device paths and termination message paths only at or under `/data`, `/config` and `/secrets`.
- No `workingDir`.
- No `privileged`.
- Images and helper images by digest, from repositories the `GrantPolicy` lists.
- A registry zygote only from a registry the `GrantPolicy` lists.
- A runtime from the set the `GrantPolicy` lists.

The admission policy and the builder both hold the flag, env and mount rules, and a test keeps them equal. A CRD rule and the builder refuse `workingDir` and `privileged`. The digest and list rules hold at admission alone. The [security table](controller.md#security) has the detail.

## Runtimes

- proc and runc need an Unconfined seccomp profile, because CRIU cannot dump from under a filter ([fiberd's design](https://github.com/Azure/fiberd/blob/main/docs/design/kubernetes.md)).
- Capabilities per runtime come from fiberd core's measured sets. The [security table](controller.md#security) says what each runtime runs with.
- A controller flag can name the hypervisor device's extended resource. When it does, the builder adds one to every hyperlight Pod and a tenant may not write that resource there.
- No code is trusted on any runtime.
- Egress is required.
