// Package ipc exposes the luminectl control socket. Line-based JSON:
// client sends {"cmd":"focus","arg":"left"}, server answers one JSON line.
package ipc

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
)

// Handler processes one command; reply is the JSON response payload.
type Handler func(cmd, arg string) any

// Server listens on $XDG_RUNTIME_DIR/lumine-<name>.sock.
type Server struct {
	ln      net.Listener
	Handler Handler

	mu   sync.Mutex
	path string
}

// Listen creates the control socket.
func Listen(runtimeDir, name string) (*Server, error) {
	path := filepath.Join(runtimeDir, name+"-ctl.sock")
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	return &Server{ln: ln, path: path}, nil
}

// Path returns the socket path (for tests and diagnostics).
func (s *Server) Path() string { return s.path }

// Serve accepts control clients until the listener closes.
func (s *Server) Serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(c)
	}
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 4096), 1<<20)
	for sc.Scan() {
		var req struct {
			Cmd string `json:"cmd"`
			Arg string `json:"arg"`
		}
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			reply(c, map[string]any{"error": "bad request"})
			continue
		}
		var resp any = map[string]any{"error": "no handler"}
		if s.Handler != nil {
			resp = s.Handler(req.Cmd, req.Arg)
		}
		reply(c, resp)
	}
}

func reply(c net.Conn, v any) {
	b, _ := json.Marshal(v)
	c.Write(append(b, '\n'))
}

// Call sends one command to the control socket and returns the reply.
func Call(path, cmd, arg string) (string, error) {
	c, err := net.Dial("unix", path)
	if err != nil {
		return "", err
	}
	defer c.Close()
	req, _ := json.Marshal(map[string]string{"cmd": cmd, "arg": arg})
	if _, err := c.Write(append(req, '\n')); err != nil {
		return "", err
	}
	sc := bufio.NewScanner(c)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return "", err
		}
		return "", nil
	}
	return sc.Text(), nil
}
