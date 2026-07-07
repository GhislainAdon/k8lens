// k8lens — binaire autonome qui embarque k8lens.sh et k8lens-renew.sh.
// Il extrait les scripts dans un répertoire temporaire puis délègue à bash
// (natif sur Linux ; Git Bash ou WSL sur Windows — ssh/scp restent requis).
package main

import (
	"embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

//go:embed k8lens.sh k8lens-renew.sh
var scripts embed.FS

func findBash() (string, error) {
	if p, err := exec.LookPath("bash"); err == nil {
		return p, nil
	}
	if runtime.GOOS == "windows" {
		for _, c := range []string{
			`C:\Program Files\Git\bin\bash.exe`,
			`C:\Program Files (x86)\Git\bin\bash.exe`,
		} {
			if _, err := os.Stat(c); err == nil {
				return c, nil
			}
		}
	}
	return "", fmt.Errorf("bash introuvable — installer Git Bash (winget install Git.Git) ou WSL")
}

func main() {
	dir, err := os.MkdirTemp("", "k8lens-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "k8lens:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	for _, name := range []string{"k8lens.sh", "k8lens-renew.sh"} {
		data, err := scripts.ReadFile(name)
		if err != nil {
			fmt.Fprintln(os.Stderr, "k8lens:", err)
			os.Exit(1)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "k8lens:", err)
			os.Exit(1)
		}
	}

	bash, err := findBash()
	if err != nil {
		fmt.Fprintln(os.Stderr, "k8lens:", err)
		os.Exit(1)
	}

	args := append([]string{filepath.Join(dir, "k8lens.sh")}, os.Args[1:]...)
	cmd := exec.Command(bash, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		fmt.Fprintln(os.Stderr, "k8lens:", err)
		os.Exit(1)
	}
}
