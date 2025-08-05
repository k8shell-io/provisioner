#!/bin/bash
curl -X POST \
  -H "Authorization: Bearer 4343afe34e324093253465464523342343242132112" \
  -H "Content-Type: text/yaml" \
  --data-binary @k8shellfile.yaml \
  "http://localhost:9201/api/v1/blueprints/compose?username=$1"
