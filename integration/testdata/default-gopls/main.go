// default-gopls is a real executable fixture for the production default launcher.
// Only the fixture reads these synthetic input/evidence variables. The proxy
// receives no backend locator, argv override, or test-only execution switch.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/fredgnr/gopls-mcp-proxy/internal/testbackend"
)

var image = "initial"

func main() {
	if len(os.Args) != 2 || os.Args[1] != "mcp" {
		fmt.Fprintln(os.Stderr, "default fixture expects exactly [mcp]")
		os.Exit(2)
	}
	var inputs []string
	if err := json.Unmarshal([]byte(os.Getenv("PROXY_RAW_INPUTS")), &inputs); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	log, err := os.OpenFile(os.Getenv("PROXY_RAW_LOG"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	err = json.NewEncoder(log).Encode(map[string]any{"kind": "image", "pid": os.Getpid(), "image": image})
	_ = log.Close()
	if err == nil {
		err = testbackend.Run(os.Stdin, os.Stdout, testbackend.Config{Inputs: inputs, Log: os.Getenv("PROXY_RAW_LOG"), Args: os.Args[1:]})
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}
