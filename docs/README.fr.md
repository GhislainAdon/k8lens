# k8lens

Création d'un cluster Kubernetes **kubeadm** — mono-nœud ou **1 master + N workers** —
entièrement paramétrable depuis la console, puis pilotage avec
**[Freelens](https://github.com/freelensapp/freelens)**.

Dérivé du script `cloud-ictneo/script.sh` (migration MicroK8s → kubeadm), généralisé :

- plus aucune variable codée en dur : cibles, version K8s, CIDR, utilisateur SSH → **arguments CLI** (ou saisie interactive)
- support **Ubuntu/Debian** (apt + ufw) **et** **Rocky / CentOS / AlmaLinux** (dnf + EPEL + firewalld + SELinux permissif)
- **cron de renouvellement** du token de join (expire après 24h) — un worker peut rejoindre le cluster à tout moment
- export automatique d'un **kubeconfig prêt pour Freelens**

> **Répartition des rôles** : la *création* des nœuds (master/workers) est faite par
> `k8lens.sh` en SSH — Freelens ne crée pas de nœuds. Freelens sert au **pilotage**
> du cluster une fois créé : vue nodes/pods/deployments, logs, shell dans les pods,
> métriques, édition des ressources, drain/cordon des nœuds, etc.

## Prérequis

- **Accès SSH passwordless** (par clé) depuis la machine d'installation vers le master
  **et** tous les workers — vérifié automatiquement au démarrage (préflight), le script
  s'arrête avec un message clair si une cible demande un mot de passe
  (`ssh-copy-id user@cible` pour déployer la clé)
- `sudo` sur les cibles (NOPASSWD recommandé pour un déroulé sans interruption)
- Cibles : Ubuntu 22.04+/Debian 12+, ou Rocky/CentOS Stream/AlmaLinux 9+ (EPEL disponible)
- Sur le poste : bash (Git Bash / WSL sous Windows), `ssh`, `scp`

## Usage

```bash
# Cluster mono-nœud (taint control-plane retiré automatiquement)
./k8lens.sh --single --master 192.168.1.50

# 1 master + 2 workers, version K8s choisie
./k8lens.sh --master srv-master --worker srv-w1 --worker srv-w2 --k8s-version 1.34

# Cibles Rocky/CentOS avec utilisateur SSH explicite + cron de renouvellement
./k8lens.sh --master 10.0.0.10 --worker 10.0.0.11 --ssh-user rocky --install-cron

# Sans argument : les cibles sont demandées en console
./k8lens.sh
```

Le script est **idempotent** : relançable à tout moment, les nœuds déjà membres du
cluster sont détectés et ignorés. L'OS de chaque cible est **auto-détecté**
(`/etc/os-release`) — on peut mélanger Ubuntu et Rocky dans le même cluster.

### Options

| Option | Défaut | Description |
|---|---|---|
| `--master HOTE` | *(demandé)* | Hôte du control-plane |
| `--worker HOTE` | — | Worker, répétable (`--worker w1 --worker w2 …`) |
| `--single` | — | Cluster mono-nœud |
| `--k8s-version X.Y` | `1.34` | Version Kubernetes (dépôts pkgs.k8s.io) |
| `--calico-version vX.Y.Z` | `v3.27.0` | Version du CNI Calico |
| `--pod-cidr CIDR` | `192.168.0.0/16` | CIDR des pods |
| `--ssh-user USER` | — | Utilisateur SSH sur les cibles |
| `--cluster-name NOM` | `k8lens` | Nom du contexte dans le kubeconfig exporté |
| `--export-kubeconfig F` | `~/.kube/<nom>.kubeconfig` | Chemin local du kubeconfig |
| `--install-cron` | — | Pose le cron de renouvellement du token sur le master |
| `--with-addons` | — | helm + local-path-provisioner + ingress-nginx |

## Ce que fait le script

1. Détecte l'OS de chaque cible (`ubuntu` ou famille RedHat)
2. Ouvre le pare-feu entre les nœuds (ufw ou firewalld) + port 6443 pour Freelens
3. Prépare chaque nœud : containerd (SystemdCgroup), swap off, modules kernel,
   sysctl, dépôt Kubernetes officiel versionné, `kubelet kubeadm kubectl` gelés
   - **Ubuntu** : apt + keyring pkgs.k8s.io deb
   - **Rocky/CentOS** : `dnf install epel-release` → containerd EPEL, repo
     pkgs.k8s.io rpm avec `exclude=` + `--disableexcludes=kubernetes`, SELinux permissif
4. `kubeadm init` (uniquement si aucun cluster n'existe) + Calico
5. Join des workers avec un token fraîchement généré
6. Exporte le kubeconfig localement, serveur réécrit sur l'IP du master

## Pilotage avec Freelens

Le kubeconfig exporté (`~/.kube/<cluster-name>.kubeconfig` par défaut) s'importe
directement :

1. Ouvrir **Freelens** ([installation](https://github.com/freelensapp/freelens#installation) :
   `winget install Freelensapp.Freelens` / `brew install --cask freelens` / AppImage-deb-rpm)
2. **Catalog → Clusters → + → Sync kubeconfig file(s)**
3. Sélectionner le fichier exporté — le cluster apparaît et se pilote depuis l'UI
   (nodes, pods, logs, shell, métriques…)

Alternative : fusionner dans le kubeconfig par défaut, Freelens le lit automatiquement :

```bash
KUBECONFIG=~/.kube/config:~/.kube/k8lens.kubeconfig kubectl config view --flatten > ~/.kube/config.new
mv ~/.kube/config.new ~/.kube/config
```

## Cron de renouvellement (expiration du token)

Le token de join kubeadm **expire après 24h**. Avec `--install-cron`, le script pose
sur le master :

- `/usr/local/bin/k8lens-renew.sh` — régénère un token TTL 24h + la certificate-key
  control-plane (TTL 2h), et purge les tokens expirés
- `/etc/cron.d/k8lens` — exécution **toutes les 12h**

La commande de join est donc toujours valide dans :

```
/var/lib/k8lens/join-command                 # pour un worker
/var/lib/k8lens/join-command-control-plane   # pour un control-plane supplémentaire
```

Ajouter un worker plus tard = préparer le nœud (relancer `k8lens.sh` avec le nouveau
`--worker`), ou manuellement :

```bash
ssh master "sudo cat /var/lib/k8lens/join-command" | ssh nouveau-worker "sudo bash"
```

## CI — binaires de livraison

Deux branches de livraison déclenchent un build GitHub Actions :

| Branche | Workflow | Artefact produit |
|---|---|---|
| `liv-win` | `build-windows` | `k8lens.exe` (windows/amd64) |
| `liv-lin` | `build-linux` | `k8lens` (linux/amd64) |

```bash
git push github main:liv-win   # déclenche le build Windows
git push github main:liv-lin   # déclenche le build Linux
```

**Build local d'abord** (Docker, zéro crédit CI consommé) — même compilation que la CI,
plus un test de fumée du `.exe` si lancé depuis Git Bash :

```bash
./build-local.sh      # → dist/k8lens + dist/k8lens.exe
```

L'artefact se télécharge depuis l'onglet **Actions** du repo (ou `gh run download`).

Le binaire est **100 % natif** (Go) : client SSH intégré (`golang.org/x/crypto/ssh`),
**aucune dépendance à Git Bash, WSL, ssh.exe ou scp**. Il supporte :

- les clés `~/.ssh/id_ed25519`, `id_rsa`, `id_ecdsa` et l'**agent SSH**
  (OpenSSH Windows via named pipe inclus)
- les **alias de `~/.ssh/config`** (HostName, User, Port, IdentityFile)
- `known_hosts` en mode *accept-new* (hôte inconnu ajouté, clé changée = erreur)

Mêmes options que le script bash :

```bash
k8lens.exe --master 10.0.0.10 --worker 10.0.0.11 --install-cron
```

## Fichiers

| Fichier | Rôle |
|---|---|
| `main.go` / `ssh.go` / `steps.go` | CLI native Go : SSH intégré, provisioning complet |
| `k8lens.sh` | Version script bash (alternative autonome, mêmes options) |
| `k8lens-renew.sh` | Renouvellement du token — déployé sur le master, lancé par cron |
| `build-local.sh` | Build Docker local (vet + linux + windows) sans consommer la CI |
| `.github/workflows/build-{win,lin}.yml` | Pipelines de livraison `liv-win` / `liv-lin` |
