//go:build windows

package main

import (
	"net"

	"github.com/Microsoft/go-winio"
)

// dialAgentConn se connecte à l'agent OpenSSH de Windows (named pipe).
func dialAgentConn() (net.Conn, error) {
	return winio.DialPipe(`\\.\pipe\openssh-ssh-agent`, nil)
}
