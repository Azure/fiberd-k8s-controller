// Package chart compares the rendered Helm chart with the build of config.
// Its tests build only with the chart tag. make test-chart runs them, and
// they need helm on the PATH but no cluster.
package chart
