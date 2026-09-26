// echoargs prints its own command-line arguments back, one JSON string per
// line, so a test can verify a parent process's argument escaping/quoting
// round-trips exactly - standing in for a real interpreter (node.exe, etc.)
// that a Windows .cmd shim's %* ultimately forwards to.
package main

import (
	"encoding/json"
	"fmt"
	"os"
)

func main() {
	for _, a := range os.Args[1:] {
		b, _ := json.Marshal(a)
		fmt.Println(string(b))
	}
}
