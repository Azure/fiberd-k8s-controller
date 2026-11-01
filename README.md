# fiberd-k8s-controller

fiberd serves fast clones of one container template under signed capacity grants.
This controller lets a tenant ask for that capacity with a `CapacityGrant` object, and it runs the matching fiberd grant Pod.
A grant Pod is an ordinary Kubernetes Pod that runs fiberd for exactly one grant.

The project is under construction. The work is tracked in the issues of this repository.
