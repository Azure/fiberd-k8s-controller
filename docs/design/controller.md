# The fiberd Kubernetes controller

This page shows how the controller turns a CapacityGrant into running **fibers**.

In the diagrams, blue is a tenant-facing Kubernetes object, purple is the controller and what it writes, teal is fiberd and what runs inside the Pod, and amber is the warm zygote. A solid arrow creates or owns, a dashed arrow is a runtime call, and a dotted arrow selects. In the sequence diagram a dashed arrow is a reply. A caller asks fiberd for a fiber with its Clone call.

[What is settled](settled.md) lists the decisions this page explains.

## What fiberd is and what the controller adds

fiberd makes many fast instances of one container. It starts the program once as a warm **zygote**, then forks or restores each fiber from it in milliseconds. It runs only under a signed **grant** that caps the fiber count and expires after its **lease**.

fiberd never schedules anything. A **home** adapts it to where it runs. The home delivers grants, reports whether their signer is reachable, and reports when its Pod is being deleted or its node is lost.

The controller adds the Kubernetes control plane. A CapacityGrant is the **tenant**'s custom resource asking for capacity. The controller turns it into a Deployment and a Service and reports status. Kubernetes alone schedules, replaces, rolls out and evicts the Pods.

The controller is the **issuer** too. It signs a grant for each grant Pod, a Pod of that Deployment, and publishes its public keys as the key set. One elected controller replica reconciles, while every replica issues grants and serves the discovery document and key set over HTTPS. The issuer URL is a ClusterIP Service in front of the replicas. A replica reports ready only once its caches have synced and its HTTPS listener serves with a certificate that has not expired. Both replicas share one certificate, so if renewal stops they go unready together when it expires. Neither restarts, the leader keeps reconciling, and a renewed certificate brings them back.

## Two projects

fiberd and the controller live in two repositories. A piece that a Slurm or Docker consumer of fiberd would need unchanged belongs to fiberd core.

**fiberd core** holds fiberd, its **runtimes** proc, runc, gVisor and Hyperlight, every home and the generic pieces a home or an issuer needs. fiberd is one binary that is told which home to use. A grant Pod runs fiberd's image with `-home kubernetes`.

**The controller** holds only Kubernetes code and builds its own image. It pins one fiberd version and checks its copies of fiberd rules, such as the zygote digest pattern, against core's exported values.

## The objects

| Object | Where | Who owns it |
| --- | --- | --- |
| CapacityGrant | Tenant namespace | The tenant, who creates it |
| Deployment | Tenant namespace | The controller, as the CapacityGrant's controller reference |
| Service | Tenant namespace | The controller, as the CapacityGrant's controller reference |
| ReplicaSet and Pod | Tenant namespace | Kubernetes' Deployment and ReplicaSet controllers |
| ServiceAccount `fiberd-grant` | Tenant namespace | The cluster admin, who [onboards the namespace](../../README.md#install) |
| `GrantPolicy` | Cluster scoped | The cluster admin, who creates the one named `default` that the admission policy reads |
| ClusterRole `fiberd-grant-author` | Cluster scoped | The cluster admin, bound to a tenant group when a namespace is onboarded |
| Signing key Secret | Controller namespace | The controller, which creates it on first start |

The controller creates one Deployment with one replica and one ClusterIP Service per CapacityGrant, in its namespace, both named `fiberd-<grant name>`. Both select the CapacityGrant's name and UID labels, so a CapacityGrant recreated under the same name never adopts stale Pods. A Deployment or Service of that name that the CapacityGrant does not control is left alone. The CapacityGrant reports it as a conflict.

Each replica caches only the Deployments and Services that carry the controller's managed-by label, so its memory and watch traffic follow the number of grants and not the size of the cluster. Before it applies, the controller reads an object its cache does not hold from the API server. A foreign object is therefore still reported, at the cost of one read per kind for a new CapacityGrant. An owned object that lost the label is treated as owned, and the next apply restores the label.

**What the spec sets.** The spec sets `zygote`, the zygote's digest and where it comes from, and `fibers`, the cap and the gVisor warm count. The zygote is either a `command` baked into the image, or a `registry` that fiberd pulls the artifact from by digest. fiberd pulls anonymously today, so only public repositories work, and a registry zygote cannot run on `hyperlight` yet. Either may name `sealKeySecretName`, a Secret in the grant's namespace whose `seal-key.json` holds the tenant's seal key, which seals the grant's deltas and opens sealed artifacts. Each namespace keeps its own key. `writeBudget` is the memory fibers may write, and `callerThumbprint` binds the grant to one caller's key ([security](#security)). The rest is `runtime`, `helperImage` and `podTemplate`.

In `podTemplate` the tenant owns every scheduling field and the one container named `fiberd`, with its image, args and resources. It also owns that container's security context, except privilege, capabilities, the seccomp and AppArmor profiles, and SELinux and Windows options. A device request, extended resource or resource claim goes in its resources.

**What the controller overlays.** It sets the home and its own flags last, `-template` or `-registry` for the zygote among them. It sets the Pod identity, probes, volumes, the seal key Secret mounted read-only and hidden from fibers, service account, fiberd labels, privilege fields and a readiness gate on the zygote-ready Pod condition. It turns service links off. The memory request is pinned to the memory limit, since fiberd carves fibers from it, and so are the requests of extended resources and hugepages. CPU gets the tenant's request, the tenant's limit when only that is set, or `--default-pod-cpu` when the template sets no CPU, and no limit unless the tenant sets one, since a CPU limit throttles the CPU-bound bursts of fiber restores. Unless the tenant sets a CPU limit equal to its request, the Pod is Burstable, but its memory use cannot exceed its request, so it stays near the bottom of the kubelet's eviction order. A namespace whose ResourceQuota covers `limits.cpu`, or whose LimitRange has any CPU rule that needs a limit (a default, a max or a maxLimitRequestRatio), needs a CPU limit in the grant's template, or its Pod is refused or limited, and the refusal shows only as ReplicaSet events. A tenant field that collides with the overlay invalidates the CapacityGrant.

**The Hyperlight helper.** fiberd runs a hyperlight grant's fibers through a helper executable, and tenants may not add init containers, so the controller installs the helper from `helperImage`. Runtime `hyperlight` requires it and every other runtime refuses it. The image holds an executable `/fiberd-helper/helper` and optionally a guest at `/fiberd-helper/guest`. A non-root init container with a read-only root filesystem, no capabilities and the runtime's default seccomp profile runs the image and copies `/fiberd-helper` into an emptyDir. fiberd mounts that emptyDir read-only at `/var/run/fiberd/helper` and starts with `-hyperlight-helper=/var/run/fiberd/helper/helper`, so fiberd does not write it. A guest is passed in the `fiberd` container's args, such as `-hyperlight-guest=/var/run/fiberd/helper/guest`. [`hack/tests/kind/fakehelper.Dockerfile`](../../hack/tests/kind/fakehelper.Dockerfile) builds a helper image that needs no hypervisor.

The helper runs in the fiberd container and not on its own image's base, so the image's base is otherwise free. The image meets four rules.

1. `cp` is on the image's `PATH`.
2. Everything under `/fiberd-helper` is readable by other users, since the copy runs as uid 65532.
3. The helper is static, or linked against glibc 2.36 or older with only libc, libm and libgcc_s.
4. The helper answers `helper --version` and speaks the protocol in fiberd's [`hack/hyperlight/PROTOCOL.md`](https://github.com/Azure/fiberd/blob/main/hack/hyperlight/PROTOCOL.md).

Of what a grant writes, the helper sees only names under `FIBERD_TENANT_`. It also sees the variables the controller and the kubelet add, the image's env and what a webhook adds. The `fiberd-helper` init container receives no tenant env.

**Status** holds the observed generation and the `Ready` and `Progressing` conditions, whose reasons [`constants.go`](../../api/v1alpha1/constants.go) lists. A bad spec, a conflict and a refused write each block the pass, so a Deployment built from an earlier spec keeps serving. The last two also record a Warning Event. The controller watches neither RoleBindings nor objects it does not control, so it looks again at a blocked grant after a minute. Everything per Pod stays on the Pod.

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

**Renewal.** The home renews at half lease and backs off as the server asks. It stops once refused with a conflict, such as a terminating Pod, a deleting CapacityGrant or a previous grant bound to no caller, which only a Pod from before mutual TLS holds. A restarted fiberd keeps the grant and serves while it retries.

**Controller outage.** In a full outage the current grant serves until its lease ends. fiberd tolerates a stale key set for one lease, so both lapse together. Then the grant yields, its fibers end and new requests are refused. A new Pod waits a minute for its first grant, then exits and the kubelet restarts it.

## How a caller gets its grants

A caller presents a grant on every Clone, and each Pod accepts only a grant whose audience is its own name, so a caller holds one grant per Pod.

**The certificate.** The caller presents a client certificate from ClusterIssuer `fiberd-ca`, and the CapacityGrant's `callerThumbprint` names its key. A cert-manager Certificate with `usages: [client auth]` and `privateKey.rotationPolicy: Never` keeps the key across renewals and lets the caller's replicas share it. The csi-driver serves a single caller Pod, with `csi.cert-manager.io/key-usages: client auth` and `csi.cert-manager.io/reuse-private-key: "true"`, but it makes a new key for every Pod.

**The request.** The caller posts `{"namespace": "<ns>", "name": "<grant>"}` to `/v1/caller-grants` on the issuer URL, over that certificate. An `audience` field naming one Pod asks for that Pod's grant only. The listener asks every client for a certificate and requires none, so a grant Pod's route, discovery and the key set serve without one. The caller route verifies the certificate against the fiberd CA itself. fiberd-ca signs leaves directly, so the route checks the leaf against the CA alone and ignores any intermediate the client sends.

**The answer.** It holds `serverName`, `fiberd-<grant>.<ns>.svc`, and `grants`, one per running Pod of the CapacityGrant, newest first and at most eight. Each grant carries `token`, `uid`, `audience`, `issuedAt`, `expiresAt` and `address`, the Pod's IP and fiberd port. The caller dials that address with the Pod's grant and verifies `serverName`, which every Pod's certificate carries. A Pod of the superseded revision is included until it terminates, and a terminating Pod is not. Each grant is minted from the current spec, and fiberd keeps each Pod on the grant it was built from, so a caller keeps working across a rollout. A `callerThumbprint` change is the exception, since it moves callers only to new Pods. An old Pod refuses a grant for the new key until the rollout replaces it, and the old key gets no grant. A CapacityGrant with no running Pod returns no grant. The caller asks again before the earliest `expiresAt` and when a Pod refuses its grant.

**Refusals.** A certificate the CapacityGrant does not name and a CapacityGrant that does not exist are refused alike, 403 with the reason `Forbidden`. No certificate, one from another CA and one whose key has no thumbprint get 401. A deleting CapacityGrant gets 409 and an invalid spec 422. The limits per client address and across all clients run before the certificate is checked. A limit per caller key, CapacityGrant and client address, on `--grant-pod-interval` as a Pod's is, runs once the CapacityGrant names the caller. One key named by many grants is limited per grant, and replicas that share the key each have their own budget, since each asks from its own Pod address. The route reads only the controller's cache, so a missing CapacityGrant costs no API call and answers as fast as one that names another key.

## Security

The tenant owns the Pod. The image allowlist is what makes the fiberd binary trustworthy, and the trust roots decide which controller that binary believes. Every field that can change either is refused.

| Concern | How it is met |
| --- | --- |
| TLS | One fiberd CA comes from cert-manager. Its csi-driver issues each grant Pod a serving certificate. fiberd serves mutual TLS only and trusts that CA for clients. Callers dial the Service name and present a client certificate. A self-signed issuer signs that CA once for ten years, and its key does not rotate. The controller serves HTTPS only, with TLS 1.3 at least and no client certificate, since the discovery document and key set are public and a grant Pod proves itself with its token. Its certificate comes from the same CA, lives a day and is reloaded on renewal without a restart. fiberd trusts the controller through the CA file in its csi volume, and the public roots in its image stay trusted beside it. |
| Bound grants | `callerThumbprint` puts the RFC 7638 thumbprint of one caller's public key into the grant. fiberd refuses the grant over any other key, so a copied grant is worthless. Replicas of one caller share a key and so share the grant. |
| Zygote registries | A registry zygote is code no policy sees, so its repository must sit under an entry in the `GrantPolicy`'s `registries`, and while that list is unset no registry grant is admitted. Plain HTTP passes only under an entry that spells `http://`. The rule holds at admission, as the `GrantPolicy`'s other lists do. |
| Least privilege per runtime | Every runtime drops all capabilities and adds back the set fiberd core measured for it. Hyperlight also holds SYS_ADMIN and SETPCAP, which fiberd uses before it narrows. A grant that sets a device engine adds fiberd's engine set. The seccomp and AppArmor profiles are Unconfined, because CRIU cannot dump from under a filter and the home remounts cgroupfs and sysctls. Nothing runs privileged. |
| CRD rules | CEL rules on the CRD fix the Pod's shape. The template has one container named `fiberd` and no init or ephemeral containers. It sets no service account, node name or host namespaces. `helperImage` is set exactly when the runtime is `hyperlight`. The rules also refuse what the overlay cannot reset. That is probes, lifecycle hooks, `workingDir`, `envFrom`, Pod-level resources, host aliases, DNS config, the runtime class, and SELinux and Windows options. They refuse `privileged` too, since nothing runs privileged. The `fiberd` container must set a memory limit. |
| Admission policy | A policy reads the `GrantPolicy` named `default` and fails closed, so every CapacityGrant write is refused while it is missing. Args may name only the fiberd flags a tenant sets. The image must carry a digest and come from a repository in `images`. The helper image of a hyperlight grant must carry a digest too and come from a repository in `helperImages`, a list of its own because the helper runs with far less than the fiberd container holds. The runtime must be listed. The policy refuses reserved labels and annotations, volume sources outside its allowlist, projected service account tokens, and env names outside the `FIBERD_TENANT_` root. The gvisor and hyperlight runtimes need a listed namespace. A second policy keeps `default` from being deleted. Only the author role may create CapacityGrants. |
| Tenant mounts | A volume mount, device path or termination message path must sit at or under `/data` for data volumes, `/config` for config files or `/secrets` for Secrets. fiberd's images ship nothing there and the controller mounts nothing there, so a mount cannot replace a file fiberd runs or trusts. The policy and the controller judge the path as written and refuse one that is not already clean, so every spelling means one place. |
| Tenant env | A tenant env name must match `^FIBERD_TENANT_[A-Z0-9_]+$`, in the admission policy and in the controller. fiberd core owns that prefix, so one rule covers every name a tenant writes. Values stay free, so `valueFrom` needs no rule. `envFrom` is refused because a source's key names are unknown at admission. The overlay turns service links off, so Service names pick no env names. Only the Hyperlight helper reads tenant env. It never reaches the zygote or fibers, so on every other runtime a tenant variable does nothing. Variables a webhook adds never pass through the grant, so the rule does not touch them. |
| Pod Security | A grant Pod needs SYS_ADMIN and SYS_PTRACE and runs with Unconfined AppArmor and seccomp, which exceeds the `baseline` level, so a namespace that hosts grant Pods enforces `privileged` and the admission policy is the guard there. That label admits any privileged Pod, so tenants who may create Pods directly should not share a namespace with grant Pods. |
| Grants stay inside the Pod | No grant is an API object or a Secret. The home writes it into a memory-backed grants directory that fibers cannot see. Tenants need no `secrets` access, and `fiberd-grant` can read no Secret. The controller logs no token or grant, at any verbosity. |
| Signing keys | The keys live in one Secret in the controller's namespace. The key set publishes every key still needed, so grants signed before a rotation verify until they expire. |

## What rolls a Pod

**A spec change rolls.** It changes the Pod-template hash, so the Deployment rolls a new Pod. fiberd refuses a redelivered grant that changes anything but the lease or the caller binding. A new spec therefore means a new Pod and a new grant UID. A bad spec sets `Ready=False` with the reason `InvalidSpec` and leaves the serving Deployment alone.

**The strategy follows the device.** An ordinary CapacityGrant surges one Pod with none unavailable. The new Pod warms behind its readiness gate before the old one goes, so the Service keeps an endpoint. For up to one lease both serve, doubling the fiber cap. A CapacityGrant that requests a device uses `Recreate`, since a surge Pod would wait forever for the old Pod's device.

**CA rotation mostly does not roll.** fiberd reloads its serving certificate, key and client CA pool when the files change, so the TLS it serves needs no restart. It reads the issuer's discovery and key set through a root pool loaded once at start, from the csi volume's CA file and the image's public roots. A CA rotation reaches that trust only through a rollout, or a CA bundle that keeps the old root valid during the overlap. A caller certificate renewed over the same key keeps its thumbprint. A new key needs a new caller thumbprint, which rolls.

**Deletion drains, in the target design.** This is not built yet. Today the controller adds no finalizer, and garbage collection removes a deleted CapacityGrant's Deployment and Service. In the target design a finalizer holds the CapacityGrant while the controller scales the Deployment to zero. Each Pod gets SIGTERM and its grace period, and if it has drain time, fiberd first **parks** its named sessions, saving each so a later request can resume it. Once no Pod is left, the finalizer discards the CapacityGrant's parked sessions and lets go, and garbage collection removes the rest. How it lists those sessions is still open. After a timeout it stops waiting for a Pod stuck on a dead node.
