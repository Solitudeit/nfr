// Command nfrc is the NFR client: it runs inside the private network, keeps
// one outbound control connection per server and forwards tunnel traffic to
// the local services it is configured for.
package main

import (
	"os"

	"github.com/solitudeit/nfr/internal/cli"
)

func main() {
	cli.Program = "nfrc"
	cli.RunClient(os.Args[1:])
}
