# fiberd-k8s-controller

![A tenant applies a CapacityGrant once. The controller reconciles it into one Deployment and one Service, Kubernetes schedules the grant Pod, and the Pod fetches its own signed grant from the controller with a bound token. fiberd in the Pod verifies the grant, warms the zygote and joins the Service. Callers then dial the Service over mutual TLS, Clone a fiber from the warm zygote, get its endpoint and fence, and connect to it directly.](./docs/images/controller-hero.svg)

fiberd serves fast clones of one container template under signed capacity grants.
This controller lets a tenant ask for that capacity with a `CapacityGrant` object, and it runs the matching fiberd grant Pod.
A grant Pod is an ordinary Kubernetes Pod that runs fiberd for exactly one grant.
The [design](./docs/design/controller.md) explains how the pieces fit.

The project is under construction. The controller does not serve grants yet. The work is tracked in the issues of this repository.
