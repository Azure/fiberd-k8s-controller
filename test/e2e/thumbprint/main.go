// Command thumbprint prints the RFC 7638 thumbprint of a PEM certificate's key,
// the value a CapacityGrant's callerThumbprint holds. The kind harness renders
// its grants with it.
package main

import (
	"fmt"
	"os"

	"github.com/Azure/fiberd/pkg/tlsconf"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: thumbprint <certificate.pem>")
		os.Exit(2)
	}
	thumbprint, err := tlsconf.ThumbprintFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(thumbprint)
}
