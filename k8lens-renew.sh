#!/usr/bin/env bash
# k8lens-renew.sh — Renouvellement du token de join kubeadm (expire après 24h).
# Installé sur le MASTER dans /usr/local/bin/ et lancé par cron toutes les 12h
# (voir /etc/cron.d/k8lens, posé par `k8lens.sh --install-cron`).
#
# Résultat : /var/lib/k8lens/join-command contient TOUJOURS une commande de join
# valide — un nouveau worker peut rejoindre le cluster à n'importe quel moment,
# même longtemps après la création du cluster.

set -u
export KUBECONFIG=/etc/kubernetes/admin.conf
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

OUT_DIR=/var/lib/k8lens
mkdir -p "$OUT_DIR"

# Nouveau token worker (TTL 24h) + commande de join complète
JOIN_CMD=$(kubeadm token create --ttl 24h --print-join-command 2>/dev/null)

if [ -n "$JOIN_CMD" ] && echo "$JOIN_CMD" | grep -q 'kubeadm join'; then
    echo "$JOIN_CMD" > "$OUT_DIR/join-command"
    chmod 600 "$OUT_DIR/join-command"
    logger -t k8lens "token de join renouvelé (TTL 24h)"
else
    logger -t k8lens "ECHEC du renouvellement du token de join"
    exit 1
fi

# Certificate-key pour joindre un control-plane supplémentaire (expire après 2h)
CERT_KEY=$(kubeadm init phase upload-certs --upload-certs 2>/dev/null | grep -oE '[a-f0-9]{64}' | tail -1)
if [ -n "$CERT_KEY" ]; then
    echo "$JOIN_CMD --control-plane --certificate-key $CERT_KEY" > "$OUT_DIR/join-command-control-plane"
    chmod 600 "$OUT_DIR/join-command-control-plane"
    logger -t k8lens "certificate-key control-plane renouvelée"
fi

# Purge des tokens expirés (hygiène)
kubeadm token list -o jsonpath='{range .items[?(@.expires=="")]}{.token}{"\n"}{end}' 2>/dev/null | while read -r t; do
    [ -n "$t" ] && kubeadm token delete "$t" >/dev/null 2>&1
done

exit 0
