# The fiberd Kubernetes controller

This page shows how the controller turns a CapacityGrant into running **fibers**.

In the diagrams, blue is a tenant-facing Kubernetes object, purple is the controller and what it writes, teal is fiberd and what runs inside the Pod, and amber is the warm zygote. A solid arrow creates or owns, a dashed arrow is a runtime call, and a dotted arrow selects. In the sequence diagram a dashed arrow is a reply. A caller asks fiberd for a fiber with its Clone call.

## What fiberd is and what the controller adds

fiberd makes many fast instances of one container. It starts the program once as a warm **zygote**, then forks or restores each fiber from it in milliseconds. It runs only under a signed **grant** that caps the fiber count and expires after its **lease**.

fiberd never schedules anything. A **home** adapts it to where it runs. The home delivers grants, reports whether their signer is reachable, and reports when its Pod is being deleted or its node is lost.

The controller adds the Kubernetes control plane. A CapacityGrant is the **tenant**'s custom resource asking for capacity. The controller turns it into a Deployment and a Service and reports status. Kubernetes alone schedules, replaces, rolls out and evicts the Pods.

The controller is the **issuer** too. It signs a grant for each grant Pod, a Pod of that Deployment, and publishes its public keys as the key set. One elected controller replica reconciles, while every replica issues grants and serves the discovery document and key set over HTTPS. The issuer URL is a ClusterIP Service in front of the replicas. A replica reports ready only once its caches have synced and its HTTPS listener serves with a loaded certificate.

## Two projects

fiberd and the controller live in two repositories. A piece that a Slurm or Docker consumer of fiberd would need unchanged belongs to fiberd core.

**fiberd core** holds fiberd, its **runtimes** proc, runc, gVisor and Hyperlight, every home and the generic pieces a home or an issuer needs. fiberd is one binary that is told which home to use. A grant Pod runs fiberd's released image with `-home kubernetes`.

**The controller** holds only Kubernetes code and builds its own image. It pins one fiberd version and checks its copies of fiberd rules, such as the zygote digest pattern, against core's exported values.

## The objects

| Object | Where | Who owns it |
| --- | --- | --- |
| CapacityGrant | Tenant namespace | The tenant, who creates it |
| Deployment | Tenant namespace | The controller, as the CapacityGrant's controller reference |
| Service | Tenant namespace | The controller, as the CapacityGrant's controller reference |
| ReplicaSet and Pod | Tenant namespace | Kubernetes' Deployment and ReplicaSet controllers |
| ServiceAccount `fiberd-grant` | Tenant namespace | The cluster admin, who [onboards the namespace](../../README.md#onboard-a-tenant-namespace) |
| `GrantPolicy` | Cluster scoped | The cluster admin, who creates the one named `default` that the admission policy reads |
| ClusterRole `fiberd-grant-author` | Cluster scoped | The cluster admin, bound to a tenant group when a namespace is onboarded |
| Signing key Secret | Controller namespace | The controller, which creates it on first start |

The controller creates one Deployment with one replica and one ClusterIP Service per CapacityGrant, in its namespace, both named `fiberd-<grant name>`. Both select the CapacityGrant's name and UID labels, so a CapacityGrant recreated under the same name never adopts stale Pods. A Deployment or Service of that name that the CapacityGrant does not control is left alone. The CapacityGrant reports it as a conflict.

**What the tenant owns.** The spec sets `zygote`, the program and its image digest, and `fibers`, the cap and the gVisor warm count. `writeBudget` is the memory fibers may write, and `callerThumbprint` binds the grant to one caller's certificate ([security](#security)). The rest is `runtime` and `podTemplate`.

In `podTemplate` the tenant owns every scheduling field and the one container named `fiberd`, with its image, args and resources. It also owns that container's security context, except privilege, capabilities, the seccomp and AppArmor profiles, and SELinux and Windows options. A device request, extended resource or resource claim goes in its resources.

**What the controller overlays.** It sets the home and its own flags last. It sets the Pod identity, probes, volumes, service account, fiberd labels, privilege fields and a readiness gate on the zygote-ready Pod condition. Requests are copied from limits for Guaranteed QoS, since fiberd carves fibers from the memory limit. A tenant field that collides with the overlay invalidates the CapacityGrant.

**Status** holds the observed generation and the `Ready` and `Progressing` conditions, whose reasons the [README](../../README.md#status) lists. A bad spec, a conflict and a refused write each block the pass, so a Deployment built from an earlier spec keeps serving. The last two also record a Warning Event. The controller watches neither RoleBindings nor objects it does not control, so it looks again at a blocked grant after a minute. Everything per Pod stays on the Pod.

## The life of a grant

1. The tenant applies a CapacityGrant, which the CRD rules and admission policy check ([security](#security)).
2. The leader maps the spec to a grant and checks it. A bad spec stops here ([what rolls a Pod](#what-rolls-a-pod)).
3. The controller server-side applies the Service and the Deployment.
4. Its ReplicaSet creates the Pod, and the scheduler places it.
5. The kubelet mounts a projected token, a read-only volume from cert-manager's csi-driver with the fiberd CA and a serving certificate, and the grants directory, then starts fiberd.
6. The home fetches the Pod's grant before fiberd admits anything ([next section](#how-a-grant-pod-gets-its-grant)).
7. fiberd fetches the issuer's key set, verifies the grant and warms the zygote.
8. The home sets the zygote-ready condition, so the Pod passes its readiness gate and joins the Service.
9. Once the replica is available, the controller sets `Ready=True`.
10. A caller asks for a fiber, which fiberd forks (proc, runc) or restores (gVisor, Hyperlight) from the warm zygote.

## How a grant Pod gets its grant

A Deployment's Pods are named only once they exist, so each Pod asks for its own grant.

![The kubelet mounts a projected token whose audience is the issuer URL. The home sends it to the controller over HTTPS. The controller runs a TokenReview, reads the Pod and walks its owners up to the CapacityGrant, signs a grant whose UID is the Pod UID and whose audience is the Pod name, and returns it with its expiry. The home writes it to the memory-backed directory and delivers it to fiberd, which fetches the key set, verifies the grant and warms the zygote. The home then sets the zygote-ready Pod condition. Every half lease the home renews, the controller re-signs the same claims with a new expiry, and the home delivers the renewed grant.](../images/controller-grant-fetch.svg)

**The token.** The Pod mounts a projected service-account token bound to the Pod, with the issuer URL as its only audience. fiberd hides its directory from fibers. The API server honours it briefly after the Pod is deleted, so the controller refuses a terminating Pod itself.

**The checks.** The TokenReview must confirm the `fiberd-grant` account of the Pod's namespace and a token bound to a Pod, and the Pod's name and UID must match.

**Refusals.** Each refusal names one reason a machine can read. A token that fails names only that it failed, and a missing Pod answers like a Pod of another UID, so a refusal never reveals whether another object exists. Limits per client address, across all clients and per Pod answer 429 with `Retry-After`. Each replica counts on its own, and the flags are `--grant-rate-per-ip`, `--grant-rate` and `--grant-pod-interval`.

**The lineage.** The walk from Pod to CapacityGrant compares owner UIDs. Labels are mutable, so they are only a consistency check.

**The grant.** Its UID is the Pod UID and its tenant the namespace. The lease is a controller setting, and the rest comes from the spec. A Pod UID is never reused, so a replacement Pod's fibers never collide with the old Pod's fiber identities.

**Superseded revisions.** In a rollout the old Pod keeps renewing its own grant, so a stuck rollout never expires a healthy Pod. Only the first grant for a Pod requires its Pod-template hash to match the current spec. Every replica answers while only the leader reconciles, so a replica checks a mismatch against the API server before it refuses.

**Renewal.** The home renews at half lease and backs off as the server asks. It stops once refused with a conflict, such as a terminating Pod or a deleting CapacityGrant. A restarted fiberd keeps the grant and serves while it retries.

**Controller outage.** In a full outage the current grant serves until its lease ends. fiberd tolerates a stale key set for one lease, so both lapse together. Then the grant yields, its fibers end and new requests are refused. A new Pod waits a minute for its first grant, then exits and the kubelet restarts it.

A tenant's caller will fetch one grant per Pod from the same listener, with the client certificate the CapacityGrant names. That route is not built yet.

## Security

The tenant owns the Pod. The image allowlist is what makes the fiberd binary trustworthy, and the trust roots decide which controller that binary believes. Every field that can change either is refused.

| Concern | How it is met |
| --- | --- |
| TLS | One fiberd CA comes from cert-manager. Its csi-driver issues each grant Pod a serving certificate. fiberd serves mutual TLS only and trusts that CA for clients. Callers dial the Service name and present a client certificate. A self-signed issuer signs that CA once for ten years, and its key does not rotate. The controller serves HTTPS only, with TLS 1.3 at least and no client certificate, since the discovery document and key set are public and a grant Pod proves itself with its token. Its certificate comes from the same CA, lives a day and is reloaded on renewal without a restart. fiberd trusts the controller through the CA file in its csi volume, and the public roots in its image stay trusted beside it. |
| Bound grants | `callerThumbprint` puts one caller's certificate thumbprint into the grant. fiberd refuses the grant over any other certificate, so a copied grant is worthless. Replicas of one caller share a certificate and so share the grant. |
| Least privilege per runtime | Every runtime drops all capabilities and adds back the set fiberd core measured for it. proc and runc also get a seccomp profile written for fiberd. AppArmor stays unconfined for now. A privileged container is allowed only in namespaces the `GrantPolicy` lists. |
| CRD rules | CEL rules on the CRD fix the Pod's shape. The template has one container named `fiberd` and no init or ephemeral containers. It sets no service account, node name or host namespaces. The rules also refuse what the overlay cannot reset. That is probes, lifecycle hooks, `envFrom`, Pod-level resources, host aliases, DNS config, the runtime class, and SELinux and Windows options. The `fiberd` container must set a memory limit. |
| Admission policy | A policy reads the `GrantPolicy` named `default` and fails closed, so every CapacityGrant write is refused while it is missing. Args may name only the fiberd flags a tenant sets. The image must carry a digest and come from a listed repository, and the runtime must be listed. The policy refuses reserved labels and annotations, volume sources outside its allowlist, and projected service account tokens. It refuses mounts over fiberd's directories, the trust roots and the resolver files, and env that changes fiberd's trust. A privileged container and the gvisor and hyperlight runtimes need a listed namespace. A second policy keeps `default` from being deleted. Only the author role may create CapacityGrants. |
| Pod Security | A grant Pod needs SYS_ADMIN and SYS_PTRACE and runs with Unconfined AppArmor and seccomp, which exceeds the `baseline` level, so a namespace that hosts grant Pods enforces `privileged` and the admission policy is the guard there. That label admits any privileged Pod, so tenants who may create Pods directly should not share a namespace with grant Pods. |
| Grants stay inside the Pod | No grant is an API object or a Secret. The home writes it into a memory-backed grants directory that fibers cannot see. Tenants need no `secrets` access, and `fiberd-grant` can read no Secret. The controller logs no token or grant, at any verbosity. |
| Signing keys | The keys live in one Secret in the controller's namespace. The key set publishes every key still needed, so grants signed before a rotation verify until they expire. |

## What rolls a Pod

**A spec change rolls.** It changes the Pod-template hash, so the Deployment rolls a new Pod. fiberd refuses a redelivered grant that changes anything but the lease or the caller binding. A new spec therefore means a new Pod and a new grant UID. A bad spec sets `Ready=False` with the reason `InvalidSpec` and leaves the serving Deployment alone.

**The strategy follows the device.** An ordinary CapacityGrant surges one Pod with none unavailable. The new Pod warms behind its readiness gate before the old one goes, so the Service keeps an endpoint. For up to one lease both serve, doubling the fiber cap. A CapacityGrant that requests a device uses `Recreate`, since a surge Pod would wait forever for the old Pod's device.

**CA rotation mostly does not roll.** fiberd reloads its serving certificate, key and client CA pool when the files change, so the TLS it serves needs no restart. It reads the issuer's discovery and key set through a root pool loaded once at start, from the csi volume's CA file and the image's public roots. A CA rotation reaches that trust only through a rollout, or a CA bundle that keeps the old root valid during the overlap. A caller whose own certificate rotates needs a new caller thumbprint, which rolls.

**Deletion drains.** A finalizer holds the CapacityGrant while the controller scales the Deployment to zero. Each Pod gets SIGTERM and its grace period, and if it has drain time, fiberd first **parks** its named sessions, saving each so a later request can resume it. Once no Pod is left, the finalizer discards the CapacityGrant's parked sessions and lets go, and garbage collection removes the rest. How it lists those sessions is still open. After a timeout it stops waiting for a Pod stuck on a dead node.
