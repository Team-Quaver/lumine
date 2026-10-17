// luminectl drives a running Lumine compositor over its control socket.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Team-Quaver/lumine/internal/ipc"
)

func socketPath() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = "/run/user/" + fmt.Sprint(os.Getuid())
	}
	return filepath.Join(dir, "lumine-ctl.sock")
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	arg := ""
	if len(os.Args) > 2 {
		arg = os.Args[2]
	}
	if cmd == "help" {
		usage()
		return
	}
	out, err := ipc.Call(socketPath(), cmd, arg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "luminectl:", err)
		os.Exit(1)
	}
	var pretty any
	if json.Unmarshal([]byte(out), &pretty) == nil {
		b, _ := json.MarshalIndent(pretty, "", "  ")
		fmt.Println(string(b))
	} else {
		fmt.Println(out)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: luminectl <command> [arg]

commands:
  focus left|right|up|down    move focus
  move left|right             reorder the focused column
  scroll <px>                 pan the column strip
  mode tile|stack|toggle      switch layout mode
  close                       close the focused window
  spawn <cmd...>              spawn a program
  reload                      force a config reload
  status                      dump layout state
  outputs                     list outputs
`)
}
