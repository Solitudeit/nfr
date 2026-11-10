// Command nfrs is the NFR control node: the public host that terminates the
// tunnels, serves the console and stores the configuration.
package main

import (
	"os"

	"github.com/solitudeit/nfr/internal/cli"
)

func main() {
	cli.Program = "nfrs"
	cli.RunServer(os.Args[1:])
}
