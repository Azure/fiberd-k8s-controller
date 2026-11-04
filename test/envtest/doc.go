// Package envtest runs the controller against a real kube-apiserver and
// etcd. Its tests build only with the envtest tag. make test-envtest
// fetches those binaries with setup-envtest, then runs the tests.
//
// There is no controller-manager, so no ReplicaSet, Pod or garbage
// collection exists. A test that needs Deployment status writes it by hand.
package envtest
