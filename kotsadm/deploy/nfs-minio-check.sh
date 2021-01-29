#!bin/bash

set -e

export MINIO_CONFIG_PATH=/nfs/.minio.sys/config
export MINIO_KEYS_SHA_FILE=/nfs/.kots/minio-keys-sha.txt

if [ ! -d "$MINIO_CONFIG_PATH" ]; then
  echo '{"hasMinioConfig": false}'
  exit 0
fi

if [ ! -f "$MINIO_KEYS_SHA_FILE" ]; then
  echo '{"hasMinioConfig": true}'
  exit 0
fi

export MINIO_KEYS_SHA=$(cat $MINIO_KEYS_SHA_FILE)
echo '{"hasMinioConfig": true, "minioKeysSHA":"'"$MINIO_KEYS_SHA"'"}'
