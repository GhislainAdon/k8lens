//go:build !windows

package main

import (
	"errors"
	"net"
	"os"
)

// dialAgentConn se connecte à l'agent SSH via SSH_AUTH_SOCK.
func dialAgentConn() (net.Conn, error) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, errors.New("SSH_AUTH_SOCK non défini")
	}
	return net.Dial("unix", sock)
}
