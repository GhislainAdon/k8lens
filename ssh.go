// Client SSH natif : clés ~/.ssh/id_*, agent SSH (OpenSSH Windows inclus),
// alias de ~/.ssh/config, known_hosts en mode accept-new (TOFU).
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	osuser "os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kevinburke/ssh_config"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

type Runner struct {
	sshUser string
	mu      sync.Mutex
	clients map[string]*ssh.Client
}

func NewRunner(sshUser string) *Runner {
	return &Runner{sshUser: sshUser, clients: map[string]*ssh.Client{}}
}

type target struct {
	addr, user    string
	identityFiles []string
}

// resolve interprète "user@hote" et les alias de ~/.ssh/config
// (HostName, User, Port, IdentityFile).
func (r *Runner) resolve(host string) target {
	user := r.sshUser
	h := host
	if i := strings.Index(h, "@"); i >= 0 {
		user, h = h[:i], h[i+1:]
	}
	hostname, _ := ssh_config.GetStrict(h, "HostName")
	if hostname == "" {
		hostname = h
	}
	if user == "" {
		user, _ = ssh_config.GetStrict(h, "User")
	}
	if user == "" {
		if u, err := osuser.Current(); err == nil {
			user = u.Username
			// Windows renvoie "MACHINE\utilisateur"
			if i := strings.LastIndexAny(user, `\`); i >= 0 {
				user = user[i+1:]
			}
		}
	}
	port, _ := ssh_config.GetStrict(h, "Port")
	if port == "" {
		port = "22"
	}
	ids, _ := ssh_config.GetAllStrict(h, "IdentityFile")
	return target{addr: net.JoinHostPort(hostname, port), user: user, identityFiles: ids}
}

func loadSigners(extra []string) []ssh.Signer {
	home, _ := os.UserHomeDir()
	seen := map[string]bool{}
	var files []string
	for _, f := range append(append([]string{}, extra...),
		"~/.ssh/id_ed25519", "~/.ssh/id_rsa", "~/.ssh/id_ecdsa") {
		if strings.HasPrefix(f, "~") {
			f = filepath.Join(home, f[1:])
		}
		f = filepath.Clean(f)
		if seen[f] {
			continue
		}
		seen[f] = true
		files = append(files, f)
	}
	var signers []ssh.Signer
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		s, err := ssh.ParsePrivateKey(b)
		if err != nil {
			continue
		}
		signers = append(signers, s)
	}
	return signers
}

// hostKeyCallback vérifie ~/.ssh/known_hosts ; hôte inconnu → ajouté
// automatiquement (équivalent StrictHostKeyChecking=accept-new).
// Une clé qui NE CORRESPOND PLUS reste une erreur bloquante.
func hostKeyCallback() ssh.HostKeyCallback {
	home, err := os.UserHomeDir()
	if err != nil {
		return ssh.InsecureIgnoreHostKey()
	}
	path := filepath.Join(home, ".ssh", "known_hosts")
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	if _, err := os.Stat(path); err != nil {
		_ = os.WriteFile(path, nil, 0o600)
	}
	base, err := knownhosts.New(path)
	if err != nil {
		return ssh.InsecureIgnoreHostKey()
	}
	var mu sync.Mutex
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := base(hostname, remote, key)
		if err == nil {
			return nil
		}
		var ke *knownhosts.KeyError
		if errors.As(err, &ke) && len(ke.Want) == 0 {
			mu.Lock()
			defer mu.Unlock()
			f, ferr := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
			if ferr != nil {
				return err
			}
			defer f.Close()
			_, _ = fmt.Fprintln(f, knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key))
			return nil
		}
		return err
	}
}

func (r *Runner) client(host string) (*ssh.Client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.clients[host]; ok {
		return c, nil
	}
	t := r.resolve(host)
	var methods []ssh.AuthMethod
	if conn, err := dialAgentConn(); err == nil {
		methods = append(methods, ssh.PublicKeysCallback(agent.NewClient(conn).Signers))
	}
	if s := loadSigners(t.identityFiles); len(s) > 0 {
		methods = append(methods, ssh.PublicKeys(s...))
	}
	if len(methods) == 0 {
		return nil, fmt.Errorf("aucune clé SSH (~/.ssh/id_*) ni agent SSH disponible")
	}
	cfg := &ssh.ClientConfig{
		User:            t.user,
		Auth:            methods,
		HostKeyCallback: hostKeyCallback(),
		Timeout:         10 * time.Second,
	}
	c, err := ssh.Dial("tcp", t.addr, cfg)
	if err != nil {
		return nil, err
	}
	r.clients[host] = c
	return c, nil
}

func (r *Runner) run(host, cmd string, stream bool, stdin io.Reader) (string, error) {
	c, err := r.client(host)
	if err != nil {
		return "", err
	}
	sess, err := c.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	var buf bytes.Buffer
	if stream {
		sess.Stdout = io.MultiWriter(os.Stdout, &buf)
		sess.Stderr = os.Stderr
	} else {
		sess.Stdout = &buf
		sess.Stderr = &buf
	}
	if stdin != nil {
		sess.Stdin = stdin
	}
	err = sess.Run(cmd)
	return strings.TrimSpace(buf.String()), err
}

// Run exécute en affichant la commande et sa sortie (équivalent run_remote).
func (r *Runner) Run(host, cmd string) error {
	fmt.Printf("[%s] %s\n", host, cmd)
	_, err := r.run(host, cmd, true, nil)
	return err
}

// Output exécute silencieusement et retourne stdout+stderr.
func (r *Runner) Output(host, cmd string) (string, error) {
	return r.run(host, cmd, false, nil)
}

// Upload pousse data sur stdin d'une commande distante (pas besoin de scp/sftp).
func (r *Runner) Upload(host string, data []byte, cmd string) error {
	_, err := r.run(host, cmd, false, bytes.NewReader(data))
	return err
}
