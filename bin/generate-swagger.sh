#!/bin/bash

set -e

echo "Generating Swagger documentation..."

cd "$(dirname "$0")/.."

mkdir -p docs

swag init \
    -g internal/server/restapi.go \
    -o docs \
    --parseDependency \
    --parseInternal \
    --parseDepth 1

echo "Swagger documentation generated successfully in docs/ directory"
echo "View at: http://localhost:8080/swagger/index.html"