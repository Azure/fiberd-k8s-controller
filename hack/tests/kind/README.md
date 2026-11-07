# The kind end-to-end run

This is the harness behind `.github/workflows/conformance.yml`. It installs the controller through its Helm chart on one kind node, then checks a grant Pod the controller made.

## Running it locally

You need docker, git, go, helm, jq, kubectl, make and kind at the version in `versions.env`.

```sh
make conform-kind
```

This builds every image, so a first run is slow. The cluster is named `fiberd-e2e`. Its kubeconfig stays in `bin/e2e/kubeconfig`, so your current context never moves. To use it yourself, set `KUBECONFIG` to that file.

`make kind-down` deletes the cluster.

## Stages

Each stage is a subcommand of `conform.sh`, and `make conform-kind` runs them in order on a new cluster. Each also has its own target, such as `make kind-smoke`, which uses the cluster that is there.

| Stage | What it does |
| --- | --- |
| `up` | Creates the node, or keeps one that is Ready |
| `images` | Builds the images, loads them, and names each by digest in the node |
| `deps` | Installs cert-manager and its csi-driver |
| `deploy` | Installs the chart, issues the caller's certificate, and installs both grants. It fails on any restart before Ready |
| `smoke` | Fetches a grant from the caller route and clones with it on an image with no test hooks |
| `conform` | Runs fiberd's suite and requires C1 to C10 and C13 to report PASS |
| `dump` | Saves the evidence, and also runs when any stage fails |

The suite and the smoke test run on the host. They reach the Pods through NodePorts 30084 and 30085.

Both present the client certificate of the Certificate in `caller.yaml`, and each grant names its key. They verify the Pod's Service name, since the NodePort address is not on the Pod's certificate. The smoke test reaches the caller route through a port-forward to the controller's Service.

## Choosing the fiberd

```sh
make conform-kind FIBERD_REF=pin
```

`FIBERD_REF` takes `main`, a commit, a tag, or `pin` for the `go.mod` version. With none set, the run follows the `FIBERD_VERSION` variable in CI, then `FIBERD_DEFAULT_REF` in `versions.env`, then `go.mod`.

A choice other than the pin moves a private copy of `go.mod` in `bin/e2e`, so the tree stays as committed and the controller, the images and the suite all build from one fiberd.

To test a local fiberd tree, set `FIBERD_SRC` to its directory. Its images and suite come from that tree, and the controller keeps the pin.

When a run goes red with no related change, read the fiberd commit on the top line of the job summary, then run again with `pin`. Green at the pin and red at main means fiberd moved.

## After a failure

Look in `bin/e2e/artifacts`. CI uploads that directory for seven days. It holds the objects, events, logs, zygote logs and audit spools, the suite's output, and the fiberd under test. It never holds a Secret, a grant file or a private key. The caller's key and the signing key stay in `bin/e2e/private` until `make kind-down`.

The cluster stays up after a failure, so you can look around it.

## Two rules for a change here

- Nothing may exec into a grant Pod before its container has started, because a process that enters its cgroup while fiberd starts can cost a restart. Waits use the API, and hooks go through `fiberd_exec` in `lib.sh`.
- `deploy` treats Ready with any restart as a failure, never a retry. Its exec probes are the first proof that an exec lands in the right cgroup, and the hooks rest on that.
