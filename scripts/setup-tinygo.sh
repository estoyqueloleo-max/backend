#!/bin/bash
set -e

TINYGO_VERSION="0.40.1"
OS="linux"
ARCH="amd64" # x86_64 is amd64 for TinyGo releases

INSTALL_DIR="_tinygo"

if [ -d "$INSTALL_DIR" ]; then
    echo "TinyGo already installed in $INSTALL_DIR"
    exit 0
fi

FILENAME="tinygo${TINYGO_VERSION}.${OS}-${ARCH}.tar.gz"
URL="https://github.com/tinygo-org/tinygo/releases/download/v${TINYGO_VERSION}/${FILENAME}"

echo "Downloading TinyGo $TINYGO_VERSION..."
curl -L -o "$FILENAME" "$URL"

echo "Extracting TinyGo..."
mkdir -p "$INSTALL_DIR"
tar -xzf "$FILENAME" -C "$INSTALL_DIR" --strip-components=1

rm "$FILENAME"

echo "TinyGo installed successfully in $(pwd)/$INSTALL_DIR"
echo "Binary location: $(pwd)/$INSTALL_DIR/bin/tinygo"
