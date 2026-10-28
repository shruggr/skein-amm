#!/usr/bin/env bash
# apps/config/www/, the config app's pages (0.17.0: the config instance serves
# them), as committed: the AMM pages built from web/ui (which empties it
# first), then the 1Color pages copied into apps/config/www/color/.
# Needs Node 22+ and npm, and the Rúnar and 1sat-sdk checkouts web/ui/README.md names.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
(cd web/engine && npm ci)
(cd web/ui && npm ci && npm run build)
scripts/color-pages.sh
