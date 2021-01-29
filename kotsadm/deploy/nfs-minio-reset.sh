#!bin/bash

set -e

export MINIO_CONFIG_PATH=/nfs/.minio.sys/config
rm -rf "$MINIO_CONFIG_PATH"
