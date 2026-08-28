// Command verge is the command line client for the Verge API image endpoints.
package main

import (
	"os"

	"github.com/img-verge/verge-cli/internal/app"
)

func main() {
	os.Exit(app.Run(os.Args[1:], os.Stdout, os.Stderr))
}
