// Package policy checks the admission policies in config/policy against a
// real kube-apiserver. The API server alone admits or refuses, so no
// controller manager runs, and every grant is a dry-run create unless a case
// needs a stored one.
package policy
