#!bin/bash

set -e

export KOTS_DIR=/nfs/.kots
export MINIO_KEYS_SHA_FILE=$KOTS_DIR/minio-keys-sha.txt

if [ "$#" -eq 0 ]; then
    echo "Keys SHA argument missing"
    exit 0
fi

if [ ! -d "$KOTS_DIR" ]; then
  mkdir -p -m 777 "$KOTS_DIR"
fi

echo "$1" > "$MINIO_KEYS_SHA_FILE"