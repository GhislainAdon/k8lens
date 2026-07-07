#!/usr/bin/env bash
# k8lens.sh — Création d'un cluster Kubernetes (kubeadm) mono-nœud ou 1 master + N workers,
#             piloté ensuite avec Freelens (https://github.com/freelensapp/freelens).
#
# OS supportés sur les cibles : Ubuntu/Debian (apt) et Rocky/CentOS/AlmaLinux (dnf + EPEL).
# Tout est paramétrable depuis la console (aucune variable codée en dur).
#
# Exemples :
#   ./k8lens.sh --single --master 192.168.1.50
#   ./k8lens.sh --master srv-master --worker srv-w1 --worker srv-w2 --k8s-version 1.34
#   ./k8lens.sh --master 10.0.0.10 --worker 10.0.0.11 --ssh-user rocky --install-cron
#
# Idempotent : re-lançable à tout moment, les nœuds déjà dans le cluster sont ignorés.

set -u

# ---------------------------------------------------------------------------
# Paramètres (tous surchargables en ligne de commande)
# ---------------------------------------------------------------------------
KUBE_VERSION="1.34"
CALICO_VERSION="v3.27.0"
POD_CIDR="192.168.0.0/16"
SSH_USER=""
MASTER=""
WORKERS=()
SINGLE=false
INSTALL_CRON=false
WITH_ADDONS=false
EXPORT_KUBECONFIG=""
CLUSTER_NAME="k8lens"

usage() {
    cat <<'EOF'
k8lens — cluster Kubernetes kubeadm (mono ou 1 master + N workers) + pilotage Freelens

Usage : ./k8lens.sh [options]

Options :
  --master HOTE            Hôte du control-plane (obligatoire, ou demandé en console)
  --worker HOTE            Hôte worker (répétable : --worker w1 --worker w2 ...)
  --single                 Cluster mono-nœud (master seul, taint control-plane retiré)
  --k8s-version X.Y        Version Kubernetes (défaut : 1.34)
  --calico-version vX.Y.Z  Version Calico (défaut : v3.27.0)
  --pod-cidr CIDR          CIDR des pods (défaut : 192.168.0.0/16)
  --ssh-user USER          Utilisateur SSH sur les cibles (défaut : celui de ~/.ssh/config)
  --cluster-name NOM       Nom du contexte dans le kubeconfig exporté (défaut : k8lens)
  --export-kubeconfig F    Chemin local du kubeconfig exporté
                           (défaut : ~/.kube/<cluster-name>.kubeconfig)
  --install-cron           Installe le cron de renouvellement du token de join sur le master
  --with-addons            Installe helm + local-path-provisioner + ingress-nginx
  -h, --help               Cette aide

Prérequis : accès SSH par clé aux cibles, sudo (NOPASSWD recommandé).
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        --master)            MASTER="$2"; shift 2 ;;
        --worker)            WORKERS+=("$2"); shift 2 ;;
        --single)            SINGLE=true; shift ;;
        --k8s-version)       KUBE_VERSION="$2"; shift 2 ;;
        --calico-version)    CALICO_VERSION="$2"; shift 2 ;;
        --pod-cidr)          POD_CIDR="$2"; shift 2 ;;
        --ssh-user)          SSH_USER="$2"; shift 2 ;;
        --cluster-name)      CLUSTER_NAME="$2"; shift 2 ;;
        --export-kubeconfig) EXPORT_KUBECONFIG="$2"; shift 2 ;;
        --install-cron)      INSTALL_CRON=true; shift ;;
        --with-addons)       WITH_ADDONS=true; shift ;;
        -h|--help)           usage; exit 0 ;;
        *) echo "Option inconnue : $1" >&2; usage; exit 1 ;;
    esac
done

# Saisie interactive si le master n'est pas fourni
if [ -z "$MASTER" ]; then
    read -r -p "Hôte du master (IP ou nom SSH) : " MASTER
    [ -z "$MASTER" ] && { echo "ERREUR : master obligatoire" >&2; exit 1; }
    if [ "$SINGLE" = false ] && [ ${#WORKERS[@]} -eq 0 ]; then
        read -r -p "Workers (séparés par des espaces, vide = mono-nœud) : " -a WORKERS
        [ ${#WORKERS[@]} -eq 0 ] && SINGLE=true
    fi
fi

if [ "$SINGLE" = true ] && [ ${#WORKERS[@]} -gt 0 ]; then
    echo "ERREUR : --single est incompatible avec --worker" >&2
    exit 1
fi

[ -z "$EXPORT_KUBECONFIG" ] && EXPORT_KUBECONFIG="$HOME/.kube/${CLUSTER_NAME}.kubeconfig"

ALL_NODES=("$MASTER" "${WORKERS[@]:-}")
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "=== k8lens : cluster Kubernetes v${KUBE_VERSION} ==="
echo "Master  : $MASTER"
if [ "$SINGLE" = true ]; then
    echo "Mode    : mono-nœud"
else
    echo "Workers : ${WORKERS[*]}"
fi

# ---------------------------------------------------------------------------
# Helpers SSH
# ---------------------------------------------------------------------------
ssh_target() {
    if [ -n "$SSH_USER" ]; then echo "${SSH_USER}@$1"; else echo "$1"; fi
}

run_remote() {
    local server=$1
    local command=$2
    echo "[$server] $command" >&2
    ssh -o StrictHostKeyChecking=accept-new "$(ssh_target "$server")" "$command"
}

get_server_ip() {
    local server=$1
    ssh "$(ssh_target "$server")" "ip -4 addr show | grep 'inet ' | awk '{print \$2}' | cut -d/ -f1 | grep -v '^127\.' | grep -v '^169\.254\.' | head -1"
}

# ---------------------------------------------------------------------------
# Détection de l'OS de chaque cible : "ubuntu" (apt) ou "rhel" (dnf + EPEL)
# ---------------------------------------------------------------------------
detect_os() {
    local server=$1
    local id id_like
    id=$(ssh "$(ssh_target "$server")" ". /etc/os-release && echo \$ID" 2>/dev/null)
    id_like=$(ssh "$(ssh_target "$server")" ". /etc/os-release && echo \${ID_LIKE:-}" 2>/dev/null)
    case "$id $id_like" in
        *ubuntu*|*debian*)          echo "ubuntu" ;;
        *rocky*|*centos*|*rhel*|*almalinux*|*fedora*) echo "rhel" ;;
        *) echo "inconnu" ;;
    esac
}

# ---------------------------------------------------------------------------
# Vérifications d'état du cluster (réutilisées du script cloud-ictneo)
# ---------------------------------------------------------------------------
check_kubectl_working() {
    local server=$1
    ssh "$(ssh_target "$server")" "command -v kubectl > /dev/null 2>&1" || return 1
    ssh "$(ssh_target "$server")" "test -f \$HOME/.kube/config" || return 1
    ssh "$(ssh_target "$server")" "kubectl get nodes > /dev/null 2>&1" || return 1
    return 0
}

check_node_in_cluster() {
    local server=$1
    local reference_node=$2
    local hostname
    hostname=$(ssh "$(ssh_target "$server")" "hostname" 2>/dev/null)
    [ -z "$hostname" ] || [ -z "$reference_node" ] && return 1
    ssh "$(ssh_target "$reference_node")" "kubectl get nodes -o jsonpath='{.items[*].metadata.name}' 2>/dev/null" | grep -q "$hostname"
}

find_working_kubectl_node() {
    local server
    for server in "${ALL_NODES[@]}"; do
        [ -z "$server" ] && continue
        echo "Vérification de kubectl sur $server..." >&2
        if check_kubectl_working "$server"; then
            echo "kubectl fonctionne sur $server" >&2
            echo "$server"
            return 0
        fi
    done
    return 1
}

# ---------------------------------------------------------------------------
# Pare-feu : UFW (Ubuntu) / firewalld (Rocky, CentOS, Alma)
# ---------------------------------------------------------------------------
open_firewall() {
    local server=$1 os=$2
    shift 2
    local peer_ips=("$@")

    if [ "$os" = "ubuntu" ]; then
        # Autoriser les autres nœuds + l'API server (Freelens s'y connecte depuis le poste)
        run_remote "$server" "command -v ufw >/dev/null 2>&1 || sudo apt-get install -y ufw"
        local ip
        for ip in "${peer_ips[@]}"; do
            [ -n "$ip" ] && run_remote "$server" "sudo ufw allow from $ip"
        done
        run_remote "$server" "sudo ufw allow 6443/tcp"
    else
        run_remote "$server" "sudo systemctl enable --now firewalld 2>/dev/null || true"
        local ip
        for ip in "${peer_ips[@]}"; do
            [ -n "$ip" ] && run_remote "$server" "sudo firewall-cmd --permanent --zone=trusted --add-source=$ip"
        done
        # Ports control-plane + kubelet + NodePort + Calico (BGP/VXLAN)
        run_remote "$server" "sudo firewall-cmd --permanent --add-port=6443/tcp --add-port=2379-2380/tcp --add-port=10250-10259/tcp --add-port=30000-32767/tcp --add-port=179/tcp --add-port=4789/udp"
        run_remote "$server" "sudo firewall-cmd --reload"
    fi
}

configure_firewall_mesh() {
    echo "=== Configuration du pare-feu entre les nœuds ==="
    declare -A NODE_IPS
    local server
    for server in "${ALL_NODES[@]}"; do
        [ -z "$server" ] && continue
        NODE_IPS[$server]=$(get_server_ip "$server")
        echo "IP de $server : ${NODE_IPS[$server]}"
    done
    for server in "${ALL_NODES[@]}"; do
        [ -z "$server" ] && continue
        local peers=()
        local other
        for other in "${ALL_NODES[@]}"; do
            [ -z "$other" ] || [ "$other" = "$server" ] && continue
            peers+=("${NODE_IPS[$other]}")
        done
        open_firewall "$server" "${NODE_OS[$server]}" "${peers[@]:-}"
    done
}

# ---------------------------------------------------------------------------
# Préparation commune (swap, modules kernel, sysctl)
# ---------------------------------------------------------------------------
prepare_common() {
    local server=$1

    run_remote "$server" "sudo swapoff -a"
    run_remote "$server" "sudo sed -i '/ swap / s/^\(.*\)$/#\1/g' /etc/fstab"

    run_remote "$server" "cat <<EOF | sudo tee /etc/modules-load.d/k8s.conf
overlay
br_netfilter
EOF"
    run_remote "$server" "sudo modprobe overlay"
    run_remote "$server" "sudo modprobe br_netfilter"

    run_remote "$server" "cat <<EOF | sudo tee /etc/sysctl.d/k8s.conf
net.bridge.bridge-nf-call-iptables  = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward                 = 1
EOF"
    run_remote "$server" "sudo sysctl --system"
}

configure_containerd() {
    local server=$1
    run_remote "$server" "sudo mkdir -p /etc/containerd"
    run_remote "$server" "containerd config default | sed 's/SystemdCgroup = false/SystemdCgroup = true/' | sed 's|sandbox_image = \".*\"|sandbox_image = \"registry.k8s.io/pause:3.10.1\"|' | sudo tee /etc/containerd/config.toml > /dev/null"
    run_remote "$server" "sudo systemctl enable --now containerd"
    run_remote "$server" "sudo systemctl restart containerd"
}

# ---------------------------------------------------------------------------
# Préparation Ubuntu/Debian (apt)
# ---------------------------------------------------------------------------
prepare_ubuntu() {
    local server=$1
    echo "--- Préparation (Ubuntu/Debian) de $server ---"

    run_remote "$server" "sudo apt-get update"
    run_remote "$server" "sudo apt-get install -y containerd apt-transport-https ca-certificates curl gpg"
    configure_containerd "$server"

    run_remote "$server" "sudo mkdir -p -m 755 /etc/apt/keyrings"
    run_remote "$server" "curl -fsSL https://pkgs.k8s.io/core:/stable:/v$KUBE_VERSION/deb/Release.key | sudo gpg --dearmor --batch --yes -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg"
    run_remote "$server" "echo 'deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v$KUBE_VERSION/deb/ /' | sudo tee /etc/apt/sources.list.d/kubernetes.list"
    run_remote "$server" "sudo apt-get update"
    run_remote "$server" "sudo apt-get install -y --allow-downgrades kubelet kubeadm kubectl"
    run_remote "$server" "sudo apt-mark hold kubelet kubeadm kubectl"
    run_remote "$server" "sudo systemctl enable --now kubelet"

    prepare_common "$server"
}

# ---------------------------------------------------------------------------
# Préparation Rocky/CentOS/AlmaLinux (dnf + EPEL)
# ---------------------------------------------------------------------------
prepare_rhel() {
    local server=$1
    echo "--- Préparation (Rocky/CentOS/Alma — dnf + EPEL) de $server ---"

    # EPEL fournit containerd sur EL9
    run_remote "$server" "sudo dnf install -y epel-release || sudo dnf install -y https://dl.fedoraproject.org/pub/epel/epel-release-latest-9.noarch.rpm"
    run_remote "$server" "sudo dnf install -y containerd curl"
    configure_containerd "$server"

    # SELinux permissif (requis par kubeadm tant que les policies ne sont pas gérées)
    run_remote "$server" "sudo setenforce 0 2>/dev/null || true"
    run_remote "$server" "sudo sed -i 's/^SELINUX=enforcing$/SELINUX=permissive/' /etc/selinux/config"

    # Dépôt Kubernetes officiel (pkgs.k8s.io, versionné)
    run_remote "$server" "cat <<EOF | sudo tee /etc/yum.repos.d/kubernetes.repo
[kubernetes]
name=Kubernetes
baseurl=https://pkgs.k8s.io/core:/stable:/v$KUBE_VERSION/rpm/
enabled=1
gpgcheck=1
gpgkey=https://pkgs.k8s.io/core:/stable:/v$KUBE_VERSION/rpm/repodata/repomd.xml.key
exclude=kubelet kubeadm kubectl cri-tools kubernetes-cni
EOF"
    run_remote "$server" "sudo dnf install -y kubelet kubeadm kubectl --disableexcludes=kubernetes"
    run_remote "$server" "sudo systemctl enable --now kubelet"

    prepare_common "$server"
}

prepare_server() {
    local server=$1
    local os=${NODE_OS[$server]}
    case "$os" in
        ubuntu) prepare_ubuntu "$server" ;;
        rhel)   prepare_rhel "$server" ;;
        *) echo "ERREUR : OS non supporté sur $server (Ubuntu/Debian ou Rocky/CentOS/Alma attendu)" >&2; exit 1 ;;
    esac
    echo "Serveur $server préparé avec succès"
}

prepare_server_if_needed() {
    local server=$1
    [ -z "$server" ] && return 0
    if [ -n "${WORKING_NODE:-}" ] && check_node_in_cluster "$server" "$WORKING_NODE"; then
        echo "$server est déjà dans le cluster, préparation ignorée"
        return 0
    fi
    prepare_server "$server"
}

# ---------------------------------------------------------------------------
# Join d'un nœud au cluster
# ---------------------------------------------------------------------------
join_node_to_cluster() {
    local node=$1
    echo "Vérification si $node est dans le cluster..."

    if [ -n "${WORKING_NODE:-}" ] && check_node_in_cluster "$node" "$WORKING_NODE"; then
        echo "$node est déjà dans le cluster"
        return 0
    fi

    echo "Ajout de $node au cluster..."
    ssh "$(ssh_target "$node")" "export JOIN_CMD='$JOIN_TOKEN' && sudo bash -c \"\$JOIN_CMD\""

    # kubectl sur le nœud (copie de la config du master)
    ssh "$(ssh_target "$node")" "mkdir -p \$HOME/.kube"
    ssh "$(ssh_target "$MASTER")" "cat \$HOME/.kube/config" | ssh "$(ssh_target "$node")" "cat > \$HOME/.kube/config && chmod 600 \$HOME/.kube/config"

    echo "$node a été ajouté au cluster avec succès"
}

# ===========================================================================
# Déroulé principal
# ===========================================================================

# 0. Préflight : la machine d'installation doit avoir un accès SSH
#    passwordless (clé) vers le master et tous les workers.
echo "0. Préflight : accès SSH passwordless vers les cibles..."
for server in "${ALL_NODES[@]}"; do
    [ -z "$server" ] && continue
    if ! ssh -o BatchMode=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=accept-new "$(ssh_target "$server")" true 2>/dev/null; then
        echo "ERREUR : pas d'accès SSH passwordless à $server." >&2
        echo "         Déployer la clé d'abord : ssh-copy-id $(ssh_target "$server")" >&2
        exit 1
    fi
    if ! ssh -o BatchMode=yes "$(ssh_target "$server")" "sudo -n true" 2>/dev/null; then
        echo "ATTENTION : sudo demandera un mot de passe sur $server (NOPASSWD recommandé)" >&2
    fi
    echo "  $server : SSH OK"
done

# 1. Détection de l'OS de chaque nœud
echo "1. Détection de l'OS des cibles..."
declare -A NODE_OS
for server in "${ALL_NODES[@]}"; do
    [ -z "$server" ] && continue
    NODE_OS[$server]=$(detect_os "$server")
    echo "  $server → ${NODE_OS[$server]}"
    if [ "${NODE_OS[$server]}" = "inconnu" ]; then
        echo "ERREUR : OS non reconnu sur $server" >&2
        exit 1
    fi
done

# 2. Cluster existant ?
echo "2. Vérification préalable d'un cluster existant..."
WORKING_NODE=$(find_working_kubectl_node) || WORKING_NODE=""

# 3. Pare-feu
echo "3. Pare-feu..."
configure_firewall_mesh

# 4. Préparation des nœuds manquants
echo "4. Préparation des serveurs..."
for server in "${ALL_NODES[@]}"; do
    prepare_server_if_needed "$server"
done

# 5. Init du cluster si nécessaire
echo "5. Initialisation du cluster..."
[ -z "$WORKING_NODE" ] && WORKING_NODE=$(find_working_kubectl_node) || true
MASTER_IP=$(get_server_ip "$MASTER")

if [ -n "$WORKING_NODE" ]; then
    echo "Cluster existant détecté sur $WORKING_NODE — pas d'init"
    SKIP_INIT=true
else
    SKIP_INIT=false
    run_remote "$MASTER" "sudo fuser -k 6443/tcp 2>/dev/null || echo 'Port 6443 libre'"
    sleep 2
    echo "IP du master pour controlPlaneEndpoint : $MASTER_IP"
    run_remote "$MASTER" "sudo kubeadm init --pod-network-cidr=$POD_CIDR --cri-socket=unix:///run/containerd/containerd.sock --control-plane-endpoint=$MASTER_IP:6443 --upload-certs"

    echo "Attente de l'initialisation du cluster..."
    sleep 10
    run_remote "$MASTER" "mkdir -p \$HOME/.kube"
    run_remote "$MASTER" "sudo cp -f /etc/kubernetes/admin.conf \$HOME/.kube/config"
    run_remote "$MASTER" "sudo chown \$(id -u):\$(id -g) \$HOME/.kube/config"
    WORKING_NODE=$MASTER
fi

# 6. CNI Calico
echo "6. Plugin réseau Calico..."
if ! ssh "$(ssh_target "$WORKING_NODE")" "kubectl get pods -n kube-system | grep -q calico"; then
    run_remote "$WORKING_NODE" "kubectl apply -f https://raw.githubusercontent.com/projectcalico/calico/$CALICO_VERSION/manifests/calico.yaml"
else
    echo "Calico est déjà installé"
fi

# 7. Mono-nœud : retirer le taint control-plane pour scheduler les pods
if [ "$SINGLE" = true ]; then
    echo "7. Mode mono-nœud : suppression du taint control-plane..."
    run_remote "$WORKING_NODE" "kubectl taint nodes --all node-role.kubernetes.io/control-plane:NoSchedule- 2>/dev/null || true"
fi

# 8. Join des workers
if [ ${#WORKERS[@]} -gt 0 ]; then
    echo "8. Génération du token de join..."
    JOIN_TOKEN=$(ssh "$(ssh_target "$MASTER")" "sudo kubeadm token create --print-join-command")
    if ! echo "$JOIN_TOKEN" | grep -q 'kubeadm join'; then
        echo "ERREUR : impossible de générer le token de join" >&2
        exit 1
    fi
    echo "Token généré."

    echo "9. Ajout des workers au cluster..."
    for worker in "${WORKERS[@]}"; do
        join_node_to_cluster "$worker"
    done
fi

# 10. Addons optionnels
if [ "$WITH_ADDONS" = true ]; then
    echo "10. Addons (helm, local-path-provisioner, ingress-nginx)..."
    ssh "$(ssh_target "$WORKING_NODE")" "command -v helm > /dev/null 2>&1 || (curl -fsSL -o get_helm.sh https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 && chmod 700 get_helm.sh && ./get_helm.sh && rm get_helm.sh)"
    run_remote "$WORKING_NODE" "kubectl apply -f https://raw.githubusercontent.com/rancher/local-path-provisioner/master/deploy/local-path-storage.yaml"
    run_remote "$WORKING_NODE" "kubectl patch storageclass local-path -p '{\"metadata\": {\"annotations\":{\"storageclass.kubernetes.io/is-default-class\":\"true\"}}}'"
    ssh "$(ssh_target "$WORKING_NODE")" "helm repo add ingress-nginx https://kubernetes.github.io/ingress-nginx 2>/dev/null || true"
    ssh "$(ssh_target "$WORKING_NODE")" "helm repo update"
    ssh "$(ssh_target "$WORKING_NODE")" "helm upgrade --install ingress-nginx ingress-nginx/ingress-nginx --namespace ingress-nginx --create-namespace --set controller.service.type=NodePort --wait"
fi

# 11. Cron de renouvellement du token (relance après expiration 24h)
if [ "$INSTALL_CRON" = true ]; then
    echo "11. Installation du cron de renouvellement sur le master..."
    scp "$SCRIPT_DIR/k8lens-renew.sh" "$(ssh_target "$MASTER"):/tmp/k8lens-renew.sh"
    run_remote "$MASTER" "sed -i 's/\r$//' /tmp/k8lens-renew.sh && sudo install -m 755 /tmp/k8lens-renew.sh /usr/local/bin/k8lens-renew.sh && rm /tmp/k8lens-renew.sh"
    run_remote "$MASTER" "echo '0 */12 * * * root /usr/local/bin/k8lens-renew.sh' | sudo tee /etc/cron.d/k8lens && sudo chmod 644 /etc/cron.d/k8lens"
    run_remote "$MASTER" "sudo /usr/local/bin/k8lens-renew.sh"
    echo "Cron installé : le token de join est régénéré toutes les 12h (TTL 24h)."
    echo "Commande de join toujours à jour dans /var/lib/k8lens/join-command sur le master."
fi

# 12. Export du kubeconfig pour Freelens
echo "12. Export du kubeconfig pour Freelens..."
mkdir -p "$(dirname "$EXPORT_KUBECONFIG")"
ssh "$(ssh_target "$MASTER")" "sudo cat /etc/kubernetes/admin.conf" \
    | sed "s|server: https://.*:6443|server: https://$MASTER_IP:6443|" \
    | sed "s/kubernetes-admin@kubernetes/$CLUSTER_NAME/" \
    | sed "s/name: kubernetes\$/name: $CLUSTER_NAME/" \
    > "$EXPORT_KUBECONFIG"
chmod 600 "$EXPORT_KUBECONFIG"

echo ""
echo "=== CLUSTER OPÉRATIONNEL ==="
run_remote "$WORKING_NODE" "kubectl get nodes -o wide"
echo ""
echo "Kubeconfig exporté : $EXPORT_KUBECONFIG"
echo ""
echo "Pilotage avec Freelens (https://github.com/freelensapp/freelens) :"
echo "  1. Ouvrir Freelens"
echo "  2. Catalog → Clusters → + → 'Sync kubeconfig file(s)'"
echo "  3. Sélectionner : $EXPORT_KUBECONFIG"
echo "  (ou copier son contenu dans ~/.kube/config, Freelens le détecte automatiquement)"
