// Command fiberd-k8s-controller runs the controller that gives every
// CapacityGrant a Service and a Deployment of fiberd Pods.
package main

import (
	"flag"
	"fmt"
	"os"

	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/Azure/fiberd-k8s-controller/cmd/fiberd-k8s-controller/app"
)

func main() {
	o := app.NewOptions()
	klog.InitFlags(nil)
	o.AddFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(klog.NewKlogr())

	err := run(o, flag.Args())
	if err != nil {
		klog.ErrorS(err, "fiberd-k8s-controller stopped")
	}
	klog.Flush()
	if err != nil {
		os.Exit(1)
	}
}

// run validates o, builds the manager and runs it until SIGTERM or SIGINT.
func run(o *app.Options, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("unexpected arguments %q", args)
	}
	if err := o.Validate(); err != nil {
		return fmt.Errorf("invalid flags: %w", err)
	}
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("load the client configuration: %w", err)
	}
	mgr, err := app.NewManager(cfg, o)
	if err != nil {
		return err
	}
	return mgr.Start(ctrl.SetupSignalHandler())
}
