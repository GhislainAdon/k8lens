// Étapes de provisioning — portage Go de k8lens.sh. Les commandes distantes
// sont identiques au script bash d'origine (éprouvé sur cloud-ictneo).
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type Cluster struct {
	r             *Runner
	master        string
	workers       []string
	nodes         []string
	single        bool
	kubeVersion   string
	calicoVersion string
	podCIDR       string
	clusterName   string
	exportPath    string
	osType        map[string]string
	ip            map[string]string
}

// ---------------------------------------------------------------------------
// 0. Préflight : accès SSH passwordless + sudo
// ---------------------------------------------------------------------------
func (c *Cluster) Preflight() {
	fmt.Println("0. Préflight : accès SSH passwordless vers les cibles...")
	for _, n := range c.nodes {
		if _, err := c.r.Output(n, "true"); err != nil {
			fmt.Fprintf(os.Stderr, "ERREUR : pas d'accès SSH passwordless à %s (%v)\n", n, err)
			fmt.Fprintf(os.Stderr, "         Déployer la clé d'abord : ssh-copy-id %s\n", n)
			os.Exit(1)
		}
		if _, err := c.r.Output(n, "sudo -n true"); err != nil {
			fmt.Fprintf(os.Stderr, "ATTENTION : sudo demandera un mot de passe sur %s (NOPASSWD recommandé)\n", n)
		}
		fmt.Printf("  %s : SSH OK\n", n)
	}
}

// ---------------------------------------------------------------------------
// 1. Détection de l'OS : "ubuntu" (apt) ou "rhel" (dnf + EPEL)
// ---------------------------------------------------------------------------
func (c *Cluster) DetectOS() {
	fmt.Println("1. Détection de l'OS des cibles...")
	for _, n := range c.nodes {
		out, err := c.r.Output(n, `. /etc/os-release && echo "$ID ${ID_LIKE:-}"`)
		osName := "inconnu"
		if err == nil {
			s := strings.ToLower(out)
			switch {
			case strings.Contains(s, "ubuntu"), strings.Contains(s, "debian"):
				osName = "ubuntu"
			case strings.Contains(s, "rocky"), strings.Contains(s, "centos"),
				strings.Contains(s, "rhel"), strings.Contains(s, "almalinux"),
				strings.Contains(s, "fedora"):
				osName = "rhel"
			}
		}
		if osName == "inconnu" {
			fatalf("OS non reconnu sur %s (Ubuntu/Debian ou Rocky/CentOS/Alma attendu)", n)
		}
		c.osType[n] = osName
		fmt.Printf("  %s → %s\n", n, osName)
	}
}

func (c *Cluster) serverIP(host string) string {
	if ip, ok := c.ip[host]; ok {
		return ip
	}
	out, err := c.r.Output(host,
		`ip -4 addr show | grep 'inet ' | awk '{print $2}' | cut -d/ -f1 | grep -v '^127\.' | grep -v '^169\.254\.' | head -1`)
	if err != nil || out == "" {
		fatalf("impossible de déterminer l'IP de %s", host)
	}
	c.ip[host] = out
	return out
}

// ---------------------------------------------------------------------------
// 2. Détection d'un cluster existant
// ---------------------------------------------------------------------------
func (c *Cluster) kubectlWorks(host string) bool {
	_, err := c.r.Output(host,
		`command -v kubectl >/dev/null 2>&1 && test -f $HOME/.kube/config && kubectl get nodes >/dev/null 2>&1`)
	return err == nil
}

func (c *Cluster) nodeInCluster(node, reference string) bool {
	hostname, err := c.r.Output(node, "hostname")
	if err != nil || hostname == "" {
		return false
	}
	names, err := c.r.Output(reference,
		`kubectl get nodes -o jsonpath='{.items[*].metadata.name}' 2>/dev/null`)
	if err != nil {
		return false
	}
	return strings.Contains(names, hostname)
}

func (c *Cluster) FindWorkingNode() string {
	fmt.Println("2. Vérification préalable d'un cluster existant...")
	for _, n := range c.nodes {
		if c.kubectlWorks(n) {
			fmt.Printf("  kubectl fonctionne sur %s\n", n)
			return n
		}
	}
	fmt.Println("  aucun cluster existant détecté")
	return ""
}

// ---------------------------------------------------------------------------
// 3. Pare-feu : UFW (Ubuntu) / firewalld (famille RedHat)
// ---------------------------------------------------------------------------
func (c *Cluster) FirewallMesh() {
	fmt.Println("3. Pare-feu entre les nœuds...")
	for _, n := range c.nodes {
		fmt.Printf("  IP de %s : %s\n", n, c.serverIP(n))
	}
	for _, n := range c.nodes {
		var peers []string
		for _, o := range c.nodes {
			if o != n {
				peers = append(peers, c.serverIP(o))
			}
		}
		if c.osType[n] == "ubuntu" {
			_ = c.r.Run(n, "command -v ufw >/dev/null 2>&1 || sudo apt-get install -y ufw")
			for _, ip := range peers {
				_ = c.r.Run(n, "sudo ufw allow from "+ip)
			}
			_ = c.r.Run(n, "sudo ufw allow 6443/tcp")
		} else {
			_ = c.r.Run(n, "sudo systemctl enable --now firewalld 2>/dev/null || true")
			for _, ip := range peers {
				_ = c.r.Run(n, "sudo firewall-cmd --permanent --zone=trusted --add-source="+ip)
			}
			_ = c.r.Run(n, "sudo firewall-cmd --permanent --add-port=6443/tcp --add-port=2379-2380/tcp --add-port=10250-10259/tcp --add-port=30000-32767/tcp --add-port=179/tcp --add-port=4789/udp")
			_ = c.r.Run(n, "sudo firewall-cmd --reload")
		}
	}
}

// ---------------------------------------------------------------------------
// 4. Préparation des nœuds (containerd + kubeadm/kubelet/kubectl)
// ---------------------------------------------------------------------------
func (c *Cluster) prepareCommon(n string) error {
	cmds := []string{
		"sudo swapoff -a",
		`sudo sed -i '/ swap / s/^\(.*\)$/#\1/g' /etc/fstab`,
		`printf 'overlay\nbr_netfilter\n' | sudo tee /etc/modules-load.d/k8s.conf`,
		"sudo modprobe overlay",
		"sudo modprobe br_netfilter",
		`printf 'net.bridge.bridge-nf-call-iptables  = 1\nnet.bridge.bridge-nf-call-ip6tables = 1\nnet.ipv4.ip_forward                 = 1\n' | sudo tee /etc/sysctl.d/k8s.conf`,
		"sudo sysctl --system",
	}
	for _, cmd := range cmds {
		if err := c.r.Run(n, cmd); err != nil {
			return err
		}
	}
	return nil
}

func (c *Cluster) configureContainerd(n string) error {
	cmds := []string{
		"sudo mkdir -p /etc/containerd",
		`containerd config default | sed 's/SystemdCgroup = false/SystemdCgroup = true/' | sed 's|sandbox_image = ".*"|sandbox_image = "registry.k8s.io/pause:3.10.1"|' | sudo tee /etc/containerd/config.toml > /dev/null`,
		"sudo systemctl enable --now containerd",
		"sudo systemctl restart containerd",
	}
	for _, cmd := range cmds {
		if err := c.r.Run(n, cmd); err != nil {
			return err
		}
	}
	return nil
}

func (c *Cluster) prepareUbuntu(n string) error {
	fmt.Printf("--- Préparation (Ubuntu/Debian) de %s ---\n", n)
	cmds := []string{
		"sudo apt-get update",
		"sudo apt-get install -y containerd apt-transport-https ca-certificates curl gpg",
	}
	for _, cmd := range cmds {
		if err := c.r.Run(n, cmd); err != nil {
			return err
		}
	}
	if err := c.configureContainerd(n); err != nil {
		return err
	}
	cmds = []string{
		"sudo mkdir -p -m 755 /etc/apt/keyrings",
		fmt.Sprintf("curl -fsSL https://pkgs.k8s.io/core:/stable:/v%s/deb/Release.key | sudo gpg --dearmor --batch --yes -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg", c.kubeVersion),
		fmt.Sprintf("echo 'deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v%s/deb/ /' | sudo tee /etc/apt/sources.list.d/kubernetes.list", c.kubeVersion),
		"sudo apt-get update",
		"sudo apt-get install -y --allow-downgrades kubelet kubeadm kubectl",
		"sudo apt-mark hold kubelet kubeadm kubectl",
		"sudo systemctl enable --now kubelet",
	}
	for _, cmd := range cmds {
		if err := c.r.Run(n, cmd); err != nil {
			return err
		}
	}
	return c.prepareCommon(n)
}

func (c *Cluster) prepareRHEL(n string) error {
	fmt.Printf("--- Préparation (Rocky/CentOS/Alma — dnf + EPEL) de %s ---\n", n)
	repo := fmt.Sprintf(`[kubernetes]
name=Kubernetes
baseurl=https://pkgs.k8s.io/core:/stable:/v%s/rpm/
enabled=1
gpgcheck=1
gpgkey=https://pkgs.k8s.io/core:/stable:/v%s/rpm/repodata/repomd.xml.key
exclude=kubelet kubeadm kubectl cri-tools kubernetes-cni
`, c.kubeVersion, c.kubeVersion)

	cmds := []string{
		// EPEL fournit containerd sur EL9
		"sudo dnf install -y epel-release || sudo dnf install -y https://dl.fedoraproject.org/pub/epel/epel-release-latest-9.noarch.rpm",
		"sudo dnf install -y containerd curl",
	}
	for _, cmd := range cmds {
		if err := c.r.Run(n, cmd); err != nil {
			return err
		}
	}
	if err := c.configureContainerd(n); err != nil {
		return err
	}
	// SELinux permissif (requis par kubeadm)
	_ = c.r.Run(n, "sudo setenforce 0 2>/dev/null || true")
	_ = c.r.Run(n, "sudo sed -i 's/^SELINUX=enforcing$/SELINUX=permissive/' /etc/selinux/config")

	if err := c.r.Upload(n, []byte(repo), "cat | sudo tee /etc/yum.repos.d/kubernetes.repo > /dev/null"); err != nil {
		return err
	}
	cmds = []string{
		"sudo dnf install -y kubelet kubeadm kubectl --disableexcludes=kubernetes",
		"sudo systemctl enable --now kubelet",
	}
	for _, cmd := range cmds {
		if err := c.r.Run(n, cmd); err != nil {
			return err
		}
	}
	return c.prepareCommon(n)
}

func (c *Cluster) PrepareAll(workingNode string) {
	fmt.Println("4. Préparation des serveurs...")
	for _, n := range c.nodes {
		if workingNode != "" && c.nodeInCluster(n, workingNode) {
			fmt.Printf("  %s est déjà dans le cluster, préparation ignorée\n", n)
			continue
		}
		var err error
		if c.osType[n] == "ubuntu" {
			err = c.prepareUbuntu(n)
		} else {
			err = c.prepareRHEL(n)
		}
		if err != nil {
			fatalf("préparation de %s échouée : %v", n, err)
		}
		fmt.Printf("  serveur %s préparé avec succès\n", n)
	}
}

// ---------------------------------------------------------------------------
// 5. Init du cluster (si aucun n'existe)
// ---------------------------------------------------------------------------
func (c *Cluster) InitIfNeeded(workingNode string) string {
	fmt.Println("5. Initialisation du cluster...")
	if workingNode != "" {
		fmt.Printf("  cluster existant détecté sur %s — pas d'init\n", workingNode)
		return workingNode
	}
	masterIP := c.serverIP(c.master)
	_ = c.r.Run(c.master, "sudo fuser -k 6443/tcp 2>/dev/null || echo 'Port 6443 libre'")
	fmt.Println("  IP du master pour controlPlaneEndpoint :", masterIP)
	initCmd := fmt.Sprintf("sudo kubeadm init --pod-network-cidr=%s --cri-socket=unix:///run/containerd/containerd.sock --control-plane-endpoint=%s:6443 --upload-certs", c.podCIDR, masterIP)
	if err := c.r.Run(c.master, initCmd); err != nil {
		fatalf("kubeadm init a échoué : %v", err)
	}
	for _, cmd := range []string{
		"mkdir -p $HOME/.kube",
		"sudo cp -f /etc/kubernetes/admin.conf $HOME/.kube/config",
		"sudo chown $(id -u):$(id -g) $HOME/.kube/config",
	} {
		if err := c.r.Run(c.master, cmd); err != nil {
			fatalf("configuration kubectl sur le master échouée : %v", err)
		}
	}
	return c.master
}

// ---------------------------------------------------------------------------
// 6-7. CNI Calico + untaint mono-nœud
// ---------------------------------------------------------------------------
func (c *Cluster) Calico(workingNode string) {
	fmt.Println("6. Plugin réseau Calico...")
	if _, err := c.r.Output(workingNode, "kubectl get pods -n kube-system | grep -q calico"); err == nil {
		fmt.Println("  Calico est déjà installé")
		return
	}
	_ = c.r.Run(workingNode, fmt.Sprintf("kubectl apply -f https://raw.githubusercontent.com/projectcalico/calico/%s/manifests/calico.yaml", c.calicoVersion))
}

func (c *Cluster) Untaint(workingNode string) {
	fmt.Println("7. Mode mono-nœud : suppression du taint control-plane...")
	_ = c.r.Run(workingNode, "kubectl taint nodes --all node-role.kubernetes.io/control-plane:NoSchedule- 2>/dev/null || true")
}

// ---------------------------------------------------------------------------
// 8. Join des workers
// ---------------------------------------------------------------------------
func (c *Cluster) JoinWorkers(workingNode string) {
	fmt.Println("8. Génération du token de join...")
	join, err := c.r.Output(c.master, "sudo kubeadm token create --print-join-command")
	if err != nil || !strings.Contains(join, "kubeadm join") {
		fatalf("impossible de générer le token de join : %v (%s)", err, join)
	}
	// kubeadm peut préfixer des warnings : ne garder que la ligne du join
	for _, line := range strings.Split(join, "\n") {
		if strings.Contains(line, "kubeadm join") {
			join = strings.TrimSpace(line)
			break
		}
	}
	fmt.Println("  token généré")

	fmt.Println("9. Ajout des workers au cluster...")
	for _, w := range c.workers {
		if c.nodeInCluster(w, workingNode) {
			fmt.Printf("  %s est déjà dans le cluster\n", w)
			continue
		}
		fmt.Printf("  ajout de %s...\n", w)
		if err := c.r.Run(w, "sudo "+join); err != nil {
			fmt.Fprintf(os.Stderr, "ATTENTION : join de %s échoué : %v\n", w, err)
			continue
		}
		cfg, err := c.r.Output(c.master, "cat $HOME/.kube/config")
		if err == nil {
			_ = c.r.Upload(w, []byte(cfg),
				"mkdir -p $HOME/.kube && cat > $HOME/.kube/config && chmod 600 $HOME/.kube/config")
		}
		fmt.Printf("  %s ajouté au cluster\n", w)
	}
}

// ---------------------------------------------------------------------------
// 9. Addons optionnels
// ---------------------------------------------------------------------------
func (c *Cluster) Addons(workingNode string) {
	fmt.Println("10. Addons (helm, local-path-provisioner, ingress-nginx)...")
	cmds := []string{
		"command -v helm > /dev/null 2>&1 || (curl -fsSL -o get_helm.sh https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 && chmod 700 get_helm.sh && ./get_helm.sh && rm get_helm.sh)",
		"kubectl apply -f https://raw.githubusercontent.com/rancher/local-path-provisioner/master/deploy/local-path-storage.yaml",
		`kubectl patch storageclass local-path -p '{"metadata": {"annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}}'`,
		"helm repo add ingress-nginx https://kubernetes.github.io/ingress-nginx 2>/dev/null || true",
		"helm repo update",
		"helm upgrade --install ingress-nginx ingress-nginx/ingress-nginx --namespace ingress-nginx --create-namespace --set controller.service.type=NodePort --wait",
	}
	for _, cmd := range cmds {
		if err := c.r.Run(workingNode, cmd); err != nil {
			fmt.Fprintf(os.Stderr, "ATTENTION : addon échoué (%v) — on continue\n", err)
		}
	}
}

// ---------------------------------------------------------------------------
// 10. Cron de renouvellement du token (expiration 24h)
// ---------------------------------------------------------------------------
func (c *Cluster) InstallCron() {
	fmt.Println("11. Installation du cron de renouvellement sur le master...")
	if err := c.r.Upload(c.master, renewScript, "cat > /tmp/k8lens-renew.sh"); err != nil {
		fmt.Fprintf(os.Stderr, "ATTENTION : upload du script de renouvellement échoué : %v\n", err)
		return
	}
	cmds := []string{
		"sed -i 's/\\r$//' /tmp/k8lens-renew.sh && sudo install -m 755 /tmp/k8lens-renew.sh /usr/local/bin/k8lens-renew.sh && rm /tmp/k8lens-renew.sh",
		"echo '0 */12 * * * root /usr/local/bin/k8lens-renew.sh' | sudo tee /etc/cron.d/k8lens && sudo chmod 644 /etc/cron.d/k8lens",
		"sudo /usr/local/bin/k8lens-renew.sh",
	}
	for _, cmd := range cmds {
		if err := c.r.Run(c.master, cmd); err != nil {
			fmt.Fprintf(os.Stderr, "ATTENTION : installation du cron : %v\n", err)
			return
		}
	}
	fmt.Println("  cron installé : token régénéré toutes les 12h (TTL 24h)")
	fmt.Println("  commande de join toujours à jour dans /var/lib/k8lens/join-command sur le master")
}

// ---------------------------------------------------------------------------
// 11. Export du kubeconfig pour Freelens
// ---------------------------------------------------------------------------
func (c *Cluster) ExportKubeconfig() {
	fmt.Println("12. Export du kubeconfig pour Freelens...")
	adm, err := c.r.Output(c.master, "sudo cat /etc/kubernetes/admin.conf")
	if err != nil {
		fatalf("lecture de admin.conf impossible : %v", err)
	}
	masterIP := c.serverIP(c.master)
	adm = regexp.MustCompile(`server: https://[^\s]+:6443`).
		ReplaceAllString(adm, "server: https://"+masterIP+":6443")
	adm = strings.ReplaceAll(adm, "kubernetes-admin@kubernetes", c.clusterName)
	adm = regexp.MustCompile(`(?m)^(\s*name: )kubernetes\s*$`).
		ReplaceAllString(adm, "${1}"+c.clusterName)
	adm = regexp.MustCompile(`(?m)^(\s*cluster: )kubernetes\s*$`).
		ReplaceAllString(adm, "${1}"+c.clusterName)

	if err := os.MkdirAll(filepath.Dir(c.exportPath), 0o700); err != nil {
		fatalf("création de %s impossible : %v", filepath.Dir(c.exportPath), err)
	}
	if err := os.WriteFile(c.exportPath, []byte(adm+"\n"), 0o600); err != nil {
		fatalf("écriture de %s impossible : %v", c.exportPath, err)
	}
}
