#!/usr/bin/env bash
# Compila o FlashGerman para Windows, macOS e Linux e empacota em dist/.
# Uso: ./scripts/build.sh v1.0.0
# (Funciona em Linux e macOS. No macOS, também gera o ícone .icns e assina o .app de forma ad-hoc.)
set -euo pipefail

VERSION="${1:-dev}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIST="$ROOT/dist"
NAME="FlashGerman"
LDFLAGS="-s -w -X main.version=$VERSION"

rm -rf "$DIST"
mkdir -p "$DIST"
cd "$ROOT"

build() { # GOOS GOARCH saída [ldflags extras]
    echo "==> $1/$2"
    CGO_ENABLED=0 GOOS="$1" GOARCH="$2" go build -trimpath -ldflags "$LDFLAGS ${4:-}" -o "$3" .
}

# --- Windows: pasta portátil com o .exe (sem janela de terminal) ---
for arch in amd64 arm64; do
    label=$([ "$arch" = amd64 ] && echo x64 || echo arm64)
    dir="$DIST/stage/windows-$label/$NAME"
    mkdir -p "$dir"
    build windows "$arch" "$dir/$NAME.exe" "-H windowsgui"
    cp "$ROOT/packaging/LEIA-ME.txt" "$dir/LEIA-ME.txt"
    (cd "$DIST/stage/windows-$label" && zip -qr "$DIST/$NAME-windows-$label.zip" "$NAME")
done

# --- Linux ---
for arch in amd64 arm64; do
    label=$([ "$arch" = amd64 ] && echo x64 || echo arm64)
    dir="$DIST/stage/linux-$label/$NAME"
    mkdir -p "$dir"
    build linux "$arch" "$dir/$NAME"
    chmod +x "$dir/$NAME"
    cp "$ROOT/packaging/LEIA-ME.txt" "$dir/LEIA-ME.txt"
    tar -C "$DIST/stage/linux-$label" -czf "$DIST/$NAME-linux-$label.tar.gz" "$NAME"
done

# --- macOS: pacote .app ---
ICNS=""
if command -v iconutil >/dev/null 2>&1 && command -v sips >/dev/null 2>&1; then
    iconset="$DIST/stage/$NAME.iconset"
    mkdir -p "$iconset"
    for s in 16 32 128 256 512; do
        sips -z $s $s "$ROOT/assets/icon-1024.png" --out "$iconset/icon_${s}x${s}.png" >/dev/null
        d=$((s * 2))
        sips -z $d $d "$ROOT/assets/icon-1024.png" --out "$iconset/icon_${s}x${s}@2x.png" >/dev/null
    done
    iconutil -c icns "$iconset" -o "$DIST/stage/$NAME.icns"
    ICNS="$DIST/stage/$NAME.icns"
fi
for arch in arm64 amd64; do
    label=$([ "$arch" = arm64 ] && echo apple-silicon || echo intel)
    app="$DIST/stage/macos-$label/$NAME.app"
    mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
    build darwin "$arch" "$app/Contents/MacOS/$NAME"
    sed "s/__VERSION__/${VERSION#v}/g" "$ROOT/packaging/macos/Info.plist" > "$app/Contents/Info.plist"
    [ -n "$ICNS" ] && cp "$ICNS" "$app/Contents/Resources/$NAME.icns"
    if command -v codesign >/dev/null 2>&1; then
        codesign --force --deep -s - "$app" || true
    fi
    if command -v ditto >/dev/null 2>&1; then
        ditto -c -k --keepParent "$app" "$DIST/$NAME-macos-$label.zip"
    else
        (cd "$DIST/stage/macos-$label" && zip -qry "$DIST/$NAME-macos-$label.zip" "$NAME.app")
    fi
done

rm -rf "$DIST/stage"
cd "$DIST"
if command -v sha256sum >/dev/null 2>&1; then sha256sum ./*.zip ./*.tar.gz > SHA256SUMS.txt; else shasum -a 256 ./*.zip ./*.tar.gz > SHA256SUMS.txt; fi
echo
ls -lh "$DIST"
