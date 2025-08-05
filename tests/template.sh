#!/bin/bash
curl -X POST \
  -H "Authorization: Bearer 4343afe34e324093253465464523342343242132112" \
  "http://localhost:9201/api/v1/workspaces/template?username=$2&blueprint=$1"
