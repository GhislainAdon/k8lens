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
	"strings"
)

//go:embed k8lens.sh k8lens-renew.sh
var scripts embed.FS

// findBash localise bash. Sous Windows on veut le bash de Git Bash (livré avec
// ssh/scp) et JAMAIS C:\Windows\System32\bash.exe qui est le lanceur WSL — il
// échoue avec « execvpe(/bin/bash) failed » quand aucune distribution WSL
// n'est installée.
func findBash() (string, error) {
	if runtime.GOOS != "windows" {
		if p, err := exec.LookPath("bash"); err == nil {
			return p, nil
		}
		return "", fmt.Errorf("bash introuvable")
	}

	// 1. Déduire l'installation Git Bash depuis git.exe
	//    (layouts : <Git>\cmd\git.exe, <Git>\bin\git.exe, <Git>\mingw64\bin\git.exe)
	if git, err := exec.LookPath("git.exe"); err == nil {
		dir := filepath.Dir(git)
		for i := 0; i < 3; i++ {
			for _, rel := range []string{`bin\bash.exe`, `usr\bin\bash.exe`} {
				p := filepath.Join(dir, rel)
				if _, err := os.Stat(p); err == nil {
					return p, nil
				}
			}
			dir = filepath.Dir(dir)
		}
	}

	// 2. Emplacements d'installation connus de Git pour Windows
	home, _ := os.UserHomeDir()
	for _, c := range []string{
		`C:\Program Files\Git\bin\bash.exe`,
		`C:\Program Files (x86)\Git\bin\bash.exe`,
		filepath.Join(os.Getenv("LOCALAPPDATA"), `Programs\Git\bin\bash.exe`),
		filepath.Join(home, `scoop\apps\git\current\bin\bash.exe`),
	} {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}

	// 3. Dernier recours : le PATH, en excluant le bash WSL
	if p, err := exec.LookPath("bash.exe"); err == nil {
		low := strings.ToLower(p)
		if !strings.Contains(low, `\system32\`) && !strings.Contains(low, `\windowsapps\`) {
			return p, nil
		}
	}

	return "", fmt.Errorf("Git Bash introuvable — installer Git pour Windows : winget install Git.Git")
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
