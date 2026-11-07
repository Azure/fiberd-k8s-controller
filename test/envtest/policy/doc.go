// Package policy checks the admission policies in config/policy and the
// fiberd-ca policies in config/certs against a real kube-apiserver. The API
// server alone admits or refuses, so no controller manager runs, and every
// object is a dry-run create unless a case needs a stored one.
package policy
