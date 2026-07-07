#!/usr/bin/env bash
# build-local.sh — Build local des binaires via Docker (image golang), sans
# consommer de crédits GitHub Actions. À lancer AVANT de pousser sur liv-win /
# liv-lin : si ça passe ici, la CI passera.
#
# Produit : dist/k8lens (linux/amd64) et dist/k8lens.exe (windows/amd64)
# Usage   : ./build-local.sh
set -e

cd "$(dirname "$0")"

echo "=== Vérification syntaxe bash ==="
bash -n k8lens.sh
bash -n k8lens-renew.sh

echo "=== Build Docker (golang:1.24, linux + windows) ==="
# MSYS_NO_PATHCONV : empêche Git Bash de réécrire les chemins /src
MSYS_NO_PATHCONV=1 docker run --rm \
    -v "$(pwd -W 2>/dev/null || pwd):/src" -w /src -e CGO_ENABLED=0 \
    golang:1.24 bash -c "
set -e
GOOS=linux  GOARCH=amd64 go build -trimpath -ldflags '-s -w' -o dist/k8lens .
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags '-s -w' -o dist/k8lens.exe .
echo '--- test de fumée linux ---'
./dist/k8lens --help > /dev/null && echo 'linux OK'
"

# Test de fumée du .exe si on est sous Windows (Git Bash)
if [ "$(uname -o 2>/dev/null)" = "Msys" ]; then
    echo "=== Test de fumée windows ==="
    ./dist/k8lens.exe --help > /dev/null && echo "windows OK"
fi

echo ""
ls -lh dist/
echo ""
echo "Build local OK. Pour livrer via la CI :"
echo "  git push github main:liv-win   # artefact k8lens.exe"
echo "  git push github main:liv-lin   # artefact k8lens"
