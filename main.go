// k8lens — CLI native (aucune dépendance bash/WSL) qui crée un cluster
// Kubernetes kubeadm mono-nœud ou 1 master + N workers via SSH, puis exporte
// un kubeconfig prêt à importer dans Freelens.
//
// Équivalent Go du script k8lens.sh (conservé dans le repo comme alternative).
package main

import (
	"bufio"
	_ "embed"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

//go:embed k8lens-renew.sh
var renewScript []byte

const version = "0.2.0"

type stringSlice []string

func (s *stringSlice) String() string     { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error { *s = append(*s, v); return nil }

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "ERREUR : "+format+"\n", a...)
	os.Exit(1)
}

func usage() {
	fmt.Fprint(os.Stdout, `k8lens — cluster Kubernetes kubeadm (mono ou 1 master + N workers) + pilotage Freelens

Usage : k8lens [options]

Options :
  --master HOTE            Hôte du control-plane (obligatoire, ou demandé en console)
  --worker HOTE            Hôte worker (répétable : --worker w1 --worker w2 ...)
  --single                 Cluster mono-nœud (master seul, taint control-plane retiré)
  --k8s-version X.Y        Version Kubernetes (défaut : 1.34)
  --calico-version vX.Y.Z  Version Calico (défaut : v3.27.0)
  --pod-cidr CIDR          CIDR des pods (défaut : 192.168.0.0/16)
  --ssh-user USER          Utilisateur SSH sur les cibles (défaut : ~/.ssh/config puis utilisateur courant)
  --cluster-name NOM       Nom du contexte dans le kubeconfig exporté (défaut : k8lens)
  --export-kubeconfig F    Chemin local du kubeconfig exporté (défaut : ~/.kube/<cluster-name>.kubeconfig)
  --install-cron           Installe le cron de renouvellement du token de join sur le master
  --with-addons            Installe helm + local-path-provisioner + ingress-nginx
  --version                Affiche la version
  -h, --help               Cette aide

Prérequis : accès SSH passwordless (clé ~/.ssh/id_* ou agent SSH) vers les cibles,
sudo NOPASSWD recommandé. Les alias de ~/.ssh/config sont supportés.
`)
}

func main() {
	var (
		master, sshUser, podCIDR, kubeVersion, calicoVersion, clusterName, exportPath string
		workers                                                                      stringSlice
		single, installCron, withAddons, showVersion                                 bool
	)
	flag.StringVar(&master, "master", "", "")
	flag.Var(&workers, "worker", "")
	flag.BoolVar(&single, "single", false, "")
	flag.StringVar(&kubeVersion, "k8s-version", "1.34", "")
	flag.StringVar(&calicoVersion, "calico-version", "v3.27.0", "")
	flag.StringVar(&podCIDR, "pod-cidr", "192.168.0.0/16", "")
	flag.StringVar(&sshUser, "ssh-user", "", "")
	flag.StringVar(&clusterName, "cluster-name", "k8lens", "")
	flag.StringVar(&exportPath, "export-kubeconfig", "", "")
	flag.BoolVar(&installCron, "install-cron", false, "")
	flag.BoolVar(&withAddons, "with-addons", false, "")
	flag.BoolVar(&showVersion, "version", false, "")
	flag.Usage = usage
	flag.Parse()

	if showVersion {
		fmt.Println("k8lens", version)
		return
	}

	// Saisie interactive si le master n'est pas fourni
	stdin := bufio.NewReader(os.Stdin)
	if master == "" {
		fmt.Print("Hôte du master (IP ou nom SSH) : ")
		line, _ := stdin.ReadString('\n')
		master = strings.TrimSpace(line)
		if master == "" {
			fatalf("master obligatoire")
		}
		if !single && len(workers) == 0 {
			fmt.Print("Workers (séparés par des espaces, vide = mono-nœud) : ")
			line, _ = stdin.ReadString('\n')
			workers = strings.Fields(line)
			if len(workers) == 0 {
				single = true
			}
		}
	}
	if single && len(workers) > 0 {
		fatalf("--single est incompatible avec --worker")
	}

	if exportPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fatalf("répertoire home introuvable : %v", err)
		}
		exportPath = filepath.Join(home, ".kube", clusterName+".kubeconfig")
	}

	c := &Cluster{
		r:             NewRunner(sshUser),
		master:        master,
		workers:       workers,
		single:        single,
		kubeVersion:   kubeVersion,
		calicoVersion: calicoVersion,
		podCIDR:       podCIDR,
		clusterName:   clusterName,
		exportPath:    exportPath,
		osType:        map[string]string{},
		ip:            map[string]string{},
	}
	c.nodes = append([]string{master}, workers...)

	fmt.Printf("=== k8lens %s : cluster Kubernetes v%s ===\n", version, kubeVersion)
	fmt.Println("Master  :", master)
	if single {
		fmt.Println("Mode    : mono-nœud")
	} else {
		fmt.Println("Workers :", strings.Join(workers, " "))
	}

	c.Preflight()
	c.DetectOS()
	workingNode := c.FindWorkingNode()
	c.FirewallMesh()
	c.PrepareAll(workingNode)
	workingNode = c.InitIfNeeded(workingNode)
	c.Calico(workingNode)
	if single {
		c.Untaint(workingNode)
	}
	if len(workers) > 0 {
		c.JoinWorkers(workingNode)
	}
	if withAddons {
		c.Addons(workingNode)
	}
	if installCron {
		c.InstallCron()
	}
	c.ExportKubeconfig()

	fmt.Println("")
	fmt.Println("=== CLUSTER OPÉRATIONNEL ===")
	_ = c.r.Run(workingNode, "kubectl get nodes -o wide")
	fmt.Println("")
	fmt.Println("Kubeconfig exporté :", exportPath)
	fmt.Println("")
	fmt.Println("Pilotage avec Freelens (https://github.com/freelensapp/freelens) :")
	fmt.Println("  1. Ouvrir Freelens")
	fmt.Println("  2. Catalog → Clusters → + → 'Sync kubeconfig file(s)'")
	fmt.Println("  3. Sélectionner :", exportPath)
}
